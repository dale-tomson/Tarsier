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
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
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
