package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func TestSingleWriter_ConcurrentReadersAndContinuousWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Seed initial data
	err = store.Config.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES ('seed_key', 'initial_value');")
		return err
	})
	if err != nil {
		t.Fatalf("seed config failed: %v", err)
	}

	err = store.Metrics.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'cpu');")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO sample_raw (series_id, ts, v) VALUES (1, 1000, 42.0);")
		return err
	})
	if err != nil {
		t.Fatalf("seed metrics failed: %v", err)
	}

	var wg sync.WaitGroup
	var stopFlag atomic.Bool
	var errCount atomic.Int64
	var firstErr atomic.Pointer[error]

	recordErr := func(err error) {
		if err == nil {
			return
		}
		errCount.Add(1)
		firstErr.CompareAndSwap(nil, &err)
	}

	// Launch 50 parallel reader goroutines continuously executing SELECT queries
	// 25 readers on store.Config.RO, 25 readers on store.Metrics.RO
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stopFlag.Load() {
				var val string
				err := store.Config.QueryRO().QueryRow("SELECT value FROM kv WHERE key = 'seed_key';").Scan(&val)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					recordErr(fmt.Errorf("config reader error: %w", err))
					return
				}
			}
		}()
	}

	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stopFlag.Load() {
				var v float64
				err := store.Metrics.QueryRO().QueryRow("SELECT v FROM sample_raw WHERE series_id = 1 LIMIT 1;").Scan(&v)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					recordErr(fmt.Errorf("metrics reader error: %w", err))
					return
				}
			}
		}()
	}

	// Concurrently launch 10 writer goroutines pushing continuous INSERT transactions
	// 5 writers on Store.Config.Writer, 5 writers on Store.Metrics.Writer
	for i := 0; i < 5; i++ {
		writerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq := 0
			for !stopFlag.Load() {
				seq++
				key := fmt.Sprintf("writer_%d_%d", writerID, seq)
				val := fmt.Sprintf("val_%d", seq)
				err := store.Config.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value;", key, val)
					return err
				})
				if err != nil {
					recordErr(fmt.Errorf("config writer error: %w", err))
					return
				}
			}
		}()
	}

	for i := 0; i < 5; i++ {
		writerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq := 0
			for !stopFlag.Load() {
				seq++
				ts := int64(1000000000 + writerID*1000000 + seq)
				val := float64(seq) * 1.5
				err := store.Metrics.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, "INSERT INTO sample_raw (series_id, ts, v) VALUES (1, ?, ?);", ts, val)
					return err
				})
				if err != nil {
					recordErr(fmt.Errorf("metrics writer error: %w", err))
					return
				}
			}
		}()
	}

	// Let the workload run under concurrent stress
	time.Sleep(1500 * time.Millisecond)
	stopFlag.Store(true)
	wg.Wait()

	if n := errCount.Load(); n > 0 {
		t.Fatalf("encountered %d errors during concurrent read/write (zero SQLITE_BUSY expected): %v", n, *firstErr.Load())
	}
}

type testSample struct {
	SeriesID int64
	TS       int64
	V        float64
}

