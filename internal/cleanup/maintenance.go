package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// DBFileStats captures filesystem and internal page allocation metrics for a SQLite database.
type DBFileStats struct {
	Name          string `json:"name"`           // "metrics.db" | "config.db"
	DBBytes       int64  `json:"db_bytes"`       // Main .db file size
	WALBytes      int64  `json:"wal_bytes"`      // -wal sidecar size (0 if absent)
	SHMBytes      int64  `json:"shm_bytes"`      // -shm sidecar size (0 if absent)
	TotalBytes    int64  `json:"total_bytes"`    // DBBytes + WALBytes + SHMBytes
	PageSize      int64  `json:"page_size"`      // PRAGMA page_size (typically 4096)
	PageCount     int64  `json:"page_count"`     // PRAGMA page_count
	FreelistCount int64  `json:"freelist_count"` // PRAGMA freelist_count
}

// DBMaintenanceResult records space reclaimed and operation counts for one database.
type DBMaintenanceResult struct {
	Before          DBFileStats   `json:"before"`
	After           DBFileStats   `json:"after"`
	BytesReclaimed  int64         `json:"bytes_reclaimed"`
	VacuumPasses    int           `json:"vacuum_passes"`
	WALBusy         int           `json:"wal_busy"` // 0 = full checkpoint, 1 = blocked by active reader
	WALLogFrames    int           `json:"wal_log_frames"`
	WALCheckpointed int           `json:"wal_checkpointed"`
	Duration        time.Duration `json:"duration"`
}

// MaintenanceSummary reports total database maintenance metrics across metrics.db and config.db.
type MaintenanceSummary struct {
	Metrics        DBMaintenanceResult `json:"metrics"`
	Config         DBMaintenanceResult `json:"config"`
	TotalReclaimed int64               `json:"total_reclaimed"`
}

// DatabaseMaintainerConfig provides tunables for post-cleanup maintenance.
type DatabaseMaintainerConfig struct {
	Store         *db.Store
	PagesPerPass  int           // default 2000 pages (~8 MiB)
	MaxPasses     int           // default 16 passes
	PassPause     time.Duration // default 25ms
	RetryPauseWAL time.Duration // default 100ms
}

// DatabaseMaintainer coordinates incremental_vacuum, query optimization, and WAL truncation.
type DatabaseMaintainer struct {
	store         *db.Store
	pagesPerPass  int
	maxPasses     int
	passPause     time.Duration
	retryPauseWAL time.Duration
	mu            sync.Mutex
}

// NewDatabaseMaintainer creates an initialized DatabaseMaintainer.
func NewDatabaseMaintainer(cfg DatabaseMaintainerConfig) *DatabaseMaintainer {
	if cfg.PagesPerPass <= 0 {
		cfg.PagesPerPass = 2000
	}
	if cfg.MaxPasses <= 0 {
		cfg.MaxPasses = 16
	}
	if cfg.PassPause <= 0 {
		cfg.PassPause = 25 * time.Millisecond
	}
	if cfg.RetryPauseWAL <= 0 {
		cfg.RetryPauseWAL = 100 * time.Millisecond
	}
	return &DatabaseMaintainer{
		store:         cfg.Store,
		pagesPerPass:  cfg.PagesPerPass,
		maxPasses:     cfg.MaxPasses,
		passPause:     cfg.PassPause,
		retryPauseWAL: cfg.RetryPauseWAL,
	}
}

// InspectHandle gathers file size and page stats for a single database handle.
func InspectHandle(ctx context.Context, handle *db.Handle) (DBFileStats, error) {
	stats := DBFileStats{
		Name: handle.Name,
	}

	if info, err := os.Stat(handle.Path); err == nil {
		stats.DBBytes = info.Size()
	}
	if info, err := os.Stat(handle.Path + "-wal"); err == nil {
		stats.WALBytes = info.Size()
	}
	if info, err := os.Stat(handle.Path + "-shm"); err == nil {
		stats.SHMBytes = info.Size()
	}
	stats.TotalBytes = stats.DBBytes + stats.WALBytes + stats.SHMBytes

	dbQuery := handle.RO
	if dbQuery == nil {
		dbQuery = handle.RW
	}
	if dbQuery != nil {
		_ = dbQuery.QueryRowContext(ctx, "PRAGMA page_size;").Scan(&stats.PageSize)
		_ = dbQuery.QueryRowContext(ctx, "PRAGMA page_count;").Scan(&stats.PageCount)
		_ = dbQuery.QueryRowContext(ctx, "PRAGMA freelist_count;").Scan(&stats.FreelistCount)
	}

	return stats, nil
}

