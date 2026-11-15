package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

func TestExecutor_O1ChunkDeletionAndSizeCap(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	logsDir := filepath.Join(dataDir, "logs")

	idxMgr := logs.NewIndexManager(logsDir)
	defer idxMgr.Close()

	chunkCfg := logs.ChunkWriterConfig{
		BaseDir:          logsDir,
		BlockTargetBytes: 1024,
		FlushInterval:    100 * time.Millisecond,
		ZstdLevel:        zstd.SpeedFastest,
		OnBlockFlushed: func(meta logs.BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}
	chunkMgr, err := logs.NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager: %v", err)
	}

	todayUTC := time.Now().UTC().Format("2006-01-02")
	dates := []string{
		"2026-11-01", // Expired 1
		"2026-11-02", // Expired 2
		"2026-11-03", // Expired 3
		"2026-11-10", // Active 1
		todayUTC,     // Active 2 (Today)
	}

	// 3 sources across 5 days = 15 day-chunks
	for sID := 1; sID <= 3; sID++ {
		for _, dStr := range dates {
			dTime, _ := time.Parse("2006-01-02", dStr)
			for l := 0; l < 10; l++ {
				_ = chunkMgr.Write(model.LogEntry{
					Timestamp: dTime.Add(time.Duration(l) * time.Minute),
					SourceID:  fmt.Sprintf("%d", sID),
					Level:     model.LevelInfo,
					Stream:    model.StreamStdout,
					Message:   []byte(fmt.Sprintf("log line from source %d on date %s", sID, dStr)),
				})
			}
			_ = chunkMgr.Flush()
		}
	}

	_ = chunkMgr.Close(ctx)
	_ = idxMgr.Sync()

	// 1. Discover chunks strictly older than 2026-11-05
	cutoffTS := time.Date(2026, 11, 05, 0, 0, 0, 0, time.UTC).UnixNano()
	expired, err := DiscoverExpiredChunks(logsDir, cutoffTS, nil)
	if err != nil {
		t.Fatalf("DiscoverExpiredChunks: %v", err)
	}

	// 3 sources * 3 expired dates = 9 expired chunks
	if len(expired) != 9 {
		t.Fatalf("expected 9 expired chunk files, got %d", len(expired))
	}

	executor := NewRetentionExecutor(dataDir, nil, nil, 5000, 10*time.Millisecond)

	// Execute O(1) unlinks
	deletedLines, reclaimedBytes, err := executor.DeleteExpiredChunks(expired)
	if err != nil {
		t.Fatalf("DeleteExpiredChunks: %v", err)
	}

	// 9 files * 10 lines = 90 lines
	if deletedLines != 90 {
		t.Errorf("expected 90 deleted lines, got %d", deletedLines)
	}
	if reclaimedBytes <= 0 {
		t.Errorf("expected positive reclaimed bytes, got %d", reclaimedBytes)
	}

	// Assert expired files no longer exist
	for _, chunk := range expired {
		if _, err := os.Stat(chunk.ChunkPath); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, but still exists", chunk.ChunkPath)
		}
		if _, err := os.Stat(chunk.IdxPath); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, but still exists", chunk.IdxPath)
		}
	}

	// Assert active files remain intact
	activeChunk := filepath.Join(logsDir, "1", "2026-11-10.chunk")
	if _, err := os.Stat(activeChunk); os.IsNotExist(err) {
		t.Errorf("active chunk %s was unexpectedly deleted", activeChunk)
	}

	// 2. Test Emergency Size-Cap Eviction
	// Measure remaining log bytes
	var remainingBytes int64
	_ = filepath.Walk(logsDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			remainingBytes += info.Size()
		}
		return nil
	})

	// Set logs_max_size smaller than current remainingBytes
	tightCap := remainingBytes / 2
	deletedFiles, capReclaimed, _, err := executor.EnforceLogsSizeCap(tightCap, todayUTC)
	if err != nil {
		t.Fatalf("EnforceLogsSizeCap: %v", err)
	}

	if deletedFiles == 0 {
		t.Errorf("expected size-cap eviction to delete files, got 0")
	}
	if capReclaimed <= 0 {
		t.Errorf("expected size-cap eviction to reclaim bytes, got %d", capReclaimed)
	}

	// Today's active UTC chunk must NEVER be deleted
	todayChunk := filepath.Join(logsDir, "1", todayUTC+".chunk")
	if _, err := os.Stat(todayChunk); os.IsNotExist(err) {
		t.Errorf("CRITICAL: today's active UTC chunk %s was deleted by size-cap!", todayChunk)
	}
}