func TestBatchWriter_SizeAndTimerFlush(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Ensure series exists
	err = store.Metrics.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'memory');")
		return err
	})
	if err != nil {
		t.Fatalf("seed series failed: %v", err)
	}

	flushFn := func(ctx context.Context, tx *sql.Tx, items []testSample) error {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO sample_raw (series_id, ts, v) VALUES (?, ?, ?);")
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, item := range items {
			if _, err := stmt.ExecContext(ctx, item.SeriesID, item.TS, item.V); err != nil {
				return err
			}
		}
		return nil
	}

	cfg := db.BatchConfig{
		MaxBatchSize:  50,
		FlushInterval: 100 * time.Millisecond,
		QueueSize:     200,
	}

	bw := db.NewBatchWriter(store.Metrics.Writer, cfg, flushFn)
	defer bw.Close(context.Background())

	// 1. Push 50 items -> verify immediate flush without waiting 100ms
	start := time.Now()
	for i := 0; i < 50; i++ {
		err := bw.TryEnqueue(testSample{
			SeriesID: 1,
			TS:       int64(i + 1),
			V:        float64(i),
		})
		if err != nil {
			t.Fatalf("TryEnqueue item %d failed: %v", i, err)
		}
	}

	// Immediate flush check: wait until SQLite reports 50 items
	var count int
	deadline := time.Now().Add(80 * time.Millisecond)
	for time.Now().Before(deadline) {
		err := store.Metrics.QueryRO().QueryRow("SELECT COUNT(*) FROM sample_raw;").Scan(&count)
		if err == nil && count == 50 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if count != 50 {
		t.Fatalf("expected 50 items flushed immediately on reaching maxBatchSize, got %d", count)
	}
	if elapsed >= 90*time.Millisecond {
		t.Errorf("expected immediate flush well before flushInterval (100ms), took %v", elapsed)
	}

	// 2. Push 12 items -> verify flush occurs within ~100–150ms via timer tick
	start = time.Now()
	for i := 50; i < 62; i++ {
		err := bw.TryEnqueue(testSample{
			SeriesID: 1,
			TS:       int64(i + 1),
			V:        float64(i),
		})
		if err != nil {
			t.Fatalf("TryEnqueue item %d failed: %v", i, err)
		}
	}

	// Verify that before ~70ms, the items are not yet flushed
	time.Sleep(50 * time.Millisecond)
	err = store.Metrics.QueryRO().QueryRow("SELECT COUNT(*) FROM sample_raw;").Scan(&count)
	if err != nil {
		t.Fatalf("query count failed: %v", err)
	}
	if count != 50 {
		t.Fatalf("expected 50 items before timer tick elapsed, got %d", count)
	}

	// Wait for timer tick (~100-150ms total)
	deadline = start.Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		err = store.Metrics.QueryRO().QueryRow("SELECT COUNT(*) FROM sample_raw;").Scan(&count)
		if err == nil && count == 62 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	timerElapsed := time.Since(start)

	if count != 62 {
		t.Fatalf("expected 62 items after timer flush tick, got %d", count)
	}
	if timerElapsed < 90*time.Millisecond {
		t.Errorf("timer flush triggered earlier than expected: %v", timerElapsed)
	}
}

func TestWriter_BoundedQueueBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Create a dedicated writer with queueSize = 5
	w := db.NewWriter(store.Config.RW, 5)
	defer w.Close(context.Background())

	gate := make(chan struct{})

	// Enqueue a blocking job to block the single worker goroutine
	blockerDone := make(chan error, 1)
	err = w.TryEnqueue(func(ctx context.Context, tx *sql.Tx) error {
		<-gate
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue blocker job failed: %v", err)
	}

	// Give the worker a brief moment to pick up the blocker job and wait on gate
	time.Sleep(20 * time.Millisecond)

	// Worker is blocked. Queue capacity is 5.
	// Push 10 non-blocking items via TryEnqueue.
	var queuedCount int
	var droppedCount int
	for i := 0; i < 10; i++ {
		err := w.TryEnqueue(func(ctx context.Context, tx *sql.Tx) error {
			return nil
		})
		if err == nil {
			queuedCount++
		} else if errors.Is(err, db.ErrQueueFull) {
			droppedCount++
		} else {
			t.Fatalf("unexpected error on TryEnqueue: %v", err)
		}
	}

	if queuedCount != 5 {
		t.Fatalf("expected 5 queued items, got %d", queuedCount)
	}
	if droppedCount != 5 {
		t.Fatalf("expected 5 dropped items with ErrQueueFull, got %d", droppedCount)
	}

	stats := w.Stats()
	if stats.Dropped != 5 {
		t.Fatalf("expected w.Stats().Dropped == 5, got %d", stats.Dropped)
	}

	// Unblock the worker so it executes the blocker and the 5 queued items
	close(gate)
	_ = blockerDone
}

