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

func TestLogQueryEngine_HotCacheAndDiskSeamlessPagination(t *testing.T) {
	tmpDir := t.TempDir()
	sourceID := int64(10)

	tailCache := NewTailCache(10, 300)
	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	chunkCfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 16 * 1024,
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

	baseTime := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	// Ingest 1000 lines into both TailCache and ChunkWriterManager
	for i := 1; i <= 1000; i++ {
		entry := model.LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Millisecond),
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     model.LevelInfo,
			Stream:    model.StreamStdout,
			Message:   []byte(fmt.Sprintf("log line %04d message payload", i)),
		}
		tailCache.Push(entry)
		if err := chunkMgr.Write(entry); err != nil {
			t.Fatalf("Write %d failed: %v", i, err)
		}
	}

	if err := chunkMgr.Close(context.Background()); err != nil {
		t.Fatalf("chunkMgr.Close failed: %v", err)
	}
	if err := idxMgr.Sync(); err != nil {
		t.Fatalf("idxMgr.Sync failed: %v", err)
	}

	engine, err := NewLogQueryEngine(LogQueryEngineConfig{
		BaseDir:   tmpDir,
		TailCache: tailCache,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	ctx := context.Background()

	// Page 1: Query "" Limit 200 (should serve from TailCache, lines 1000..801)
	res1, err := engine.Search(ctx, LogQuery{
		SourceID: sourceID,
		Limit:    200,
	})
	if err != nil {
		t.Fatalf("Page 1 failed: %v", err)
	}

	if !res1.FromCache {
		t.Errorf("Page 1: expected FromCache == true")
	}
	if res1.ScannedBlocks != 0 {
		t.Errorf("Page 1: expected ScannedBlocks == 0, got %d", res1.ScannedBlocks)
	}
	if len(res1.Entries) != 200 {
		t.Fatalf("Page 1: expected 200 entries, got %d", len(res1.Entries))
	}
	if !res1.HasMore {
		t.Errorf("Page 1: expected HasMore == true")
	}
	if res1.NextCursor == "" {
		t.Errorf("Page 1: expected NextCursor to be populated")
	}

	// Verify Page 1 order: 1000 down to 801
	for idx, entry := range res1.Entries {
		expectedNum := 1000 - idx
		expectedLine := fmt.Sprintf("log line %04d message payload", expectedNum)
		if entry.Line != expectedLine {
			t.Fatalf("Page 1 entry %d mismatch: exp %q, got %q", idx, expectedLine, entry.Line)
		}
	}

	// Page 2: Query using Page 1 NextCursor, Limit 200
	// 800..701 should come from TailCache (100 lines), 700..601 from disk (100 lines)
	res2, err := engine.Search(ctx, LogQuery{
		SourceID: sourceID,
		Cursor:   res1.NextCursor,
		Limit:    200,
	})
	if err != nil {
		t.Fatalf("Page 2 failed: %v", err)
	}

	if len(res2.Entries) != 200 {
		t.Fatalf("Page 2: expected 200 entries, got %d", len(res2.Entries))
	}

	// Verify Page 2 order: 800 down to 601 with zero duplicates or gaps
	for idx, entry := range res2.Entries {
		expectedNum := 800 - idx
		expectedLine := fmt.Sprintf("log line %04d message payload", expectedNum)
		if entry.Line != expectedLine {
			t.Fatalf("Page 2 entry %d mismatch: exp %q, got %q (ID: %s)", idx, expectedLine, entry.Line, entry.ID)
		}
	}
}

func TestLogQueryEngine_FilteredKeywordAndRegexSearch(t *testing.T) {
	tmpDir := t.TempDir()
	sourceID := int64(22)

	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	chunkCfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 16 * 1024,
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

	baseTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	const totalLines = 10000

	r := rand.New(rand.NewSource(1337))
	needleIndices := make(map[int]bool)

	// Inject 5 needle lines at specific points
	needles := []int{1500, 3200, 5800, 7400, 9100}
	for _, n := range needles {
		needleIndices[n] = true
	}

	for i := 1; i <= totalLines; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Millisecond)

		var lvl model.Level
		var stream model.Stream
		var msg string

		if needleIndices[i] {
			lvl = model.LevelError
			stream = model.StreamStderr
			msg = fmt.Sprintf("CRITICAL failure: timeout connecting to redis-%02d shard", i%10)
		} else {
			// Routine background logs: 95% Info, 5% Debug, all stdout
			lvl = model.LevelInfo
			if r.Float64() < 0.1 {
				lvl = model.LevelDebug
			}
			stream = model.StreamStdout
			msg = fmt.Sprintf("routine background operation %d completed successfully", i)
		}

		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: ts,
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     lvl,
			Stream:    stream,
			Message:   []byte(msg),
		})
	}

	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Sync()

	engine, err := NewLogQueryEngine(LogQueryEngineConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	ctx := context.Background()

	// 1. Substring query with MinLevel: LevelError and Stream: StreamStderr
	subRes, err := engine.Search(ctx, LogQuery{
		SourceID:   sourceID,
		Query:      "REDIS-",
		IgnoreCase: true,
		MinLevel:   model.LevelError,
		Stream:     model.StreamStderr,
		Limit:      50,
	})
	if err != nil {
		t.Fatalf("Substring search failed: %v", err)
	}

	if len(subRes.Entries) != len(needles) {
		t.Fatalf("expected %d entries, got %d", len(needles), len(subRes.Entries))
	}

	// 2. Regex query: redis-\d+
	regRes, err := engine.Search(ctx, LogQuery{
		SourceID:   sourceID,
		Query:      `redis-\d+`,
		Regex:      true,
		IgnoreCase: true,
		MinLevel:   model.LevelError,
		Stream:     model.StreamStderr,
		Limit:      50,
	})
	if err != nil {
		t.Fatalf("Regex search failed: %v", err)
	}

	if len(regRes.Entries) != len(subRes.Entries) {
		t.Fatalf("regex entries count %d != substring entries count %d", len(regRes.Entries), len(subRes.Entries))
	}

	for i := range subRes.Entries {
		if subRes.Entries[i].Line != regRes.Entries[i].Line {
			t.Errorf("entry %d mismatch between substring and regex: %q vs %q", i, subRes.Entries[i].Line, regRes.Entries[i].Line)
		}
	}
}

