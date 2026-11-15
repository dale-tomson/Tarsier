package logs

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultBlockTargetBytes is the uncompressed block size threshold (64 KiB).
	DefaultBlockTargetBytes = 65536

	// DefaultFlushInterval is the idle flush interval (1 second).
	DefaultFlushInterval = 1 * time.Second

	// DefaultMaxOpenFiles is the maximum number of concurrent open chunk file handles.
	DefaultMaxOpenFiles = 64
)

// ChunkWriterConfig configures the ChunkWriterManager.
type ChunkWriterConfig struct {
	BaseDir          string
	BlockTargetBytes int
	FlushInterval    time.Duration
	ZstdLevel        zstd.EncoderLevel
	MaxOpenFiles     int
	OnBlockFlushed   func(BlockMeta)
}

type sourceBuffer struct {
	sourceID    int64
	date        string
	rawBuf      []byte
	recordCount uint32
	minTS       int64
	maxTS       int64
	minLevel    model.Level
	maxLevel    model.Level
	streamMask  uint8
	lastActive  time.Time
}

type cachedFile struct {
	file       *os.File
	lastAccess time.Time
}

// ChunkWriterManager manages per-source 64 KiB uncompressed block buffers, compresses them with zstd,
// and appends them to /data/logs/<source_id>/<YYYY-MM-DD>.chunk files.
type ChunkWriterManager struct {
	mu        sync.Mutex
	cfg       ChunkWriterConfig
	buffers   map[int64]*sourceBuffer
	openFiles map[string]*cachedFile
	enc       *zstd.Encoder
	rawPool   *sync.Pool
	compPool  *sync.Pool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewChunkWriterManager creates and initializes a ChunkWriterManager.
func NewChunkWriterManager(cfg ChunkWriterConfig) (*ChunkWriterManager, error) {
	if cfg.BaseDir == "" {
		cfg.BaseDir = "/data/logs"
	}
	if cfg.BlockTargetBytes <= 0 {
		cfg.BlockTargetBytes = DefaultBlockTargetBytes
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.MaxOpenFiles <= 0 {
		cfg.MaxOpenFiles = DefaultMaxOpenFiles
	}
	if cfg.ZstdLevel == 0 {
		cfg.ZstdLevel = zstd.SpeedDefault
	}

	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(cfg.ZstdLevel),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}

	rawPoolCap := cfg.BlockTargetBytes + 4096
	compPoolCap := int(float64(rawPoolCap)*1.2) + 1024

	ctx, cancel := context.WithCancel(context.Background())

	m := &ChunkWriterManager{
		cfg:       cfg,
		buffers:   make(map[int64]*sourceBuffer),
		openFiles: make(map[string]*cachedFile),
		enc:       enc,
		rawPool: &sync.Pool{
			New: func() any {
				b := make([]byte, 0, rawPoolCap)
				return &b
			},
		},
		compPool: &sync.Pool{
			New: func() any {
				b := make([]byte, compPoolCap)
				return &b
			},
		},
		ctx:    ctx,
		cancel: cancel,
	}

	// Start background idle buffer flusher
	m.wg.Add(1)
	go m.runFlusher()

	return m, nil
}

// runFlusher periodically flushes buffers that have been idle for >= FlushInterval.
func (m *ChunkWriterManager) runFlusher() {
	defer m.wg.Done()

	interval := m.cfg.FlushInterval / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.flushIdleBuffers()
		}
	}
}

func (m *ChunkWriterManager) flushIdleBuffers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, buf := range m.buffers {
		if buf.recordCount > 0 && now.Sub(buf.lastActive) >= m.cfg.FlushInterval {
			_ = m.flushBufferLocked(buf)
		}
	}
}

// Write routes a parsed LogEntry to the active source buffer, flushing when size, date rollover,
// or shutdown boundaries occur.
func (m *ChunkWriterManager) Write(entry model.LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sourceID := parseSourceID(entry.SourceID)

	ts := entry.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
		entry.Timestamp = ts
	} else {
		ts = ts.UTC()
		entry.Timestamp = ts
	}

	entryTS := ts.UnixNano()
	entryDate := ts.Format("2006-01-02")

	buf, exists := m.buffers[sourceID]
	recordSize := RecordHeaderSize + len(entry.Message)
	if len(entry.Message) == 0 && len(entry.Raw) > 0 {
		recordSize = RecordHeaderSize + len(entry.Raw)
	}

	// 1. Check Date Boundary Change
	if exists && buf.recordCount > 0 && buf.date != entryDate {
		if err := m.flushBufferLocked(buf); err != nil {
			return err
		}
		buf = nil
		exists = false
	}

	// 2. Check Size Threshold
	if exists && buf.recordCount > 0 && len(buf.rawBuf)+recordSize > m.cfg.BlockTargetBytes {
		if err := m.flushBufferLocked(buf); err != nil {
			return err
		}
		buf = nil
		exists = false
	}

	// 3. Initialize new buffer if needed
	if !exists || buf == nil {
		rawPtr := m.rawPool.Get().(*[]byte)
		rawBuf := (*rawPtr)[:0]
		// Reserve 16-byte block header
		if cap(rawBuf) >= BlockHeaderSize {
			rawBuf = rawBuf[:BlockHeaderSize]
		} else {
			rawBuf = make([]byte, BlockHeaderSize, m.cfg.BlockTargetBytes+4096)
		}

		buf = &sourceBuffer{
			sourceID:    sourceID,
			date:        entryDate,
			rawBuf:      rawBuf,
			recordCount: 0,
			minTS:       entryTS,
			maxTS:       entryTS,
			minLevel:    entry.Level,
			maxLevel:    entry.Level,
			streamMask:  1 << uint8(entry.Stream),
			lastActive:  time.Now(),
		}
		m.buffers[sourceID] = buf
	} else {
		// Update running stats for existing buffer
		if entryTS < buf.minTS {
			buf.minTS = entryTS
		}
		if entryTS > buf.maxTS {
			buf.maxTS = entryTS
		}
		if entry.Level < buf.minLevel {
			buf.minLevel = entry.Level
		}
		if entry.Level > buf.maxLevel {
			buf.maxLevel = entry.Level
		}
		buf.streamMask |= 1 << uint8(entry.Stream)
	}

	// 4. Encode record into buffer
	buf.rawBuf = EncodeRecord(buf.rawBuf, entry)
	buf.recordCount++
	buf.lastActive = time.Now()

	return nil
}

