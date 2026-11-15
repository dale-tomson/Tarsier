package logs

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

func TestIndexRecord_RoundTripZeroAlloc(t *testing.T) {
	testCases := []BlockMeta{
		{
			MinTS:         1700000000000000000,
			MaxTS:         1700000060000000000,
			MinLevel:      model.LevelDebug,
			MaxLevel:      model.LevelFatal,
			StreamMask:    0x07, // NA, Stdout, Stderr
			FileOffset:    0,
			CompressedLen: 4096,
			RecordCount:   250,
		},
		{
			MinTS:         0,
			MaxTS:         0x7fffffffffffffff,
			MinLevel:      model.LevelUnknown,
			MaxLevel:      model.LevelUnknown,
			StreamMask:    0x00,
			FileOffset:    0x7fffffffffffffff,
			CompressedLen: 0xffffffff,
			RecordCount:   0xffffffff,
		},
	}

	for i, tc := range testCases {
		var buf [IndexRecordSize]byte
		MarshalIndexRecord(&buf, tc)

		decoded, err := UnmarshalIndexRecord(buf[:])
		if err != nil {
			t.Fatalf("case %d: UnmarshalIndexRecord failed: %v", i, err)
		}

		if decoded.MinTS != tc.MinTS ||
			decoded.MaxTS != tc.MaxTS ||
			decoded.MinLevel != tc.MinLevel ||
			decoded.MaxLevel != tc.MaxLevel ||
			decoded.StreamMask != tc.StreamMask ||
			decoded.FileOffset != tc.FileOffset ||
			decoded.CompressedLen != tc.CompressedLen ||
			decoded.RecordCount != tc.RecordCount {
			t.Fatalf("case %d round-trip mismatch:\nexpected: %+v\ngot:      %+v", i, tc, decoded)
		}
	}

	// Zero-allocation assertion
	var scratchBuf [IndexRecordSize]byte
	meta := testCases[0]

	allocs := testing.AllocsPerRun(1000, func() {
		MarshalIndexRecord(&scratchBuf, meta)
		m, err := UnmarshalIndexRecord(scratchBuf[:])
		if err != nil || m.MinTS != meta.MinTS {
			panic("mismatch")
		}
	})

	if allocs > 0 {
		t.Errorf("expected 0 allocs/op, got %.2f", allocs)
	}
}

