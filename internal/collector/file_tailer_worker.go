package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

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
