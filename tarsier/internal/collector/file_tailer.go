package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// Default limits and intervals for file tailing.
const (
	DefaultMaxLineBytes   = 65536 // 64 KiB
	DefaultPollInterval   = 500 * time.Millisecond
	DefaultRescanInterval = 30 * time.Second
	DefaultFlushInterval  = 3 * time.Second
	InitialTailReadCap    = 65536 // 64 KiB tail window on first discovery
)

var readBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 16384)
		return &b
	},
}

// FileTailerConfig provides configuration parameters for FileTailerManager.
type FileTailerConfig struct {
	Files          []config.LogFileConfig
	ConfigDB       *db.Handle
	Sink           model.LogSink
	PollInterval   time.Duration // default 500ms
	RescanInterval time.Duration // default 30s
	FlushInterval  time.Duration // default 3s
	MaxSources     int           // default 500
	MaxLineBytes   int           // default 65536
	DefaultTZ      *time.Location
}

// fileTailer tracks and polls an individual file on disk.
type fileTailer struct {
	sourceName string
	sourceID   int64
	path       string

	file   *os.File
	inode  uint64
	offset int64
	lastTS int64
	dirty  atomic.Bool

	detector *logs.SourceDetector

	ctx    context.Context
	cancel context.CancelFunc
}

// FileTailerManager discovers, spawns, and coordinates file tailer workers.
type FileTailerManager struct {
	files          []config.LogFileConfig
	configDB       *db.Handle
	sink           model.LogSink
	pollInterval   time.Duration
	rescanInterval time.Duration
	flushInterval  time.Duration
	maxSources     int
	maxLineBytes   int
	defaultTZ      *time.Location

	droppedLines atomic.Uint64

	mu      sync.RWMutex
	tailers map[string]*fileTailer // canonical path -> tailer

	ctx    context.Context
	cancel context.CancelFunc
}

// NewFileTailerManager initializes a FileTailerManager.
func NewFileTailerManager(cfg FileTailerConfig) *FileTailerManager {
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	rescanInterval := cfg.RescanInterval
	if rescanInterval <= 0 {
		rescanInterval = DefaultRescanInterval
	}
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = DefaultFlushInterval
	}
	maxSources := cfg.MaxSources
	if maxSources <= 0 {
		maxSources = 500
	}
	maxLineBytes := cfg.MaxLineBytes
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxLineBytes
	}
	defaultTZ := cfg.DefaultTZ
	if defaultTZ == nil {
		defaultTZ = time.Local
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &FileTailerManager{
		files:          cfg.Files,
		configDB:       cfg.ConfigDB,
		sink:           cfg.Sink,
		pollInterval:   pollInterval,
		rescanInterval: rescanInterval,
		flushInterval:  flushInterval,
		maxSources:     maxSources,
		maxLineBytes:   maxLineBytes,
		defaultTZ:      defaultTZ,
		tailers:        make(map[string]*fileTailer),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// DroppedLines returns the total count of lines dropped due to a full sink.
func (m *FileTailerManager) DroppedLines() uint64 {
	return m.droppedLines.Load()
}

// Start begins periodic glob discovery and tailing workers.
func (m *FileTailerManager) Start(ctx context.Context) {
	// 1. Initial immediate discovery scan
	m.rescanFiles()

	// 2. Periodic glob rescan ticker
	go func() {
		rescanTicker := time.NewTicker(m.rescanInterval)
		defer rescanTicker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ctx.Done():
				m.Stop()
				return
			case <-rescanTicker.C:
				m.rescanFiles()
			}
		}
	}()
}

// Rescan triggers an immediate filesystem discovery scan for newly added or removed files.
func (m *FileTailerManager) Rescan() {
	m.rescanFiles()
}

// Stop terminates all file tailer workers cleanly and flushes final cursor states.
func (m *FileTailerManager) Stop() {
	m.cancel()

	m.mu.Lock()
	active := make([]*fileTailer, 0, len(m.tailers))
	for _, t := range m.tailers {
		active = append(active, t)
	}
	m.tailers = make(map[string]*fileTailer)
	m.mu.Unlock()

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()

	for _, t := range active {
		t.cancel()
		if t.file != nil {
			_ = t.file.Close()
		}
		if m.configDB != nil && t.sourceID > 0 {
			_ = SaveFileSourceOffset(flushCtx, m.configDB, t.sourceID, t.inode, t.offset, t.lastTS)
		}
	}
}

// rescanFiles discovers matching log files and updates active tailers.
func (m *FileTailerManager) rescanFiles() {
	discovered := make(map[string]string) // path -> sourceName

	for _, entry := range m.files {
		matches, err := filepath.Glob(entry.Path)
		if err != nil {
			continue
		}

		isGlob := filepath.Base(entry.Path) != entry.Path && (containsGlobMeta(entry.Path))

		for _, p := range matches {
			if len(discovered) >= m.maxSources {
				break
			}
			info, statErr := os.Stat(p)
			if statErr != nil || info.IsDir() {
				continue
			}

			sourceName := entry.Name
			if isGlob || len(matches) > 1 {
				sourceName = fmt.Sprintf("%s/%s", entry.Name, filepath.Base(p))
			}
			discovered[p] = sourceName
		}
	}

	m.mu.Lock()
	// Start tailers for new files
	for p, name := range discovered {
		if _, exists := m.tailers[p]; !exists {
			m.startTailerLocked(p, name)
		}
	}

	// Stop tailers for removed files
	for p, t := range m.tailers {
		if _, exists := discovered[p]; !exists {
			t.cancel()
			if t.file != nil {
				_ = t.file.Close()
			}
			if m.configDB != nil && t.sourceID > 0 {
				_ = SaveFileSourceOffset(context.Background(), m.configDB, t.sourceID, t.inode, t.offset, t.lastTS)
			}
			delete(m.tailers, p)
		}
	}
	m.mu.Unlock()
}

func containsGlobMeta(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' || s[i] == '?' || s[i] == '[' {
			return true
		}
	}
	return false
}

// startTailerLocked creates and launches a fileTailer (caller must hold m.mu).
func (m *FileTailerManager) startTailerLocked(path, sourceName string) {
	tailerCtx, tailerCancel := context.WithCancel(m.ctx)

	t := &fileTailer{
		sourceName: sourceName,
		path:       path,
		detector:   logs.NewSourceDetector(m.defaultTZ, 4096),
		ctx:        tailerCtx,
		cancel:     tailerCancel,
	}

	// Register / resolve source in config.db
	if m.configDB != nil {
		sid, err := EnsureSource(m.ctx, m.configDB, "file", sourceName)
		if err == nil {
			t.sourceID = sid
		}
	}

	m.tailers[path] = t
	go m.runTailer(t)
}

