package docker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// DefaultMaxLineBytes is the hard cap for a single assembled log line (64 KiB).
const DefaultMaxLineBytes = 65536

// bufferPool pools 16 KiB byte slices for stream reads to minimize heap allocations.
var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 16384)
		return &b
	},
}

// EnsureSource registers or retrieves the integer source ID for a container from config.db.
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

// GetSourceOffset returns the last ingested timestamp (in nanoseconds) recorded for a source.
func GetSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64) (int64, error) {
	if configDB == nil {
		return 0, errors.New("configDB handle is required")
	}
	var lastTS int64
	err := configDB.RO.QueryRowContext(ctx, "SELECT last_ts FROM source_offset WHERE source_id = ?", sourceID).Scan(&lastTS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query source offset: %w", err)
	}
	return lastTS, nil
}

// SaveSourceOffset records the last seen timestamp (in nanoseconds) for a source in config.db.
func SaveSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64, lastTS int64) error {
	if configDB == nil {
		return errors.New("configDB handle is required")
	}
	return configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO source_offset (source_id, inode, offset, last_ts)
			VALUES (?, 0, 0, ?)
			ON CONFLICT(source_id) DO UPDATE SET last_ts = excluded.last_ts;
		`, sourceID, lastTS)
		return err
	})
}

// lineAssembler accumulates stream bytes and breaks them into newline-delimited chunks
// capped at maxLineBytes.
type lineAssembler struct {
	buf []byte
	max int
}

func newLineAssembler(max int) *lineAssembler {
	if max <= 0 {
		max = DefaultMaxLineBytes
	}
	return &lineAssembler{
		buf: make([]byte, 0, 1024),
		max: max,
	}
}

func (a *lineAssembler) feed(chunk []byte, onLine func(line []byte) error) error {
	for len(chunk) > 0 {
		idx := bytes.IndexByte(chunk, '\n')
		if idx >= 0 {
			// Found newline
			part := chunk[:idx]
			chunk = chunk[idx+1:]

			if len(a.buf)+len(part) > a.max {
				// Line exceeds maximum cap: flush current buffer as a segment
				rem := a.max - len(a.buf)
				if rem > 0 {
					a.buf = append(a.buf, part[:rem]...)
					part = part[rem:]
				}
				if err := a.flush(onLine); err != nil {
					return err
				}
				// If remaining part still exceeds max, flush chunks
				for len(part) >= a.max {
					if err := onLine(stripCR(part[:a.max])); err != nil {
						return err
					}
					part = part[a.max:]
				}
			}

			a.buf = append(a.buf, part...)
			if err := a.flush(onLine); err != nil {
				return err
			}
		} else {
			// No newline in remaining chunk
			if len(a.buf)+len(chunk) >= a.max {
				rem := a.max - len(a.buf)
				if rem > 0 {
					a.buf = append(a.buf, chunk[:rem]...)
					chunk = chunk[rem:]
				}
				if err := a.flush(onLine); err != nil {
					return err
				}
				for len(chunk) >= a.max {
					if err := onLine(stripCR(chunk[:a.max])); err != nil {
						return err
					}
					chunk = chunk[a.max:]
				}
			}
			a.buf = append(a.buf, chunk...)
			break
		}
	}
	return nil
}

func (a *lineAssembler) flush(onLine func(line []byte) error) error {
	if len(a.buf) == 0 {
		return nil
	}
	clean := stripCR(a.buf)
	a.buf = a.buf[:0]
	return onLine(clean)
}

func stripCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// DemuxStream demultiplexes a Docker log byte stream (TTY or non-TTY 8-byte frames)
// and emits clean, assembled lines to the callback.
func DemuxStream(r io.Reader, isTTY bool, maxLineBytes int, emit func(stream model.Stream, line []byte) error) error {
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxLineBytes
	}

	bufPtr := bufferPool.Get().(*[]byte)
	readBuf := *bufPtr
	defer bufferPool.Put(bufPtr)

	if isTTY {
		// TTY raw stream: no 8-byte headers, all lines are stdout
		stdoutAssembler := newLineAssembler(maxLineBytes)
		for {
			n, err := r.Read(readBuf)
			if n > 0 {
				feedErr := stdoutAssembler.feed(readBuf[:n], func(line []byte) error {
					return emit(model.StreamStdout, line)
				})
				if feedErr != nil {
					return feedErr
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
		}
		return stdoutAssembler.flush(func(line []byte) error {
			return emit(model.StreamStdout, line)
		})
	}

	// Non-TTY multiplexed frames: 8-byte header per frame
	stdoutAssembler := newLineAssembler(maxLineBytes)
	stderrAssembler := newLineAssembler(maxLineBytes)

	var header [8]byte
	for {
		_, err := io.ReadFull(r, header[:])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return err
		}

		streamType := header[0]
		payloadLen := binary.BigEndian.Uint32(header[4:8])

		// Guard against corrupt / giant frame header
		if payloadLen > 16<<20 { // 16 MiB
			return fmt.Errorf("corrupt docker log frame: payload length %d exceeds 16 MiB", payloadLen)
		}

		var targetAssembler *lineAssembler
		var stream model.Stream
		switch streamType {
		case 1:
			targetAssembler = stdoutAssembler
			stream = model.StreamStdout
		case 2:
			targetAssembler = stderrAssembler
			stream = model.StreamStderr
		default:
			// Ingest 0 (stdin) or other stream: read and discard payload
			if _, discardErr := io.CopyN(io.Discard, r, int64(payloadLen)); discardErr != nil {
				return discardErr
			}
			continue
		}

		// Stream exactly payloadLen bytes into targetAssembler
		var remaining = int64(payloadLen)
		for remaining > 0 {
			toRead := int64(len(readBuf))
			if toRead > remaining {
				toRead = remaining
			}
			n, readErr := r.Read(readBuf[:toRead])
			if n > 0 {
				feedErr := targetAssembler.feed(readBuf[:n], func(line []byte) error {
					return emit(stream, line)
				})
				if feedErr != nil {
					return feedErr
				}
				remaining -= int64(n)
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) && remaining == 0 {
					break
				}
				return readErr
			}
		}
	}

	// Flush any trailing line fragments
	if err := stdoutAssembler.flush(func(line []byte) error {
		return emit(model.StreamStdout, line)
	}); err != nil {
		return err
	}
	return stderrAssembler.flush(func(line []byte) error {
		return emit(model.StreamStderr, line)
	})
}

// containerFollower manages log streaming, parsing, and cursor tracking for a single container.
type containerFollower struct {
	info     ContainerInfo
	sourceID int64
	detector *logs.SourceDetector

	lastSeenTS   int64
	hasNewOffset atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// LogManagerConfig provides configuration settings for DockerLogManager.
type LogManagerConfig struct {
	Client        *Client
	Filter        *ContainerFilter
	ConfigDB      *db.Handle
	Sink          model.LogSink
	FlushInterval time.Duration // default 3s
	MaxLineBytes  int           // default 65536
}

// DockerLogManager coordinates follower goroutines across monitored containers.
type DockerLogManager struct {
	client        *Client
	filter        *ContainerFilter
	configDB      *db.Handle
	sink          model.LogSink
	flushInterval time.Duration
	maxLineBytes  int

	droppedLines atomic.Uint64

	mu        sync.RWMutex
	followers map[string]*containerFollower // containerID -> follower

	ctx    context.Context
	cancel context.CancelFunc
}

// NewLogManager initializes a new DockerLogManager.
func NewLogManager(cfg LogManagerConfig) *DockerLogManager {
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 3 * time.Second
	}
	maxLineBytes := cfg.MaxLineBytes
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxLineBytes
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &DockerLogManager{
		client:        cfg.Client,
		filter:        cfg.Filter,
		configDB:      cfg.ConfigDB,
		sink:          cfg.Sink,
		flushInterval: flushInterval,
		maxLineBytes:  maxLineBytes,
		followers:     make(map[string]*containerFollower),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// DroppedLines returns the total number of log lines dropped due to a full sink.
func (m *DockerLogManager) DroppedLines() uint64 {
	return m.droppedLines.Load()
}

// StartContainer initiates log following for a container if matching filter criteria.
func (m *DockerLogManager) StartContainer(ctx context.Context, info ContainerInfo) {
	if m.filter != nil && !m.filter.Match(info.Name) {
		return
	}

	m.mu.Lock()
	if _, exists := m.followers[info.ID]; exists {
		m.mu.Unlock()
		return
	}

	// Register / resolve source in config.db
	var sourceID int64
	if m.configDB != nil {
		sid, err := EnsureSource(ctx, m.configDB, "container", info.Name)
		if err == nil {
			sourceID = sid
		}
	}

	followerCtx, followerCancel := context.WithCancel(m.ctx)
	follower := &containerFollower{
		info:     info,
		sourceID: sourceID,
		detector: logs.NewSourceDetector(time.UTC, 4096),
		ctx:      followerCtx,
		cancel:   followerCancel,
	}

	// Load existing cursor
	if m.configDB != nil && sourceID > 0 {
		lastTS, err := GetSourceOffset(ctx, m.configDB, sourceID)
		if err == nil && lastTS > 0 {
			follower.lastSeenTS = lastTS
		}
	}

	m.followers[info.ID] = follower
	m.mu.Unlock()

	go m.runFollower(follower)
}

// StopContainer stops following logs for the specified container.
func (m *DockerLogManager) StopContainer(containerID string) {
	m.mu.Lock()
	follower, exists := m.followers[containerID]
	if exists {
		delete(m.followers, containerID)
	}
	m.mu.Unlock()

	if exists {
		follower.cancel()
		// Flush final cursor on shutdown
		if m.configDB != nil && follower.sourceID > 0 && follower.lastSeenTS > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = SaveSourceOffset(ctx, m.configDB, follower.sourceID, follower.lastSeenTS)
		}
	}
}

// HandleEvent listens to lifecycle events from EventWatcher and starts/stops followers.
func (m *DockerLogManager) HandleEvent(ev ContainerEvent) {
	switch ev.Action {
	case "start":
		name := ev.ContainerName
		if name == "" {
			if len(ev.ContainerID) >= 12 {
				name = ev.ContainerID[:12]
			} else {
				name = ev.ContainerID
			}
		}
		info := ContainerInfo{
			ID:    ev.ContainerID,
			Name:  name,
			Image: ev.Image,
			State: "running",
		}
		m.StartContainer(m.ctx, info)
	case "die", "stop", "destroy", "kill":
		m.StopContainer(ev.ContainerID)
	}
}

// Stop gracefully terminates all active follower goroutines and flushes final cursors.
func (m *DockerLogManager) Stop() {
	m.cancel()

	m.mu.Lock()
	activeFollowers := make([]*containerFollower, 0, len(m.followers))
	for _, f := range m.followers {
		activeFollowers = append(activeFollowers, f)
	}
	m.followers = make(map[string]*containerFollower)
	m.mu.Unlock()

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()

	for _, f := range activeFollowers {
		f.cancel()
		if m.configDB != nil && f.sourceID > 0 && f.lastSeenTS > 0 {
			_ = SaveSourceOffset(flushCtx, m.configDB, f.sourceID, f.lastSeenTS)
		}
	}
}

// runFollower manages the connection lifecycle and periodic offset flushing for a single container.
func (m *DockerLogManager) runFollower(f *containerFollower) {
	// Periodic cursor flush ticker
	flushTicker := time.NewTicker(m.flushInterval)
	defer flushTicker.Stop()

	go func() {
		for {
			select {
			case <-f.ctx.Done():
				return
			case <-flushTicker.C:
				if f.hasNewOffset.CompareAndSwap(true, false) {
					if m.configDB != nil && f.sourceID > 0 && f.lastSeenTS > 0 {
						_ = SaveSourceOffset(context.Background(), m.configDB, f.sourceID, f.lastSeenTS)
					}
				}
			}
		}
	}()

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		if f.ctx.Err() != nil {
			return
		}

		sinceUnix := int64(0)
		if f.lastSeenTS > 0 {
			// Subtract 1 second overlap to avoid missing sub-second entries across restart boundary
			sinceUnix = (f.lastSeenTS / 1e9) - 1
			if sinceUnix < 0 {
				sinceUnix = 0
			}
		}

		opts := ContainerLogsOptions{
			Follow:     true,
			Stdout:     true,
			Stderr:     true,
			Timestamps: true,
			Since:      sinceUnix,
		}

		body, err := m.client.GetContainerLogsStream(f.ctx, f.info.ID, opts)
		if err != nil {
			if errors.Is(err, context.Canceled) || f.ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrContainerNotFound) {
				m.StopContainer(f.info.ID)
				return
			}
			select {
			case <-f.ctx.Done():
				return
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
		}

		// Reset backoff on successful connection
		backoff = 1 * time.Second

		// Stream demultiplexing loop
		demuxErr := DemuxStream(body, f.info.TTY, m.maxLineBytes, func(stream model.Stream, rawLine []byte) error {
			if f.ctx.Err() != nil {
				return f.ctx.Err()
			}
			m.processLine(f, stream, rawLine)
			return nil
		})
		body.Close()

		if demuxErr != nil && !errors.Is(demuxErr, context.Canceled) && f.ctx.Err() == nil {
			select {
			case <-f.ctx.Done():
				return
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}
}

// processLine parses timestamps, extracts Docker headers, detects level, and emits model.LogEntry.
func (m *DockerLogManager) processLine(f *containerFollower, stream model.Stream, rawLine []byte) {
	if len(rawLine) == 0 {
		return
	}

	var dockerTS int64
	appLine := rawLine

	// 1. Docker prepends RFC 3339 timestamp prefix: "<RFC3339_timestamp> <app_message>"
	spaceIdx := bytes.IndexByte(rawLine, ' ')
	if spaceIdx > 15 && spaceIdx < 40 {
		tsPrefix := rawLine[:spaceIdx]
		// Parse Docker's RFC 3339 prefix
		t, err := time.Parse(time.RFC3339Nano, string(tsPrefix))
		if err == nil {
			dockerTS = t.UnixNano()
			appLine = rawLine[spaceIdx+1:]
		}
	}

	appLine = stripCR(appLine)
	nowNS := time.Now().UTC().UnixNano()

	// 2. Run byte-level detectors
	tsRes := f.detector.DetectTimestamp(logs.DetectInput{
		Line:       appLine,
		ReadTimeNS: nowNS,
		DockerTSNS: dockerTS,
		IsDocker:   true,
	})

	// 3. Deduplicate restart overlap lines
	if f.lastSeenTS > 0 && tsRes.TS <= f.lastSeenTS {
		return
	}
	f.lastSeenTS = tsRes.TS
	f.hasNewOffset.Store(true)

	lvlRes := f.detector.DetectLevel(appLine, tsRes.TS, false)

	// Copy appLine to prevent buffer aliasing when passed across goroutine channel
	msgCopy := make([]byte, len(appLine))
	copy(msgCopy, appLine)

	entry := model.LogEntry{
		Timestamp:       time.Unix(0, tsRes.TS).UTC(),
		TimestampSource: tsRes.TSSrc,
		SourceID:        fmt.Sprintf("%d", f.sourceID),
		Level:           lvlRes.Level,
		LevelSource:     lvlRes.LevelSrc,
		Stream:          stream,
		Message:         msgCopy,
		Raw:             msgCopy,
	}

	if m.sink != nil {
		if !m.sink.PushLog(entry) {
			m.droppedLines.Add(1)
		}
	}
}
