package logs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

func TestChunkWriter_50kMultiSourceStreamAndDecompress(t *testing.T) {
	tmpDir := t.TempDir()

	var metaMu sync.Mutex
	var flushedBlocks []BlockMeta

	cfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 65536,
		FlushInterval:    500 * time.Millisecond,
		ZstdLevel:        zstd.SpeedDefault,
		OnBlockFlushed: func(meta BlockMeta) {
			metaMu.Lock()
			flushedBlocks = append(flushedBlocks, meta)
			metaMu.Unlock()
		},
	}

	mgr, err := NewChunkWriterManager(cfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	const totalLines = 50000
	const numSources = 5

	baseTime := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	expectedPerSource := make(map[int64][]model.LogEntry)

	// Repetitive log formats to test typical compression ratio
	formats := []string{
		`{"ts":"%s","level":"%s","service":"auth-service","msg":"user authenticated successfully","user_id":%d,"duration_ms":12.5}`,
		`{"ts":"%s","level":"%s","service":"payment-gateway","msg":"processed stripe charge","charge_id":"ch_%06d","amount_cents":4500}`,
		`{"ts":"%s","level":"%s","service":"order-processor","msg":"inventory reserved for cart","cart_id":%d,"items_count":3}`,
		`[%s] [%s] worker.pool.worker-%d: job finished in 45ms, status=ok`,
		`[%s] [%s] database: connection pool acquired slot %d, active=12/50`,
	}

	levels := []model.Level{
		model.LevelDebug,
		model.LevelInfo,
		model.LevelWarn,
		model.LevelError,
		model.LevelFatal,
	}

	streams := []model.Stream{
		model.StreamNA,
		model.StreamStdout,
		model.StreamStderr,
	}

	var totalUncompressedBytes int64

	for i := 0; i < totalLines; i++ {
		sourceID := int64((i % numSources) + 1)
		sourceStr := fmt.Sprintf("%d", sourceID)
		lineTime := baseTime.Add(time.Duration(i) * time.Millisecond)
		lvl := levels[i%len(levels)]
		stream := streams[i%len(streams)]
		lvlSrc := model.LevelSourceStructured
		tsSrc := model.TimestampSourceLine

		fmtTemplate := formats[i%len(formats)]
		msgStr := fmt.Sprintf(fmtTemplate, lineTime.Format(time.RFC3339Nano), lvl.String(), i)
		msgBytes := []byte(msgStr)
		totalUncompressedBytes += int64(len(msgBytes))

		entry := model.LogEntry{
			Timestamp:       lineTime,
			TimestampSource: tsSrc,
			SourceID:        sourceStr,
			Level:           lvl,
			LevelSource:     lvlSrc,
			Stream:          stream,
			Message:         msgBytes,
			Raw:             msgBytes,
		}

		expectedPerSource[sourceID] = append(expectedPerSource[sourceID], entry)

		if err := mgr.Write(entry); err != nil {
			t.Fatalf("Write entry %d failed: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Decompress and verify round-trip fidelity
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader failed: %v", err)
	}
	defer dec.Close()

	var totalCompressedBytes int64
	decompressedCount := 0

	for sID := int64(1); sID <= numSources; sID++ {
		chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", sID), "2026-11-15.chunk")
		chunkBytes, err := os.ReadFile(chunkPath)
		if err != nil {
			t.Fatalf("failed to read chunk file %s: %v", chunkPath, err)
		}
		totalCompressedBytes += int64(len(chunkBytes))

		var sourceEntries []model.LogEntry

		// Filter flushed blocks for this source
		metaMu.Lock()
		var sourceBlocks []BlockMeta
		for _, b := range flushedBlocks {
			if b.SourceID == sID {
				sourceBlocks = append(sourceBlocks, b)
			}
		}
		metaMu.Unlock()

		if len(sourceBlocks) == 0 {
			t.Fatalf("no blocks flushed for source %d", sID)
		}

		for bIdx, block := range sourceBlocks {
			end := block.FileOffset + int64(block.CompressedLen)
			if int64(len(chunkBytes)) < end {
				t.Fatalf("block %d out of bounds: chunk size %d, block end %d", bIdx, len(chunkBytes), end)
			}

			frameBytes := chunkBytes[block.FileOffset:end]
			rawBytes, err := dec.DecodeAll(frameBytes, nil)
			if err != nil {
				t.Fatalf("source %d block %d zstd decompression failed: %v", sID, bIdx, err)
			}

			if uint32(len(rawBytes)) != block.UncompressedLen {
				t.Fatalf("uncompressed len mismatch: got %d, expected %d", len(rawBytes), block.UncompressedLen)
			}

			entries, err := DecodeBlock(rawBytes, fmt.Sprintf("%d", sID))
			if err != nil {
				t.Fatalf("DecodeBlock failed: %v", err)
			}

			if uint32(len(entries)) != block.RecordCount {
				t.Fatalf("record count mismatch: got %d, expected %d", len(entries), block.RecordCount)
			}

			sourceEntries = append(sourceEntries, entries...)
		}

		expected := expectedPerSource[sID]
		if len(sourceEntries) != len(expected) {
			t.Fatalf("source %d: expected %d entries, got %d", sID, len(expected), len(sourceEntries))
		}

		for idx := range expected {
			eExp := expected[idx]
			eAct := sourceEntries[idx]

			if !eExp.Timestamp.Equal(eAct.Timestamp) {
				t.Fatalf("source %d entry %d TS mismatch: exp %v, got %v", sID, idx, eExp.Timestamp, eAct.Timestamp)
			}
			if eExp.Level != eAct.Level {
				t.Fatalf("source %d entry %d Level mismatch: exp %v, got %v", sID, idx, eExp.Level, eAct.Level)
			}
			if eExp.LevelSource != eAct.LevelSource {
				t.Fatalf("source %d entry %d LevelSource mismatch: exp %v, got %v", sID, idx, eExp.LevelSource, eAct.LevelSource)
			}
			if eExp.TimestampSource != eAct.TimestampSource {
				t.Fatalf("source %d entry %d TSSource mismatch: exp %v, got %v", sID, idx, eExp.TimestampSource, eAct.TimestampSource)
			}
			if eExp.Stream != eAct.Stream {
				t.Fatalf("source %d entry %d Stream mismatch: exp %v, got %v", sID, idx, eExp.Stream, eAct.Stream)
			}
			if string(eExp.Message) != string(eAct.Message) {
				t.Fatalf("source %d entry %d Message mismatch:\nexp: %s\ngot: %s", sID, idx, string(eExp.Message), string(eAct.Message))
			}
		}

		decompressedCount += len(sourceEntries)
	}

	if decompressedCount != totalLines {
		t.Fatalf("total decompressed mismatch: got %d, expected %d", decompressedCount, totalLines)
	}

	// Verify compression ratio > 3x
	compressionRatio := float64(totalUncompressedBytes) / float64(totalCompressedBytes)
	t.Logf("Total uncompressed: %d bytes, Total compressed: %d bytes, Ratio: %.2fx",
		totalUncompressedBytes, totalCompressedBytes, compressionRatio)

	if compressionRatio < 3.0 {
		t.Errorf("compression ratio %.2fx is less than expected 3.0x", compressionRatio)
	}
}

func TestChunkWriter_UTCDateBoundaryRollover(t *testing.T) {
	tmpDir := t.TempDir()

	var flushed []BlockMeta
	var mu sync.Mutex

	cfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 65536,
		FlushInterval:    1 * time.Second,
		OnBlockFlushed: func(meta BlockMeta) {
			mu.Lock()
			flushed = append(flushed, meta)
			mu.Unlock()
		},
	}

	mgr, err := NewChunkWriterManager(cfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	tDay1 := time.Date(2026, 11, 13, 23, 59, 58, 0, time.UTC)
	tDay2 := time.Date(2026, 11, 14, 0, 0, 2, 0, time.UTC)

	// Write Day 1 entry
	err = mgr.Write(model.LogEntry{
		Timestamp: tDay1,
		SourceID:  "42",
		Level:     model.LevelInfo,
		Message:   []byte("last entry of day 1"),
	})
	if err != nil {
		t.Fatalf("Write day 1 failed: %v", err)
	}

	// Write Day 2 entry (must trigger immediate rollover flush of Day 1 buffer)
	err = mgr.Write(model.LogEntry{
		Timestamp: tDay2,
		SourceID:  "42",
		Level:     model.LevelWarn,
		Message:   []byte("first entry of day 2"),
	})
	if err != nil {
		t.Fatalf("Write day 2 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Check files
	file1 := filepath.Join(tmpDir, "42", "2026-11-13.chunk")
	file2 := filepath.Join(tmpDir, "42", "2026-11-14.chunk")

	if _, err := os.Stat(file1); err != nil {
		t.Fatalf("file1 %s does not exist: %v", file1, err)
	}
	if _, err := os.Stat(file2); err != nil {
		t.Fatalf("file2 %s does not exist: %v", file2, err)
	}

	dec, _ := zstd.NewReader(nil)
	defer dec.Close()

	// Verify file 1 content has only Day 1
	b1, _ := os.ReadFile(file1)
	raw1, err := dec.DecodeAll(b1, nil)
	if err != nil {
		t.Fatalf("decompress file1 failed: %v", err)
	}
	entries1, err := DecodeBlock(raw1, "42")
	if err != nil {
		t.Fatalf("DecodeBlock file1 failed: %v", err)
	}
	if len(entries1) != 1 || string(entries1[0].Message) != "last entry of day 1" {
		t.Fatalf("file1 entries mismatch: %v", entries1)
	}

	// Verify file 2 content has only Day 2
	b2, _ := os.ReadFile(file2)
	raw2, err := dec.DecodeAll(b2, nil)
	if err != nil {
		t.Fatalf("decompress file2 failed: %v", err)
	}
	entries2, err := DecodeBlock(raw2, "42")
	if err != nil {
		t.Fatalf("DecodeBlock file2 failed: %v", err)
	}
	if len(entries2) != 1 || string(entries2[0].Message) != "first entry of day 2" {
		t.Fatalf("file2 entries mismatch: %v", entries2)
	}
}

func TestChunkWriter_IdleTimerAndGracefulShutdown(t *testing.T) {
	tmpDir := t.TempDir()

	flushedChan := make(chan BlockMeta, 10)

	cfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 65536,
		FlushInterval:    100 * time.Millisecond,
		OnBlockFlushed: func(meta BlockMeta) {
			flushedChan <- meta
		},
	}

	mgr, err := NewChunkWriterManager(cfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}
	defer mgr.Close(context.Background())

	// Write 3 small entries (< 500 bytes total)
	ts := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		err := mgr.Write(model.LogEntry{
			Timestamp: ts,
			SourceID:  "10",
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("small log message %d", i)),
		})
		if err != nil {
			t.Fatalf("Write %d failed: %v", i, err)
		}
	}

	// Wait for idle timer to trigger flush (FlushInterval = 100ms)
	select {
	case meta := <-flushedChan:
		if meta.SourceID != 10 {
			t.Errorf("expected SourceID 10, got %d", meta.SourceID)
		}
		if meta.RecordCount != 3 {
			t.Errorf("expected 3 records, got %d", meta.RecordCount)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for idle timer flush")
	}
}

func TestChunkWriter_StorageCapCallbackOrErrorPropagation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a read-only subdirectory for source 99 so OpenFile fails
	blockedDir := filepath.Join(tmpDir, "99")
	if err := os.MkdirAll(blockedDir, 0555); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	// On Unix, ensure read-only permissions
	_ = os.Chmod(blockedDir, 0555)
	defer os.Chmod(blockedDir, 0755)

	cfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 100, // Small limit so 1 line forces immediate flush on 2nd write
		FlushInterval:    1 * time.Second,
	}

	mgr, err := NewChunkWriterManager(cfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}
	defer mgr.Close(context.Background())

	// First write fills buffer
	_ = mgr.Write(model.LogEntry{
		Timestamp: time.Now().UTC(),
		SourceID:  "99",
		Message:   make([]byte, 120),
	})

	// Second write triggers flush of buffer which fails due to permissions
	err = mgr.Write(model.LogEntry{
		Timestamp: time.Now().UTC(),
		SourceID:  "99",
		Message:   make([]byte, 20),
	})

	if err == nil {
		// Or verify Flush() returns error
		err = mgr.Flush()
	}

	if err == nil {
		t.Errorf("expected permission error when writing to read-only directory, got nil")
	}
}