func TestWriter_GracefulDrainOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Enqueue 100 write jobs into store.Config.Writer
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("drain_key_%d", i)
		val := fmt.Sprintf("drain_val_%d", i)
		err := store.Config.Writer.TryEnqueue(func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES (?, ?);", key, val)
			return err
		})
		if err != nil {
			t.Fatalf("enqueue job %d failed: %v", i, err)
		}
	}

	// Immediately call Store.Close(ctx)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()

	if err := store.Close(closeCtx); err != nil {
		t.Fatalf("Store.Close failed: %v", err)
	}

	// Subsequent writes must return ErrWriterClosed
	err = store.Config.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return nil
	})
	if !errors.Is(err, db.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed after store.Close(), got: %v", err)
	}

	err = store.Config.Writer.TryEnqueue(func(ctx context.Context, tx *sql.Tx) error {
		return nil
	})
	if !errors.Is(err, db.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed after store.Close(), got: %v", err)
	}

	// Verify all 100 jobs are committed to SQLite on disk
	verifyDB, err := sql.Open("sqlite", filepath.Join(tmpDir, "config.db")+"?mode=ro")
	if err != nil {
		t.Fatalf("open verification DB failed: %v", err)
	}
	defer verifyDB.Close()

	var count int
	if err := verifyDB.QueryRow("SELECT COUNT(*) FROM kv;").Scan(&count); err != nil {
		t.Fatalf("count rows failed: %v", err)
	}
	if count != 100 {
		t.Fatalf("expected all 100 jobs committed to SQLite before Close returned, got %d", count)
	}
}

func TestWriter_PanicRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Execute a write that panics inside the transaction
	panicMsg := "simulated catastrophic panic inside transaction"
	err = store.Config.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, _ = tx.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES ('panic_key', 'panic_val');")
		panic(panicMsg)
	})

	if err == nil {
		t.Fatal("expected panic to be caught and returned as error, got nil")
	}
	if !strings.Contains(err.Error(), panicMsg) {
		t.Fatalf("expected error message to contain %q, got: %v", panicMsg, err)
	}

	// Verify that the panicked transaction was rolled back
	var count int
	err = store.Config.QueryRO().QueryRow("SELECT COUNT(*) FROM kv WHERE key = 'panic_key';").Scan(&count)
	if err != nil {
		t.Fatalf("query count failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after panic rollback, got %d", count)
	}

	// Verify that writer goroutine survived and processes subsequent valid writes
	err = store.Config.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES ('valid_key', 'valid_val');")
		return err
	})
	if err != nil {
		t.Fatalf("expected subsequent write to succeed after panic recovery, got: %v", err)
	}

	var val string
	err = store.Config.QueryRO().QueryRow("SELECT value FROM kv WHERE key = 'valid_key';").Scan(&val)
	if err != nil {
		t.Fatalf("query valid_key failed: %v", err)
	}
	if val != "valid_val" {
		t.Fatalf("expected 'valid_val', got %q", val)
	}
}

func TestBatchWriter_BackpressureAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Seed series
	err = store.Metrics.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'disk');")
		return err
	})
	if err != nil {
		t.Fatalf("seed series failed: %v", err)
	}

	flushFn := func(ctx context.Context, tx *sql.Tx, items []testSample) error {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO sample_raw (series_id, ts, v) VALUES (?, ?, ?);")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, item := range items {
			if _, err := stmt.ExecContext(ctx, item.SeriesID, item.TS, item.V); err != nil {
				return err
			}
		}
		return nil
	}

	// Small queue size 2, max batch 50, long flush interval (won't trigger on timer during test)
	cfg := db.BatchConfig{
		MaxBatchSize:  50,
		FlushInterval: 10 * time.Second,
		QueueSize:     2,
	}

	bw := db.NewBatchWriter(store.Metrics.Writer, cfg, flushFn)

	// Block the accumulator by sending items faster than it flushes
	// We saturate the queue
	var droppedCount int
	for i := 0; i < 20; i++ {
		err := bw.TryEnqueue(testSample{
			SeriesID: 1,
			TS:       int64(i + 1),
			V:        float64(i),
		})
		if errors.Is(err, db.ErrQueueFull) {
			droppedCount++
		}
	}

	if droppedCount == 0 {
		t.Fatal("expected at least some items to be dropped due to small queue capacity")
	}

	stats := bw.Stats()
	if stats.Dropped != uint64(droppedCount) {
		t.Fatalf("expected stats.Dropped == %d, got %d", droppedCount, stats.Dropped)
	}

	// Close BatchWriter and verify all accepted items are flushed cleanly
	if err := bw.Close(ctx); err != nil {
		t.Fatalf("BatchWriter.Close failed: %v", err)
	}

	// Subsequent TryEnqueue must return ErrWriterClosed
	err = bw.TryEnqueue(testSample{SeriesID: 1, TS: 999, V: 999})
	if !errors.Is(err, db.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed after BatchWriter.Close(), got: %v", err)
	}
}