func TestExecutor_ChunkedSQLitePruningNonBlocking(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	// Seed series
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'cpu');`)
		return err
	})

	// Seed 25,000 expired rows (ts 1..25000) + 2,000 active rows (ts 30000..32000)
	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, `INSERT INTO sample_raw (series_id, ts, v) VALUES (1, ?, 50.0);`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for ts := 1; ts <= 25000; ts++ {
			if _, err := stmt.ExecContext(txCtx, ts); err != nil {
				return err
			}
		}
		for ts := 30000; ts < 32000; ts++ {
			if _, err := stmt.ExecContext(txCtx, ts); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed seeding sample_raw: %v", err)
	}

	executor := NewRetentionExecutor(tmpDir, store.Metrics, store.Config, 5000, 5*time.Millisecond)

	// Launch concurrent live writer continuously writing new samples
	stopWriter := make(chan struct{})
	var writerErrors atomic.Int64
	var writerWrites atomic.Int64

	go func() {
		liveTS := 40000
		for {
			select {
			case <-stopWriter:
				return
			default:
				liveTS++
				wErr := store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(txCtx, `INSERT INTO sample_raw (series_id, ts, v) VALUES (1, ?, 99.0);`, liveTS)
					return err
				})
				if wErr != nil {
					writerErrors.Add(1)
				} else {
					writerWrites.Add(1)
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Prune rows where ts < 26000 (the 25,000 expired rows)
	stats, err := executor.PruneTableChunked(ctx, store.Metrics, "sample_raw", "ts", true, 26000)
	close(stopWriter)

	if err != nil {
		t.Fatalf("PruneTableChunked failed: %v", err)
	}

	if stats.RowsDeleted != 25000 {
		t.Errorf("expected 25,000 deleted rows, got %d", stats.RowsDeleted)
	}
	if stats.Batches != 5 {
		t.Errorf("expected 5 batches (5000 rows each), got %d", stats.Batches)
	}

	if writerErrors.Load() > 0 {
		t.Errorf("concurrent writer hit %d database errors during pruning!", writerErrors.Load())
	}
	if writerWrites.Load() == 0 {
		t.Errorf("concurrent writer was starved and completed 0 writes during pruning")
	}

	// Verify all 2,000 active rows remain
	var activeCount int
	_ = store.Metrics.RO.QueryRow("SELECT COUNT(*) FROM sample_raw WHERE ts >= 30000 AND ts < 35000;").Scan(&activeCount)
	if activeCount != 2000 {
		t.Errorf("expected 2,000 active rows, got %d", activeCount)
	}
}

func TestCleanupManager_DryRunAndStrictBlock(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	// Seed source and container event
	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO source (id, kind, name, first_seen, last_seen) VALUES (1, 'docker', 'api', 1000, 2000);
			INSERT INTO container_event (id, container_id, ts, type) VALUES (1, 'c1', 100, 'restart');
		`)
		return err
	})

	cfg := config.NewDefaultConfig()
	cfg.Storage.Path = tmpDir
	cfg.Retention.Logs = "1d"
	cfg.Retention.MetricsRaw = "1d"
	cfg.Retention.Metrics1m = "1d"
	cfg.Retention.Metrics5m = "1d"

	reportBuilder, _ := NewReportBuilder(ReportBuilderConfig{
		DataDir:   tmpDir,
		MetricsRO: store.Metrics.RO,
		ConfigRO:  store.Config.RO,
	})
	tmplEngine, _ := NewTemplateEngine("", "")

	// 1. Dry Run test
	mgr, err := NewCleanupManager(CleanupManagerConfig{
		Config:         cfg,
		Store:          store,
		ReportBuilder:  reportBuilder,
		TemplateEngine: tmplEngine,
	})
	if err != nil {
		t.Fatalf("NewCleanupManager: %v", err)
	}

	resDry, err := mgr.Run(ctx, "manual", true)
	if err != nil {
		t.Fatalf("Dry run failed: %v", err)
	}

	if !resDry.DryRun {
		t.Errorf("expected DryRun=true in result")
	}
	if resDry.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", resDry.Status)
	}

	// Verify dry_run = 1 row recorded in cleanup_run
	var dryRunRecorded int
	_ = store.Config.RO.QueryRow("SELECT dry_run FROM cleanup_run WHERE id = ?", resDry.ID).Scan(&dryRunRecorded)
	if dryRunRecorded != 1 {
		t.Errorf("expected cleanup_run.dry_run = 1 in database, got %d", dryRunRecorded)
	}

	// Verify container_event row was NOT deleted during dry run
	var eventCount int
	_ = store.Config.RO.QueryRow("SELECT COUNT(*) FROM container_event;").Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("dry run deleted event rows! expected 1, got %d", eventCount)
	}

	// 2. Strict OnFailure: block mode test
	failingSender := &mockSender{name: "discord", failCount: 10}
	reportCfg := config.CleanupReportConfig{
		Enabled:     config.NewDefaultConfig().Cleanup.Report.Enabled,
		Channels:    []string{"discord"},
		MinAttempts: 2,
		OnFailure:   "block",
	}
	deliverer := NewReportDeliverer(reportCfg, []ReportSender{failingSender},
		WithDelivererSleeper(func(ctx context.Context, d time.Duration) error { return nil }),
	)

	mgrStrict, _ := NewCleanupManager(CleanupManagerConfig{
		Config:         cfg,
		Store:          store,
		ReportBuilder:  reportBuilder,
		TemplateEngine: tmplEngine,
		Deliverer:      deliverer,
	})

	resStrict, err := mgrStrict.Run(ctx, "manual", false)
	if err != nil {
		t.Fatalf("Strict run returned unexpected error: %v", err)
	}

	if resStrict.Status != "blocked" {
		t.Errorf("expected status 'blocked', got %q", resStrict.Status)
	}

	// Verify status = 'blocked' recorded in database
	var dbStatus string
	_ = store.Config.RO.QueryRow("SELECT status FROM cleanup_run WHERE id = ?", resStrict.ID).Scan(&dbStatus)
	if dbStatus != "blocked" {
		t.Errorf("expected db status 'blocked', got %q", dbStatus)
	}
}