// MaintainHandle executes incremental_vacuum, optimize, and wal_checkpoint(TRUNCATE) on handle.
func (m *DatabaseMaintainer) MaintainHandle(ctx context.Context, handle *db.Handle) (DBMaintenanceResult, error) {
	start := time.Now()
	res := DBMaintenanceResult{}

	if handle == nil {
		return res, nil
	}

	// 1. Inspect before stats
	before, err := InspectHandle(ctx, handle)
	if err != nil {
		return res, err
	}
	res.Before = before

	// 2. Incremental vacuum, optimize, and wal_checkpoint via ExecRawWrite
	err = handle.ExecRawWrite(ctx, func(rawCtx context.Context, conn *sql.Conn) error {
		// A. Check auto_vacuum pragma
		var autoVacuum int
		_ = conn.QueryRowContext(rawCtx, "PRAGMA auto_vacuum;").Scan(&autoVacuum)

		// B. Bounded incremental_vacuum passes if free pages exist
		if autoVacuum == 2 && before.FreelistCount > 0 {
			lastFreelist := before.FreelistCount
			for pass := 0; pass < m.maxPasses; pass++ {
				if rawCtx.Err() != nil {
					return rawCtx.Err()
				}

				vacuumCmd := fmt.Sprintf("PRAGMA incremental_vacuum(%d);", m.pagesPerPass)
				if _, vErr := conn.ExecContext(rawCtx, vacuumCmd); vErr != nil {
					break
				}
				res.VacuumPasses++

				var currentFreelist int64
				if sErr := conn.QueryRowContext(rawCtx, "PRAGMA freelist_count;").Scan(&currentFreelist); sErr != nil {
					break
				}

				// If freelist dropped to zero or did not decrease, stop vacuuming
				if currentFreelist == 0 || currentFreelist >= lastFreelist {
					break
				}
				lastFreelist = currentFreelist

				time.Sleep(m.passPause)
			}
		}

		// C. PRAGMA optimize
		_, _ = conn.ExecContext(rawCtx, "PRAGMA optimize;")

		// D. PRAGMA wal_checkpoint(TRUNCATE)
		row := conn.QueryRowContext(rawCtx, "PRAGMA wal_checkpoint(TRUNCATE);")
		if scanErr := row.Scan(&res.WALBusy, &res.WALLogFrames, &res.WALCheckpointed); scanErr == nil {
			if res.WALBusy != 0 {
				// Retry checkpoint once after pause if active reader held read transaction
				time.Sleep(m.retryPauseWAL)
				rowRetry := conn.QueryRowContext(rawCtx, "PRAGMA wal_checkpoint(TRUNCATE);")
				_ = rowRetry.Scan(&res.WALBusy, &res.WALLogFrames, &res.WALCheckpointed)
			}
		}

		return nil
	})

	if err != nil {
		res.Duration = time.Since(start)
		return res, fmt.Errorf("maintain %s: %w", handle.Name, err)
	}

	// 3. Inspect after stats
	after, err := InspectHandle(ctx, handle)
	if err != nil {
		res.Duration = time.Since(start)
		return res, err
	}
	res.After = after

	reclaimed := before.TotalBytes - after.TotalBytes
	if reclaimed < 0 {
		reclaimed = 0
	}
	res.BytesReclaimed = reclaimed
	res.Duration = time.Since(start)

	return res, nil
}

// MaintainAll runs database maintenance on metrics.db first, then config.db.
func (m *DatabaseMaintainer) MaintainAll(ctx context.Context) (MaintenanceSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	summary := MaintenanceSummary{}
	if m.store == nil {
		return summary, nil
	}

	// 1. Maintain metrics.db
	if m.store.Metrics != nil {
		resMetrics, err := m.MaintainHandle(ctx, m.store.Metrics)
		if err == nil {
			summary.Metrics = resMetrics
			summary.TotalReclaimed += resMetrics.BytesReclaimed
		}
	}

	// 2. Maintain config.db
	if m.store.Config != nil {
		resConfig, err := m.MaintainHandle(ctx, m.store.Config)
		if err == nil {
			summary.Config = resConfig
			summary.TotalReclaimed += resConfig.BytesReclaimed
		}
	}

	return summary, nil
}

// OptimizeOnly executes PRAGMA optimize on both databases without running incremental vacuum.
func (m *DatabaseMaintainer) OptimizeOnly(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.store == nil {
		return nil
	}

	if m.store.Metrics != nil {
		_ = m.store.Metrics.ExecRawWrite(ctx, func(rawCtx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(rawCtx, "PRAGMA optimize;")
			return err
		})
	}

	if m.store.Config != nil {
		_ = m.store.Config.ExecRawWrite(ctx, func(rawCtx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(rawCtx, "PRAGMA optimize;")
			return err
		})
	}

	return nil
}