// flushBufferLocked finalizes the block header, compresses via zstd, writes to disk,
// invokes OnBlockFlushed, and recycles memory buffers (caller must hold m.mu).
func (m *ChunkWriterManager) flushBufferLocked(buf *sourceBuffer) error {
	if buf == nil || buf.recordCount == 0 {
		return nil
	}

	// 1. Finalize uncompressed block header
	uncompressedLen := uint32(len(buf.rawBuf))
	EncodeBlockHeader(buf.rawBuf[:BlockHeaderSize], buf.recordCount, uncompressedLen)

	// 2. Compress via zstd
	compPtr := m.compPool.Get().(*[]byte)
	compBuf := *compPtr
	compBytes := m.enc.EncodeAll(buf.rawBuf, compBuf[:0])

	// 3. Write to chunk file
	filePath := filepath.Join(m.cfg.BaseDir, strconv.FormatInt(buf.sourceID, 10), buf.date+".chunk")
	f, err := m.getOrCreateFileLocked(filePath)
	if err != nil {
		m.compPool.Put(compPtr)
		return fmt.Errorf("open chunk file %s: %w", filePath, err)
	}

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		m.compPool.Put(compPtr)
		return fmt.Errorf("seek chunk file %s: %w", filePath, err)
	}

	if _, wErr := f.Write(compBytes); wErr != nil {
		m.compPool.Put(compPtr)
		return fmt.Errorf("write chunk file %s: %w", filePath, wErr)
	}

	meta := BlockMeta{
		SourceID:        buf.sourceID,
		Date:            buf.date,
		MinTS:           buf.minTS,
		MaxTS:           buf.maxTS,
		MinLevel:        buf.minLevel,
		MaxLevel:        buf.maxLevel,
		StreamMask:      buf.streamMask,
		FileOffset:      offset,
		CompressedLen:   uint32(len(compBytes)),
		UncompressedLen: uncompressedLen,
		RecordCount:     buf.recordCount,
	}

	// 4. Recycle compression buffer
	m.compPool.Put(compPtr)

	// 5. Recycle raw buffer and reset source buffer
	rawSlice := buf.rawBuf[:0]
	m.rawPool.Put(&rawSlice)
	delete(m.buffers, buf.sourceID)

	// 6. Invoke callback if configured
	if m.cfg.OnBlockFlushed != nil {
		m.cfg.OnBlockFlushed(meta)
	}

	return nil
}

// getOrCreateFileLocked retrieves an open *os.File or opens a new one, evicting LRU files if needed.
func (m *ChunkWriterManager) getOrCreateFileLocked(filePath string) (*os.File, error) {
	if entry, exists := m.openFiles[filePath]; exists {
		entry.lastAccess = time.Now()
		return entry.file, nil
	}

	// Check eviction limit
	if len(m.openFiles) >= m.cfg.MaxOpenFiles {
		var oldestPath string
		var oldestTime time.Time
		first := true

		for p, ent := range m.openFiles {
			if first || ent.lastAccess.Before(oldestTime) {
				oldestPath = p
				oldestTime = ent.lastAccess
				first = false
			}
		}

		if oldestPath != "" {
			old := m.openFiles[oldestPath]
			_ = old.file.Sync()
			_ = old.file.Close()
			delete(m.openFiles, oldestPath)
		}
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, err
	}

	m.openFiles[filePath] = &cachedFile{
		file:       f,
		lastAccess: time.Now(),
	}

	return f, nil
}

// Flush flushes all active buffers across all sources to disk immediately.
func (m *ChunkWriterManager) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for _, buf := range m.buffers {
		if buf.recordCount > 0 {
			if err := m.flushBufferLocked(buf); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Close gracefully flushes all source buffers, syncs and closes all open files, and stops background tasks.
func (m *ChunkWriterManager) Close(ctx context.Context) error {
	m.cancel()
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error

	// Flush all pending buffers
	for _, buf := range m.buffers {
		if buf.recordCount > 0 {
			if err := m.flushBufferLocked(buf); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	m.buffers = make(map[int64]*sourceBuffer)

	// Sync and close all open files
	for path, entry := range m.openFiles {
		if syncErr := entry.file.Sync(); syncErr != nil && firstErr == nil {
			firstErr = syncErr
		}
		if closeErr := entry.file.Close(); closeErr != nil && firstErr == nil {
			firstErr = closeErr
		}
		delete(m.openFiles, path)
	}

	return firstErr
}

func parseSourceID(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return v
	}
	// Fallback to FNV hash
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
