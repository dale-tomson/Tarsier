package logs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestSourceTailRing_WrapAroundAndOrder(t *testing.T) {
	ring := newSourceTailRing(1, 5)

	baseTime := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 12; i++ {
		ring.Push(model.LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			SourceID:  "1",
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("line %d", i)),
		})
	}

	ring.mu.RLock()
	res := ring.queryTailLocked(TailQuery{
		SourceID: 1,
		Limit:    10,
	})
	ring.mu.RUnlock()

	if len(res.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(res.Entries))
	}
	if !res.HasMoreOlder {
		t.Errorf("expected HasMoreOlder to be true after 12 pushes into capacity 5")
	}

	expectedSeqs := []uint64{12, 11, 10, 9, 8}
	for i, expSeq := range expectedSeqs {
		if res.Entries[i].Seq != expSeq {
			t.Errorf("entry %d: expected Seq %d, got %d", i, expSeq, res.Entries[i].Seq)
		}
		expectedLine := fmt.Sprintf("line %d", expSeq)
		if res.Entries[i].Line != expectedLine {
			t.Errorf("entry %d: expected line %q, got %q", i, expectedLine, res.Entries[i].Line)
		}
	}
}

func TestTailCache_FilteredAndCursorPagination(t *testing.T) {
	cache := NewTailCache(10, 300)
	sourceID := int64(42)

	baseTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	levels := []model.Level{model.LevelInfo, model.LevelWarn, model.LevelError}
	streams := []model.Stream{model.StreamStdout, model.StreamStderr}

	var allExpectedMatches []TailEntry

	// Push 100 entries, including identical timestamps for adjacent entries
	for i := 1; i <= 100; i++ {
		// Pairwise identical timestamps every 2 items
		tsOffset := time.Duration(i/2) * time.Millisecond
		entryTS := baseTime.Add(tsOffset)

		lvl := levels[i%len(levels)]
		stream := streams[i%len(streams)]

		te := cache.Push(model.LogEntry{
			Timestamp: entryTS,
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     lvl,
			Stream:    stream,
			Message:   []byte(fmt.Sprintf("log entry %d", i)),
		})

		// Track matches for: Level >= LevelWarn AND Stream == StreamStderr
		if lvl >= model.LevelWarn && stream == model.StreamStderr {
			allExpectedMatches = append(allExpectedMatches, te)
		}
	}

	if len(allExpectedMatches) == 0 {
		t.Fatalf("expected some matches to be generated")
	}

	// Reverse expected matches to newest-first order
	for i, j := 0, len(allExpectedMatches)-1; i < j; i, j = i+1, j-1 {
		allExpectedMatches[i], allExpectedMatches[j] = allExpectedMatches[j], allExpectedMatches[i]
	}

	// Keyset paginate through all matching entries with Limit = 5
	var collected []TailEntry
	var beforeTS int64
	var beforeSeq uint64

	const pageSize = 5
	for {
		res := cache.QueryTail(TailQuery{
			SourceID:  sourceID,
			MinLevel:  model.LevelWarn,
			Stream:    model.StreamStderr,
			BeforeTS:  beforeTS,
			BeforeSeq: beforeSeq,
			Limit:     pageSize,
		})

		if len(res.Entries) == 0 {
			break
		}

		collected = append(collected, res.Entries...)

		last := res.Entries[len(res.Entries)-1]
		beforeTS = last.TS
		beforeSeq = last.Seq

		if len(res.Entries) < pageSize {
			break
		}
	}

	// Verify exact match count, order, and zero duplicates
	if len(collected) != len(allExpectedMatches) {
		t.Fatalf("collected %d entries, expected %d", len(collected), len(allExpectedMatches))
	}

	seenSeqs := make(map[uint64]bool)
	for i, act := range collected {
		if seenSeqs[act.Seq] {
			t.Fatalf("duplicate Seq %d observed in pagination results", act.Seq)
		}
		seenSeqs[act.Seq] = true

		exp := allExpectedMatches[i]
		if act.Seq != exp.Seq || act.TS != exp.TS || act.Line != exp.Line {
			t.Fatalf("entry %d mismatch:\nexpected: %+v\ngot:      %+v", i, exp, act)
		}
	}
}

