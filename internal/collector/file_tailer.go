package collector

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
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

// LogSink defines an interface for receiving ingested log entries.
type LogSink interface {
	PushLog(entry model.LogEntry) bool
}

// LogChanSink adapts a chan<- model.LogEntry channel into a LogSink.
type LogChanSink chan<- model.LogEntry

// PushLog pushes a log entry without blocking; returns false if channel is full.
func (s LogChanSink) PushLog(entry model.LogEntry) bool {
	select {
	case s <- entry:
		return true
	default:
		return false
	}
}

// getInode extracts the underlying filesystem inode from os.FileInfo on Unix systems.
func getInode(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	sys := info.Sys()
	if sys == nil {
		return 0
	}
	if stat, ok := sys.(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}

// EnsureSource registers or retrieves the integer source ID for a file from config.db.
func EnsureSource(ctx context.Context, configDB *db.Handle, kind, name string) (int64, error) {
	if configDB == nil {
		return 0, errors.New("configDB handle is required")
	}
	now := time.Now().Unix()
	var id int64

	// 1. Fast read from RO pool
	err := configDB.RO.QueryRowContext(ctx, "SELECT id FROM source WHERE kind = ? AND name = ?", kind, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("query source id: %w", err)
	}

	// 2. Insert into source table via SingleWriter
	err = configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, writeErr := tx.ExecContext(ctx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES (?, ?, 1, ?, ?)
			ON CONFLICT(kind, name) DO UPDATE SET last_seen = excluded.last_seen;
		`, kind, name, now, now)
		return writeErr
	})
	if err != nil {
		return 0, fmt.Errorf("insert source: %w", err)
	}

	// 3. Read back assigned ID
	err = configDB.RO.QueryRowContext(ctx, "SELECT id FROM source WHERE kind = ? AND name = ?", kind, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("readback source id: %w", err)
	}
	return id, nil
}

// GetFileSourceOffset loads the persisted (inode, offset, last_ts) cursor for a source from config.db.
func GetFileSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64) (inode uint64, offset int64, lastTS int64, err error) {
	if configDB == nil {
		return 0, 0, 0, errors.New("configDB handle is required")
	}
	var inVal, offVal, tsVal int64
	qErr := configDB.RO.QueryRowContext(ctx, "SELECT inode, offset, last_ts FROM source_offset WHERE source_id = ?", sourceID).Scan(&inVal, &offVal, &tsVal)
	if errors.Is(qErr, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if qErr != nil {
		return 0, 0, 0, fmt.Errorf("query file source offset: %w", qErr)
	}
	return uint64(inVal), offVal, tsVal, nil
}

// SaveFileSourceOffset persists the (inode, offset, last_ts) cursor for a source in config.db.
func SaveFileSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64, inode uint64, offset int64, lastTS int64) error {
	if configDB == nil {
		return errors.New("configDB handle is required")
	}
	return configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO source_offset (source_id, inode, offset, last_ts)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(source_id) DO UPDATE SET
				inode   = excluded.inode,
				offset  = excluded.offset,
				last_ts = excluded.last_ts;
		`, sourceID, int64(inode), offset, lastTS)
		return err
	})
}

// FileTailerConfig provides configuration parameters for FileTailerManager.
type FileTailerConfig struct {
	Files          []config.LogFileConfig
	ConfigDB       *db.Handle
	Sink           LogSink
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
	sink           LogSink
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

// runTailer manages polling, rotation, truncation, and line processing for a single file.
func (m *FileTailerManager) runTailer(t *fileTailer) {
	// 1. Initial open and position calculation
	if err := m.initTailerFile(t); err != nil {
		// If initial open fails, retry on polling ticks
	}
	defer func() {
		if t.file != nil {
			_ = t.file.Close()
		}
		if m.configDB != nil && t.sourceID > 0 {
			_ = SaveFileSourceOffset(context.Background(), m.configDB, t.sourceID, t.inode, t.offset, t.lastTS)
		}
	}()

	pollTicker := time.NewTicker(m.pollInterval)
	defer pollTicker.Stop()

	flushTicker := time.NewTicker(m.flushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-flushTicker.C:
			if t.dirty.CompareAndSwap(true, false) {
				if m.configDB != nil && t.sourceID > 0 {
					_ = SaveFileSourceOffset(context.Background(), m.configDB, t.sourceID, t.inode, t.offset, t.lastTS)
				}
			}
		case <-pollTicker.C:
			m.pollFile(t)
		}
	}
}