func TestIndexReader_BinarySearchAndPruning(t *testing.T) {
	tmpDir := t.TempDir()
	sourceID := int64(101)
	dateStr := "2026-11-15"

	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	const totalBlocks = 5000
	baseTime := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	blockDuration := 15 * time.Second // 5000 * 15s = 75,000s ~ 20.8 hours

	var expectedMatches []BlockMeta
	// Query target: 10-minute window between 12:00 and 12:10 UTC, Error level or higher, Stderr only
	targetStart := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano()
	targetEnd := time.Date(2026, 11, 15, 12, 10, 0, 0, time.UTC).UnixNano()
	targetLevel := model.LevelError
	targetStreamMask := uint8(1 << model.StreamStderr)

	r := rand.New(rand.NewSource(42))

	var currentOffset int64
	for i := 0; i < totalBlocks; i++ {
		bStart := baseTime.Add(time.Duration(i) * blockDuration).UnixNano()
		bEnd := baseTime.Add(time.Duration(i+1)*blockDuration - time.Millisecond).UnixNano()

		// Vary levels: mostly Info (1..2), occasionally Error/Fatal spikes
		var minLvl, maxLvl model.Level
		if r.Float64() < 0.15 { // 15% have Error/Fatal
			minLvl = model.LevelInfo
			maxLvl = model.LevelError
		} else {
			minLvl = model.LevelDebug
			maxLvl = model.LevelInfo
		}

		// Vary streams: stdout, stderr, or both
		var streamMask uint8
		sChoice := r.Intn(3)
		switch sChoice {
		case 0:
			streamMask = 1 << model.StreamStdout
		case 1:
			streamMask = 1 << model.StreamStderr
		case 2:
			streamMask = (1 << model.StreamStdout) | (1 << model.StreamStderr)
		}

		compLen := uint32(4000 + r.Intn(2000))
		recCount := uint32(200 + r.Intn(100))

		meta := BlockMeta{
			SourceID:      sourceID,
			Date:          dateStr,
			MinTS:         bStart,
			MaxTS:         bEnd,
			MinLevel:      minLvl,
			MaxLevel:      maxLvl,
			StreamMask:    streamMask,
			FileOffset:    currentOffset,
			CompressedLen: compLen,
			RecordCount:   recCount,
		}

		if err := idxMgr.AppendBlock(meta); err != nil {
			t.Fatalf("AppendBlock %d failed: %v", i, err)
		}
		currentOffset += int64(compLen)

		// Check if this block should match our query
		overlapsTime := meta.MaxTS >= targetStart && meta.MinTS <= targetEnd
		matchesLevel := meta.MaxLevel >= targetLevel
		matchesStream := (meta.StreamMask & targetStreamMask) != 0

		if overlapsTime && matchesLevel && matchesStream {
			expectedMatches = append(expectedMatches, meta)
		}
	}

	if err := idxMgr.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	reader := NewIndexReader(tmpDir)

	// Execute search with latency measurement
	startTimer := time.Now()
	results, err := reader.SearchBlocks(BlockQuery{
		SourceID:   sourceID,
		FromTS:     targetStart,
		ToTS:       targetEnd,
		MinLevel:   targetLevel,
		StreamMask: targetStreamMask,
		Descending: false,
	})
	elapsed := time.Since(startTimer)

	if err != nil {
		t.Fatalf("SearchBlocks failed: %v", err)
	}

	t.Logf("SearchBlocks completed in %s, found %d matches (expected %d)", elapsed, len(results), len(expectedMatches))

	if elapsed > 1*time.Millisecond {
		t.Errorf("SearchBlocks took %s, expected < 1ms", elapsed)
	}

	if len(results) != len(expectedMatches) {
		t.Fatalf("result count mismatch: got %d, expected %d", len(results), len(expectedMatches))
	}

	for i, cand := range results {
		exp := expectedMatches[i]
		if cand.Meta.FileOffset != exp.FileOffset {
			t.Fatalf("result %d offset mismatch: got %d, exp %d", i, cand.Meta.FileOffset, exp.FileOffset)
		}
		if cand.Meta.MinTS != exp.MinTS || cand.Meta.MaxTS != exp.MaxTS {
			t.Fatalf("result %d TS mismatch: got [%d, %d], exp [%d, %d]", i, cand.Meta.MinTS, cand.Meta.MaxTS, exp.MinTS, exp.MaxTS)
		}
	}

	// Verify Descending query reverses exact results
	descResults, err := reader.SearchBlocks(BlockQuery{
		SourceID:   sourceID,
		FromTS:     targetStart,
		ToTS:       targetEnd,
		MinLevel:   targetLevel,
		StreamMask: targetStreamMask,
		Descending: true,
	})
	if err != nil {
		t.Fatalf("SearchBlocks Descending failed: %v", err)
	}
	if len(descResults) != len(expectedMatches) {
		t.Fatalf("descending count mismatch: got %d, expected %d", len(descResults), len(expectedMatches))
	}
	for i := range descResults {
		exp := expectedMatches[len(expectedMatches)-1-i]
		if descResults[i].Meta.FileOffset != exp.FileOffset {
			t.Fatalf("descending result %d offset mismatch: got %d, exp %d", i, descResults[i].Meta.FileOffset, exp.FileOffset)
		}
	}
}