func TestTailCache_ContextWindowLookup(t *testing.T) {
	cache := NewTailCache(10, 300)
	sourceID := int64(10)
	baseTime := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)

	var targetSeq uint64
	var targetTS int64

	// Push 20 lines
	for i := 1; i <= 20; i++ {
		te := cache.Push(model.LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			SourceID:  fmt.Sprintf("%d", sourceID),
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("context line %d", i)),
		})
		if i == 10 {
			targetSeq = te.Seq
			targetTS = te.TS
		}
	}

	// Query context for Seq = 10, before = 3, after = 3
	entries, found := cache.QueryContext(sourceID, targetTS, targetSeq, 3, 3)
	if !found {
		t.Fatalf("expected target entry to be found in context lookup")
	}

	// Expected: lines 7..13 in chronological order
	if len(entries) != 7 {
		t.Fatalf("expected 7 entries (3 before + 1 target + 3 after), got %d", len(entries))
	}

	expectedLines := []string{
		"context line 7",
		"context line 8",
		"context line 9",
		"context line 10",
		"context line 11",
		"context line 12",
		"context line 13",
	}

	for i, expLine := range expectedLines {
		if entries[i].Line != expLine {
			t.Errorf("entry %d: expected %q, got %q", i, expLine, entries[i].Line)
		}
	}

	// Verify not found for nonexistent source or nonexistent seq
	_, foundMissing := cache.QueryContext(999, targetTS, targetSeq, 3, 3)
	if foundMissing {
		t.Errorf("expected found=false for nonexistent source")
	}

	_, foundMissingSeq := cache.QueryContext(sourceID, targetTS, 99999, 3, 3)
	if foundMissingSeq {
		t.Errorf("expected found=false for nonexistent sequence")
	}
}

func TestTailCache_MaxSourcesLRUEvictionAndConcurrency(t *testing.T) {
	const maxSources = 25
	const linesPerSource = 50
	cache := NewTailCache(maxSources, linesPerSource)

	const numWriters = 16
	const numReaders = 16
	const totalSources = 60

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 16 concurrent writers pushing across 60 sources
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
					sID := int64((workerID*7 + i) % totalSources)
					cache.Push(model.LogEntry{
						Timestamp: time.Now().UTC(),
						SourceID:  fmt.Sprintf("%d", sID),
						Level:     model.LevelInfo,
						Message:   []byte(fmt.Sprintf("worker %d line %d", workerID, i)),
					})
					i++
					if i > 50 {
						time.Sleep(1 * time.Millisecond)
					}
				}
			}
		}(w)
	}

	// 16 concurrent readers calling QueryTail
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
					sID := int64((workerID*3 + i) % totalSources)
					_ = cache.QueryTail(TailQuery{
						SourceID: sID,
						Limit:    20,
					})
					i++
					time.Sleep(2 * time.Millisecond)
				}
			}
		}(r)
	}

	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()

	active := cache.ActiveSources()
	evicted := cache.EvictedSources()

	t.Logf("Final active sources: %d (max: %d), Evicted sources: %d", active, maxSources, evicted)

	if active > maxSources {
		t.Errorf("active sources %d exceeded maxSources %d", active, maxSources)
	}
	if evicted == 0 {
		t.Errorf("expected some source evictions across %d sources with cap %d", totalSources, maxSources)
	}
}

func BenchmarkTailCache_PushAndQuery(b *testing.B) {
	cache := NewTailCache(100, 300)
	sourceID := int64(1)
	entry := model.LogEntry{
		Timestamp: time.Now().UTC(),
		SourceID:  "1",
		Level:     model.LevelInfo,
		Message:   []byte(`{"level":"info","msg":"benchmark test line for tail cache"}`),
	}

	b.Run("Push", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cache.Push(entry)
		}
	})

	// Pre-fill ring
	for i := 0; i < 300; i++ {
		cache.Push(entry)
	}

	b.Run("QueryTail_Limit50", func(b *testing.B) {
		q := TailQuery{
			SourceID: sourceID,
			Limit:    50,
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res := cache.QueryTail(q)
			if len(res.Entries) != 50 {
				b.Fatalf("expected 50 entries, got %d", len(res.Entries))
			}
		}
	})
}