// initTailerFile resolves initial file position respecting persisted cursor or 64 KiB tail.
func (m *FileTailerManager) initTailerFile(t *fileTailer) error {
	info, err := os.Stat(t.path)
	if err != nil {
		return err
	}

	currInode := getInode(info)
	currSize := info.Size()

	var persistedInode uint64
	var persistedOffset int64
	var persistedTS int64

	if m.configDB != nil && t.sourceID > 0 {
		pIno, pOff, pTS, qErr := GetFileSourceOffset(t.ctx, m.configDB, t.sourceID)
		if qErr == nil {
			persistedInode = pIno
			persistedOffset = pOff
			persistedTS = pTS
			t.lastTS = persistedTS
		}
	}

	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	t.file = f
	t.inode = currInode

	var targetOffset int64
	if persistedInode > 0 && persistedInode == currInode && currSize >= persistedOffset {
		// Valid resume from exact offset
		targetOffset = persistedOffset
	} else if persistedInode == 0 {
		// Brand-new file: tail last 64 KiB, discarding partial leading line
		if currSize > InitialTailReadCap {
			startTail := currSize - InitialTailReadCap
			foundNL := false
			if _, seekErr := f.Seek(startTail, io.SeekStart); seekErr == nil {
				buf := make([]byte, 1024)
				var discarded int64
				for discarded < InitialTailReadCap {
					n, rErr := f.Read(buf)
					if n > 0 {
						idx := bytes.IndexByte(buf[:n], '\n')
						if idx >= 0 {
							targetOffset = startTail + discarded + int64(idx+1)
							foundNL = true
							break
						}
						discarded += int64(n)
					}
					if rErr != nil {
						break
					}
				}
			}
			if !foundNL {
				targetOffset = startTail
			}
		} else {
			targetOffset = 0
		}
	} else {
		// Inode changed or file truncated while offline
		targetOffset = 0
	}

	t.offset = targetOffset
	_, _ = f.Seek(t.offset, io.SeekStart)
	return nil
}

// pollFile inspects file stat, detects rotation/truncation, and reads available bytes.
func (m *FileTailerManager) pollFile(t *fileTailer) {
	info, err := os.Stat(t.path)
	if err != nil {
		// File temporarily missing
		return
	}

	diskInode := getInode(info)
	diskSize := info.Size()

	// If file was not open, open it
	if t.file == nil {
		f, openErr := os.Open(t.path)
		if openErr != nil {
			return
		}
		t.file = f
		t.inode = diskInode
		t.offset = 0
		_, _ = f.Seek(0, io.SeekStart)
	}

	// Case 1 — Inode Changed (e.g. logrotate rename + create)
	if diskInode != 0 && t.inode != 0 && diskInode != t.inode {
		// 1. Drain all remaining bytes from the old file handle
		m.drainFileUntilEOF(t)
		_ = t.file.Close()

		// 2. Open new file
		f, openErr := os.Open(t.path)
		if openErr != nil {
			t.file = nil
			return
		}
		t.file = f
		t.inode = diskInode
		t.offset = 0
		_, _ = f.Seek(0, io.SeekStart)
	}

	// Case 2 — File Truncated (e.g. copytruncate or : > file.log)
	if diskSize < t.offset {
		t.offset = 0
		_, _ = t.file.Seek(0, io.SeekStart)
	}

	// Case 3 — Read new bytes
	m.readAvailableLines(t)
}