func TestIndexManager_CrashRecoveryAndTornTail(t *testing.T) {
	tmpDir := t.TempDir()
	sourceID := int64(77)
	dateStr := "2026-11-15"

	idxMgr := NewIndexManager(tmpDir)

	chunkCfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 65536,
		FlushInterval:    1 * time.Second,
		ZstdLevel:        zstd.SpeedFastest,
		OnBlockFlushed: func(meta BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}

	chunkMgr, err := NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	baseTime := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)

	// 1. Write 5 blocks cleanly
	for i := 0; i < 5; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: baseTime.Add(time.Duration(i*10) * time.Second),
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("clean block %d - %s", i, string(make([]byte, 120)))),
		})
		_ = chunkMgr.Flush()
	}

	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Close()

	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", sourceID), dateStr+".chunk")
	idxPath := filepath.Join(tmpDir, fmt.Sprintf("%d", sourceID), dateStr+".idx")

	// 2. Append a 6th valid block to .chunk directly without indexing it
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	var block6Raw []byte
	block6Raw = make([]byte, BlockHeaderSize)
	b6TS := baseTime.Add(60 * time.Second)
	entry6 := model.LogEntry{
		Timestamp: b6TS,
		SourceID:  fmt.Sprintf("%d", sourceID),
		Level:     model.LevelError,
		Stream:    model.StreamStderr,
		Message:   []byte("sixth unindexed block message"),
	}
	block6Raw = EncodeRecord(block6Raw, entry6)
	EncodeBlockHeader(block6Raw[:BlockHeaderSize], 1, uint32(len(block6Raw)))
	comp6 := enc.EncodeAll(block6Raw, nil)

	fChunk, err := os.OpenFile(chunkPath, os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		t.Fatalf("open chunk failed: %v", err)
	}
	if _, err := fChunk.Write(comp6); err != nil {
		t.Fatalf("write chunk 6 failed: %v", err)
	}
	fChunk.Close()

	// 3. Append 17 torn garbage bytes to .idx
	fIdx, err := os.OpenFile(idxPath, os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		t.Fatalf("open idx failed: %v", err)
	}
	tornBytes := []byte("torn garbage data")
	if len(tornBytes) != 17 {
		t.Fatalf("expected 17 bytes, got %d", len(tornBytes))
	}
	if _, err := fIdx.Write(tornBytes); err != nil {
		t.Fatalf("write torn bytes failed: %v", err)
	}
	fIdx.Close()

	// 4. Run RepairIfNeeded
	if err := RepairIfNeeded(chunkPath, idxPath); err != nil {
		t.Fatalf("RepairIfNeeded failed: %v", err)
	}

	// 5. Verify index now has exactly 6 records
	idxBytes, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read idx failed: %v", err)
	}
	if len(idxBytes)%IndexRecordSize != 0 {
		t.Fatalf("idx size %d is not a multiple of %d", len(idxBytes), IndexRecordSize)
	}
	numRecs := len(idxBytes) / IndexRecordSize
	if numRecs != 6 {
		t.Fatalf("expected 6 recovered records, got %d", numRecs)
	}

	// 6. Search for the 6th block
	reader := NewIndexReader(tmpDir)
	results, err := reader.SearchBlocks(BlockQuery{
		SourceID:   sourceID,
		FromTS:     b6TS.UnixNano(),
		ToTS:       b6TS.UnixNano(),
		MinLevel:   model.LevelError,
		StreamMask: 1 << model.StreamStderr,
	})
	if err != nil {
		t.Fatalf("SearchBlocks failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 6th block, got %d", len(results))
	}
	if results[0].Meta.RecordCount != 1 {
		t.Errorf("expected 1 record in 6th block, got %d", results[0].Meta.RecordCount)
	}
	if results[0].Meta.MaxLevel != model.LevelError {
		t.Errorf("expected LevelError, got %v", results[0].Meta.MaxLevel)
	}
}

func BenchmarkIndexReader_Search5000Blocks(b *testing.B) {
	tmpDir := b.TempDir()
	sourceID := int64(999)
	dateStr := "2026-11-15"

	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	baseTime := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		meta := BlockMeta{
			SourceID:      sourceID,
			Date:          dateStr,
			MinTS:         baseTime.Add(time.Duration(i*10) * time.Second).UnixNano(),
			MaxTS:         baseTime.Add(time.Duration(i*10+9) * time.Second).UnixNano(),
			MinLevel:      model.LevelInfo,
			MaxLevel:      model.LevelError,
			StreamMask:    1 << model.StreamStdout,
			FileOffset:    int64(i * 4096),
			CompressedLen: 4096,
			RecordCount:   200,
		}
		_ = idxMgr.AppendBlock(meta)
	}
	_ = idxMgr.Sync()

	reader := NewIndexReader(tmpDir)
	qStart := baseTime.Add(2500 * 10 * time.Second).UnixNano()
	qEnd := baseTime.Add(2550 * 10 * time.Second).UnixNano()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := reader.SearchBlocks(BlockQuery{
			SourceID:   sourceID,
			FromTS:     qStart,
			ToTS:       qEnd,
			MinLevel:   model.LevelInfo,
			StreamMask: 1 << model.StreamStdout,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
