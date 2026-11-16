package logs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

type mockEvaluatorTap struct {
	mu      sync.Mutex
	entries []model.LogEntry
	called  atomic.Uint64
}

func (m *mockEvaluatorTap) Enqueue(entry model.LogEntry) {
	m.called.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
}

func TestLogIngestor_Multiplexing(t *testing.T) {
	tempDir := t.TempDir()

	idxMgr := NewIndexManager(tempDir)
	defer idxMgr.Close()

	chunkCfg := ChunkWriterConfig{
		BaseDir:          tempDir,
		BlockTargetBytes: 64 * 1024,
		FlushInterval:    100 * time.Millisecond,
		OnBlockFlushed: func(meta BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}
	chunkMgr, err := NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("failed to create ChunkWriterManager: %v", err)
	}
	defer chunkMgr.Close(context.Background())

	tailCache := NewTailCache(10, 50)
	evaluator := &mockEvaluatorTap{}

	ing := NewLogIngestor(LogIngestorConfig{
		TailCache:   tailCache,
		ChunkWriter: chunkMgr,
		Evaluator:   evaluator,
		QueueCap:    100,
	})

	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		ok := ing.PushLog(model.LogEntry{
			SourceID:  "1",
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
			Level:     model.LevelInfo,
			Stream:    model.StreamStdout,
			Message:   []byte(fmt.Sprintf("test message %d", i)),
		})
		if !ok {
			t.Fatalf("expected push %d to succeed", i)
		}
	}

	if ing.IngestedLines() != 20 {
		t.Fatalf("expected 20 ingested lines, got %d", ing.IngestedLines())
	}
	if ing.DroppedLines() != 0 {
		t.Fatalf("expected 0 dropped lines, got %d", ing.DroppedLines())
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ing.Close(closeCtx); err != nil {
		t.Fatalf("ingestor close failed: %v", err)
	}

	// 1. Verify TailCache received entries
	if tailCache.BufferedLines(1) != 20 {
		t.Errorf("expected 20 buffered lines in TailCache, got %d", tailCache.BufferedLines(1))
	}

	// 2. Verify evaluator tap received entries
	if evaluator.called.Load() != 20 {
		t.Errorf("expected 20 evaluator calls, got %d", evaluator.called.Load())
	}
}

func TestLogIngestor_SaturationDrops(t *testing.T) {
	// Tiny queue capacity of 2 with no background consumer to simulate full saturation
	ing := &LogIngestor{
		queue: make(chan model.LogEntry, 2),
	}

	// Push 2 entries -> succeed
	if !ing.PushLog(model.LogEntry{SourceID: "1"}) {
		t.Errorf("expected push 1 to succeed")
	}
	if !ing.PushLog(model.LogEntry{SourceID: "1"}) {
		t.Errorf("expected push 2 to succeed")
	}

	// Push 3rd entry -> should drop without blocking
	if ing.PushLog(model.LogEntry{SourceID: "1"}) {
		t.Errorf("expected push 3 to drop")
	}

	if ing.IngestedLines() != 2 {
		t.Errorf("expected 2 ingested lines, got %d", ing.IngestedLines())
	}
	if ing.DroppedLines() != 1 {
		t.Errorf("expected 1 dropped line, got %d", ing.DroppedLines())
	}
}

func TestLogIngestor_ShutdownDrainsQueue(t *testing.T) {
	evaluator := &mockEvaluatorTap{}

	ing := NewLogIngestor(LogIngestorConfig{
		Evaluator: evaluator,
		QueueCap:  50,
	})

	for i := 0; i < 15; i++ {
		ing.PushLog(model.LogEntry{
			SourceID: "1",
			Message:  []byte(fmt.Sprintf("line %d", i)),
		})
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ing.Close(closeCtx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if evaluator.called.Load() != 15 {
		t.Errorf("expected all 15 queued lines to be processed on shutdown, got %d", evaluator.called.Load())
	}
}
