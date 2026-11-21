package cleanup

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func TestDatabaseMaintainer_ShrinksDBAndTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Seed 30,000 rows into sample_raw and 2,000 into container_event
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'cpu');`)
		return err
	})

	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, `INSERT INTO sample_raw (series_id, ts, v) VALUES (1, ?, 75.0);`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for i := 1; i <= 30000; i++ {
			if _, err := stmt.ExecContext(txCtx, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed sample_raw: %v", err)
	}

	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, `INSERT INTO container_event (id, container_id, ts, type) VALUES (?, 'c1', ?, 'restart');`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for i := 1; i <= 2000; i++ {
			if _, err := stmt.ExecContext(txCtx, i, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed container_event: %v", err)
	}

	// Flush WAL to main DB file before deletion so pages reside in the main B-tree
	_ = store.Metrics.Checkpoint(ctx)
	_ = store.Config.Checkpoint(ctx)

	// 2. Delete all 30,000 rows from sample_raw and 2,000 from container_event
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `DELETE FROM sample_raw;`)
		return err
	})
	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `DELETE FROM container_event;`)
		return err
	})

	// Checkpoint once so deleted pages move into the main database freelist
	_ = store.Metrics.Checkpoint(ctx)

	beforeMetrics, err := InspectHandle(ctx, store.Metrics)
	if err != nil {
		t.Fatalf("InspectHandle metrics before: %v", err)
	}

	if beforeMetrics.FreelistCount == 0 {
		t.Fatalf("expected positive freelist count after deleting 30,000 rows, got 0")
	}

	maintainer := NewDatabaseMaintainer(DatabaseMaintainerConfig{
		Store:         store,
		PagesPerPass:  2000,
		MaxPasses:     16,
		PassPause:     5 * time.Millisecond,
		RetryPauseWAL: 10 * time.Millisecond,
	})

	// 3. Run MaintainAll
	summary, err := maintainer.MaintainAll(ctx)
	if err != nil {
		t.Fatalf("MaintainAll failed: %v", err)
	}

	// 4. Assertions
	if summary.Metrics.After.FreelistCount >= summary.Metrics.Before.FreelistCount {
		t.Errorf("expected FreelistCount to decrease: before=%d, after=%d",
			summary.Metrics.Before.FreelistCount, summary.Metrics.After.FreelistCount)
	}
	if summary.Metrics.After.FreelistCount != 0 {
		t.Errorf("expected FreelistCount to reach 0, got %d", summary.Metrics.After.FreelistCount)
	}

	if summary.Metrics.After.DBBytes >= summary.Metrics.Before.DBBytes {
		t.Errorf("expected physical .db file to shrink: before=%d, after=%d",
			summary.Metrics.Before.DBBytes, summary.Metrics.After.DBBytes)
	}

	if summary.Metrics.After.WALBytes != 0 {
		t.Errorf("expected metrics WAL file truncated to 0, got %d bytes", summary.Metrics.After.WALBytes)
	}
	if summary.Config.After.WALBytes != 0 {
		t.Errorf("expected config WAL file truncated to 0, got %d bytes", summary.Config.After.WALBytes)
	}

	if summary.TotalReclaimed <= 0 {
		t.Errorf("expected positive TotalReclaimed, got %d", summary.TotalReclaimed)
	}
}

func TestDatabaseMaintainer_ConcurrentReaderSafety(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Seed metrics
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'cpu');
			INSERT INTO sample_raw (series_id, ts, v) VALUES (1, 100, 50.0);
		`)
		return err
	})

	maintainer := NewDatabaseMaintainer(DatabaseMaintainerConfig{
		Store:         store,
		PagesPerPass:  500,
		MaxPasses:     4,
		PassPause:     5 * time.Millisecond,
		RetryPauseWAL: 10 * time.Millisecond,
	})

	stopReaders := make(chan struct{})
	var readerErrors atomic.Int64
	var readerReads atomic.Int64
	var wg sync.WaitGroup

	// Launch 10 concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					var val float64
					err := store.Metrics.RO.QueryRowContext(ctx, "SELECT v FROM sample_raw WHERE series_id = 1 LIMIT 1;").Scan(&val)
					if err != nil && err != sql.ErrNoRows {
						readerErrors.Add(1)
					} else {
						readerReads.Add(1)
					}
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()
	}

	// Run MaintainAll during concurrent reads
	summary, err := maintainer.MaintainAll(ctx)
	close(stopReaders)
	wg.Wait()

	if err != nil {
		t.Fatalf("MaintainAll failed during concurrent reads: %v", err)
	}

	if readerErrors.Load() > 0 {
		t.Errorf("readers encountered %d errors during database maintenance!", readerErrors.Load())
	}
	if readerReads.Load() == 0 {
		t.Errorf("concurrent readers completed 0 reads")
	}

	_ = summary
}

func TestInspectHandle_ReportsAccurateSizes(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	stats, err := InspectHandle(ctx, store.Config)
	if err != nil {
		t.Fatalf("InspectHandle failed: %v", err)
	}

	if stats.Name != "config" {
		t.Errorf("expected name 'config', got %q", stats.Name)
	}
	if stats.PageSize <= 0 {
		t.Errorf("expected positive page size, got %d", stats.PageSize)
	}
	if stats.PageCount <= 0 {
		t.Errorf("expected positive page count, got %d", stats.PageCount)
	}
	if stats.DBBytes <= 0 {
		t.Errorf("expected positive db bytes, got %d", stats.DBBytes)
	}

	// Verify -wal and -shm files are captured if present
	walPath := store.Config.Path + "-wal"
	if _, err := os.Stat(walPath); err == nil {
		if stats.WALBytes < 0 {
			t.Errorf("invalid wal bytes: %d", stats.WALBytes)
		}
	}
}