// drainFileUntilEOF reads any final unread bytes from old file before closing it on rotation.
func (m *FileTailerManager) drainFileUntilEOF(t *fileTailer) {
	if t.file == nil {
		return
	}
	m.readAvailableLines(t)
}

// readAvailableLines reads lines between t.offset and EOF, advancing offset only per complete line.
func (m *FileTailerManager) readAvailableLines(t *fileTailer) {
	if t.file == nil {
		return
	}

	bufPtr := readBufPool.Get().(*[]byte)
	readBuf := *bufPtr
	defer readBufPool.Put(bufPtr)

	var lineAccum []byte

	for {
		if t.ctx.Err() != nil {
			return
		}

		n, err := t.file.Read(readBuf)
		if n > 0 {
			chunk := readBuf[:n]
			for len(chunk) > 0 {
				idx := bytes.IndexByte(chunk, '\n')
				if idx >= 0 {
					// Complete line found
					linePart := chunk[:idx]
					chunk = chunk[idx+1:]

					lineAccum = append(lineAccum, linePart...)
					lineBytes := stripCR(lineAccum)
					consumed := len(lineAccum) + 1
					lineAccum = lineAccum[:0]

					// Advance offset by exact line bytes + 1 for '\n'
					t.offset += int64(consumed)
					t.dirty.Store(true)

					m.processFileLine(t, lineBytes)
				} else {
					// No newline in remaining chunk
					if len(lineAccum)+len(chunk) >= m.maxLineBytes {
						// Hard 64 KiB cap reached without '\n': flush 64 KiB chunk immediately
						rem := m.maxLineBytes - len(lineAccum)
						if rem > 0 {
							lineAccum = append(lineAccum, chunk[:rem]...)
							chunk = chunk[rem:]
						}
						lineBytes := stripCR(lineAccum)
						lineAccum = lineAccum[:0]

						t.offset += int64(m.maxLineBytes)
						t.dirty.Store(true)

						m.processFileLine(t, lineBytes)
					} else {
						lineAccum = append(lineAccum, chunk...)
						chunk = nil
					}
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Reached EOF
				// Partial line safety: if incomplete trailing bytes remain without '\n',
				// do NOT advance offset! Seek back to t.offset so next tick re-reads complete line.
				if len(lineAccum) > 0 {
					_, _ = t.file.Seek(t.offset, io.SeekStart)
				}
				break
			}
			break
		}
	}
}

func stripCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// processFileLine parses timestamps, detects level, and emits model.LogEntry to sink.
func (m *FileTailerManager) processFileLine(t *fileTailer, lineBytes []byte) {
	if len(lineBytes) == 0 {
		return
	}

	nowNS := time.Now().UTC().UnixNano()

	tsRes := t.detector.DetectTimestamp(logs.DetectInput{
		Line:       lineBytes,
		ReadTimeNS: nowNS,
		IsDocker:   false,
	})

	lvlRes := t.detector.DetectLevel(lineBytes, tsRes.TS, false)

	t.lastTS = tsRes.TS
	t.dirty.Store(true)

	// Make copy of lineBytes for safe concurrency handoff
	msgCopy := make([]byte, len(lineBytes))
	copy(msgCopy, lineBytes)

	entry := model.LogEntry{
		Timestamp:       time.Unix(0, tsRes.TS).UTC(),
		TimestampSource: tsRes.TSSrc,
		SourceID:        fmt.Sprintf("%d", t.sourceID),
		Level:           lvlRes.Level,
		LevelSource:     lvlRes.LevelSrc,
		Stream:          model.StreamNA,
		Message:         msgCopy,
		Raw:             msgCopy,
	}

	if m.sink != nil {
		if !m.sink.PushLog(entry) {
			m.droppedLines.Add(1)
		}
	}
}
