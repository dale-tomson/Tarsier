package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
)

// PruneStats captures metrics from a chunked table prune execution.
type PruneStats struct {
	RowsDeleted int64
	Batches     int
	Duration    time.Duration
}

// RetentionExecutor manages O(1) filesystem unlinks and non-blocking chunked SQLite pruning.
type RetentionExecutor struct {
	dataDir    string
	logsDir    string
	metricsDB  *db.Handle
	configDB   *db.Handle
	batchSize  int
	batchPause time.Duration
	sleeper    func(context.Context, time.Duration) error
}

// NewRetentionExecutor creates an initialized RetentionExecutor.
func NewRetentionExecutor(dataDir string, metricsDB, configDB *db.Handle, batchSize int, batchPause time.Duration) *RetentionExecutor {
	if batchSize <= 0 {
		batchSize = 5000
	}
	if batchPause <= 0 {
		batchPause = 50 * time.Millisecond
	}
	return &RetentionExecutor{
		dataDir:    dataDir,
		logsDir:    filepath.Join(dataDir, "logs"),
		metricsDB:  metricsDB,
		configDB:   configDB,
		batchSize:  batchSize,
		batchPause: batchPause,
		sleeper: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// SetSleeper overrides the pause sleeper function (e.g. for accelerated testing).
func (e *RetentionExecutor) SetSleeper(sleeper func(context.Context, time.Duration) error) {
	if sleeper != nil {
		e.sleeper = sleeper
	}
}

// DeleteExpiredChunks deletes expired .chunk and .idx files in O(1) time via os.Remove.
func (e *RetentionExecutor) DeleteExpiredChunks(chunks []ExpiredChunkFile) (int64, int64, error) {
	var totalLines int64
	var bytesReclaimed int64

	for _, chunk := range chunks {
		// Calculate lines and bytes from .idx before removing
		var chunkBytes, idxBytes int64
		if info, err := os.Stat(chunk.ChunkPath); err == nil {
			chunkBytes = info.Size()
		}
		if info, err := os.Stat(chunk.IdxPath); err == nil {
			idxBytes = info.Size()
		}

		if idxData, err := os.ReadFile(chunk.IdxPath); err == nil {
			numBlocks := len(idxData) / logs.IndexRecordSize
			for i := 0; i < numBlocks; i++ {
				recBytes := idxData[i*logs.IndexRecordSize : (i+1)*logs.IndexRecordSize]
				if meta, err := logs.UnmarshalIndexRecord(recBytes); err == nil {
					totalLines += int64(meta.RecordCount)
				}
			}
		}

		// Remove chunk and idx files
		_ = os.Remove(chunk.ChunkPath)
		_ = os.Remove(chunk.IdxPath)

		bytesReclaimed += (chunkBytes + idxBytes)

		// Clean up parent source directory if empty
		sourceDir := filepath.Dir(chunk.ChunkPath)
		if entries, err := os.ReadDir(sourceDir); err == nil && len(entries) == 0 {
			_ = os.Remove(sourceDir)
		}
	}

	return totalLines, bytesReclaimed, nil
}

type dayPair struct {
	sourceID  int64
	date      string
	chunkPath string
	idxPath   string
	bytes     int64
	lines     int64
}

// EnforceLogsSizeCap evicts oldest closed chunks until total log storage drops below 90% of logsMaxSize.
func (e *RetentionExecutor) EnforceLogsSizeCap(logsMaxSize int64, todayUTC string) (int, int64, int64, error) {
	if logsMaxSize <= 0 {
		return 0, 0, 0, nil
	}

	var totalBytes int64
	var pairs []dayPair

	entries, err := os.ReadDir(e.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sourceID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		sourceDir := filepath.Join(e.logsDir, entry.Name())
		files, err := os.ReadDir(sourceDir)
		if err != nil {
			continue
		}

		dateFiles := make(map[string]bool)
		for _, f := range files {
			if !f.IsDir() && filepath.Ext(f.Name()) == ".chunk" {
				date := f.Name()[:len(f.Name())-6]
				dateFiles[date] = true
			}
		}

		for date := range dateFiles {
			chunkPath := filepath.Join(sourceDir, date+".chunk")
			idxPath := filepath.Join(sourceDir, date+".idx")

			var chunkBytes, idxBytes int64
			if info, err := os.Stat(chunkPath); err == nil {
				chunkBytes = info.Size()
			}
			if info, err := os.Stat(idxPath); err == nil {
				idxBytes = info.Size()
			}

			var lines int64
			if idxData, err := os.ReadFile(idxPath); err == nil {
				numBlocks := len(idxData) / logs.IndexRecordSize
				for i := 0; i < numBlocks; i++ {
					recBytes := idxData[i*logs.IndexRecordSize : (i+1)*logs.IndexRecordSize]
					if meta, err := logs.UnmarshalIndexRecord(recBytes); err == nil {
						lines += int64(meta.RecordCount)
					}
				}
			}

			pairBytes := chunkBytes + idxBytes
			totalBytes += pairBytes

			pairs = append(pairs, dayPair{
				sourceID:  sourceID,
				date:      date,
				chunkPath: chunkPath,
				idxPath:   idxPath,
				bytes:     pairBytes,
				lines:     lines,
			})
		}
	}

	if totalBytes <= logsMaxSize {
		return 0, 0, 0, nil
	}

	// 90% hysteresis target watermark
	targetWatermark := int64(float64(logsMaxSize) * 0.90)

	// Sort oldest date first. Put today's active UTC chunk last.
	sort.Slice(pairs, func(i, j int) bool {
		isTodayI := (pairs[i].date == todayUTC)
		isTodayJ := (pairs[j].date == todayUTC)
		if isTodayI != isTodayJ {
			return !isTodayI // Non-today files first
		}
		if pairs[i].date != pairs[j].date {
			return pairs[i].date < pairs[j].date
		}
		return pairs[i].sourceID < pairs[j].sourceID
	})

	var deletedFiles int
	var reclaimedBytes int64
	var deletedLines int64

	for _, p := range pairs {
		if totalBytes <= targetWatermark {
			break
		}

		// Don't delete today's active chunk unless no older chunks remain and still over max size
		if p.date == todayUTC && totalBytes <= logsMaxSize {
			break
		}

		_ = os.Remove(p.chunkPath)
		_ = os.Remove(p.idxPath)

		totalBytes -= p.bytes
		reclaimedBytes += p.bytes
		deletedLines += p.lines
		deletedFiles++

		sourceDir := filepath.Dir(p.chunkPath)
		if remEntries, err := os.ReadDir(sourceDir); err == nil && len(remEntries) == 0 {
			_ = os.Remove(sourceDir)
		}
	}

	return deletedFiles, reclaimedBytes, deletedLines, nil
}

// PruneTableChunked deletes rows older than cutoffTS in bounded transactions separated by yields.
func (e *RetentionExecutor) PruneTableChunked(
	ctx context.Context,
	handle *db.Handle,
	table string,
	tsCol string,
	withoutRowID bool,
	cutoffTS int64,
) (PruneStats, error) {
	start := time.Now()
	stats := PruneStats{}

	if handle == nil || cutoffTS <= 0 {
		return stats, nil
	}

	for {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}

		var rowsAffected int64
		err := handle.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
			var query string
			if withoutRowID {
				// Composite primary key for metrics WITHOUT ROWID tables
				query = fmt.Sprintf(`
					DELETE FROM %s
					WHERE (series_id, ts) IN (
						SELECT series_id, ts FROM %s
						WHERE %s < ?
						ORDER BY %s ASC
						LIMIT ?
					);
				`, table, table, tsCol, tsCol)
			} else {
				// Standard rowid table
				query = fmt.Sprintf(`
					DELETE FROM %s
					WHERE rowid IN (
						SELECT rowid FROM %s
						WHERE %s < ?
						ORDER BY %s ASC
						LIMIT ?
					);
				`, table, table, tsCol, tsCol)
			}

			res, execErr := tx.ExecContext(txCtx, query, cutoffTS, e.batchSize)
			if execErr != nil {
				return execErr
			}
			rowsAffected, execErr = res.RowsAffected()
			return execErr
		})

		if err != nil {
			stats.Duration = time.Since(start)
			return stats, fmt.Errorf("prune table %s: %w", table, err)
		}

		stats.RowsDeleted += rowsAffected
		if rowsAffected > 0 {
			stats.Batches++
		}

		// If batch deleted fewer rows than limit, all expired rows have been pruned
		if rowsAffected < int64(e.batchSize) {
			break
		}

		// Yield single-writer connection to live writers
		if err := e.sleeper(ctx, e.batchPause); err != nil {
			stats.Duration = time.Since(start)
			return stats, err
		}
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// PruneMetrics prunes expired rows from sample_raw, sample_1m, sample_5m in metrics.db.
func (e *RetentionExecutor) PruneMetrics(ctx context.Context, plan RetentionPlan) (int64, error) {
	if e.metricsDB == nil {
		return 0, nil
	}

	var totalPruned int64

	// 1. sample_raw
	if plan.RawCutoffTS > 0 {
		stats, err := e.PruneTableChunked(ctx, e.metricsDB, "sample_raw", "ts", true, plan.RawCutoffTS)
		if err != nil && !strings.Contains(err.Error(), "no such table") {
			return totalPruned, err
		}
		totalPruned += stats.RowsDeleted
	}

	// 2. sample_1m
	if plan.Rollup1mCutoff > 0 {
		stats, err := e.PruneTableChunked(ctx, e.metricsDB, "sample_1m", "ts", true, plan.Rollup1mCutoff)
		if err != nil && !strings.Contains(err.Error(), "no such table") {
			return totalPruned, err
		}
		totalPruned += stats.RowsDeleted
	}

	// 3. sample_5m
	if plan.Rollup5mCutoff > 0 {
		stats, err := e.PruneTableChunked(ctx, e.metricsDB, "sample_5m", "ts", true, plan.Rollup5mCutoff)
		if err != nil && !strings.Contains(err.Error(), "no such table") {
			return totalPruned, err
		}
		totalPruned += stats.RowsDeleted
	}

	return totalPruned, nil
}

// PruneEvents prunes expired container_event and alert_event rows in config.db.
func (e *RetentionExecutor) PruneEvents(ctx context.Context, plan RetentionPlan) (int64, error) {
	if e.configDB == nil || plan.EventsCutoffTS <= 0 {
		return 0, nil
	}

	var totalPruned int64

	// 1. container_event
	stats1, err := e.PruneTableChunked(ctx, e.configDB, "container_event", "ts", false, plan.EventsCutoffTS)
	if err == nil {
		totalPruned += stats1.RowsDeleted
	}

	// 2. alert_event
	stats2, err := e.PruneTableChunked(ctx, e.configDB, "alert_event", "created_at", false, plan.EventsCutoffTS)
	if err == nil {
		totalPruned += stats2.RowsDeleted
	}

	return totalPruned, nil
}