func TestLogQueryEngine_ReadContextAcrossBlockBoundary(t *testing.T) {
	tmpDir := t.TempDir()
	sourceID := int64(33)
	dateStr := "2026-11-15"

	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	// Small block target to force multiple blocks
	chunkCfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 1024,
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
	const totalLines = 500

	for i := 1; i <= totalLines; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Millisecond),
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("context line %04d payload", i)),
		})
	}

	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Sync()

	engine, err := NewLogQueryEngine(LogQueryEngineConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	// Read blocks in .idx to find block #2
	idxPath := filepath.Join(tmpDir, "33", dateStr+".idx")
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read idx failed: %v", err)
	}

	numBlocks := len(idxData) / IndexRecordSize
	if numBlocks < 3 {
		t.Fatalf("expected at least 3 blocks, got %d", numBlocks)
	}

	// Inspect block 1 (second block)
	metaBlock1, err := UnmarshalIndexRecord(idxData[IndexRecordSize : 2*IndexRecordSize])
	if err != nil {
		t.Fatalf("unmarshal block 1 failed: %v", err)
	}

	// Target record is at index 2 of block #2
	targetRecordID := fmt.Sprintf("%s:%d:2", dateStr, metaBlock1.FileOffset)

	records, targetIdx, err := engine.ReadContext(context.Background(), sourceID, targetRecordID, 5, 5)
	if err != nil {
		t.Fatalf("ReadContext failed: %v", err)
	}

	// 5 before + 1 target + 5 after = 11 records
	if len(records) != 11 {
		t.Fatalf("expected 11 records, got %d", len(records))
	}

	if targetIdx != 5 {
		t.Errorf("expected targetIdx == 5, got %d", targetIdx)
	}

	if records[targetIdx].ID != targetRecordID {
		t.Errorf("expected target record ID %s, got %s", targetRecordID, records[targetIdx].ID)
	}

	// Verify records are in strict chronological order
	for i := 1; i < len(records); i++ {
		if records[i].TS <= records[i-1].TS {
			t.Errorf("records not in strictly increasing order at index %d: %d <= %d", i, records[i].TS, records[i-1].TS)
		}
	}
}

func BenchmarkLogQueryEngine_1HourSingleSourceScan(b *testing.B) {
	tmpDir := b.TempDir()
	sourceID := int64(88)

	idxMgr := NewIndexManager(tmpDir)
	defer idxMgr.Close()

	chunkCfg := ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 65536,
		FlushInterval:    1 * time.Second,
		ZstdLevel:        zstd.SpeedDefault,
		OnBlockFlushed: func(meta BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}

	chunkMgr, err := NewChunkWriterManager(chunkCfg)
	if err != nil {
		b.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	baseTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	const totalLines = 50000

	for i := 1; i <= totalLines; i++ {
		ts := baseTime.Add(time.Duration(i*70) * time.Millisecond) // ~3500s ~ 1 hour

		lvl := model.LevelInfo
		msg := fmt.Sprintf(`{"ts":"%s","service":"billing","op":"compute_tax","cust_id":%d,"amount":45.20}`,
			ts.Format(time.RFC3339Nano), i)

		// Inject 3 deadlock errors
		if i == 12000 || i == 28000 || i == 45000 {
			lvl = model.LevelError
			msg = `{"level":"error","error":"transaction deadlock detected on account lock"}`
		}

		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: ts,
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     lvl,
			Stream:    model.StreamStdout,
			Message:   []byte(msg),
		})
	}

	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Sync()

	engine, err := NewLogQueryEngine(LogQueryEngineConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		b.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	ctx := context.Background()
	query := LogQuery{
		SourceID:   sourceID,
		Query:      "deadlock detected",
		MinLevel:   model.LevelError,
		IgnoreCase: true,
		Limit:      50,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Search(ctx, query)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Entries) != 3 {
			b.Fatalf("expected 3 entries, got %d", len(res.Entries))
		}
	}
}
