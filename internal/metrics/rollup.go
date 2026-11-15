package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// Clock provides an interface for time abstraction to allow deterministic testing.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now().UTC()
}

// DailyStat represents computed aggregate stats for a series over a single day.
type DailyStat struct {
	SeriesID int64
	Entity   string
	Metric   string
	DayTS    int64
	Avg      float64
	Min      float64
	Max      float64 // Peak
}

// RollupEngine coordinates periodic SQL downsampling from sample_raw -> sample_1m -> sample_5m -> sample_1d.
type RollupEngine struct {
	handle *db.Handle
	clock  Clock

	mu           sync.Mutex
	last1mBucket int64
	last5mBucket int64
	last1dBucket int64
	initialized  bool
}

// NewRollupEngine constructs a RollupEngine backed by the metrics database handle.
func NewRollupEngine(handle *db.Handle, clock Clock) *RollupEngine {
	if clock == nil {
		clock = realClock{}
	}
	return &RollupEngine{
		handle: handle,
		clock:  clock,
	}
}

// initWatermarks reads the highest completed bucket timestamps currently stored in rollup tables.
func (r *RollupEngine) initWatermarks(ctx context.Context) error {
	if r.initialized {
		return nil
	}

	var max1m, max5m, max1d sql.NullInt64
	_ = r.handle.RO.QueryRowContext(ctx, "SELECT MAX(ts) FROM sample_1m;").Scan(&max1m)
	_ = r.handle.RO.QueryRowContext(ctx, "SELECT MAX(ts) FROM sample_5m;").Scan(&max5m)
	_ = r.handle.RO.QueryRowContext(ctx, "SELECT MAX(ts) FROM sample_1d;").Scan(&max1d)

	if max1m.Valid {
		r.last1mBucket = max1m.Int64
	}
	if max5m.Valid {
		r.last5mBucket = max5m.Int64
	}
	if max1d.Valid {
		r.last1dBucket = max1d.Int64
	}

	r.initialized = true
	return nil
}

// RunOnce executes a single sequential downsampling pass (1m -> 5m -> 1d) inside dedicated single-writer transactions.
func (r *RollupEngine) RunOnce(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.initWatermarks(ctx); err != nil {
		return fmt.Errorf("init watermarks: %w", err)
	}

	nowUnix := r.clock.Now().Unix()

	// Tier 1: Raw (10s) -> 1-Minute Rollup (sample_1m)
	if err := r.rollup1m(ctx, nowUnix); err != nil {
		return fmt.Errorf("rollup 1m: %w", err)
	}

	// Tier 2: 1-Minute (sample_1m) -> 5-Minute Rollup (sample_5m)
	if err := r.rollup5m(ctx, nowUnix); err != nil {
		return fmt.Errorf("rollup 5m: %w", err)
	}

	// Tier 3: 5-Minute (sample_5m) -> Daily Rollup (sample_1d)
	if err := r.rollup1d(ctx, nowUnix); err != nil {
		return fmt.Errorf("rollup 1d: %w", err)
	}

	return nil
}

// rollup1m aggregates completed 1m buckets from sample_raw into sample_1m with a 1-bucket overlap.
func (r *RollupEngine) rollup1m(ctx context.Context, nowUnix int64) error {
	endBucket := (nowUnix / 60) * 60
	if endBucket <= 0 {
		return nil
	}

	var startBucket int64
	if r.last1mBucket > 0 {
		startBucket = r.last1mBucket - 60 // 1-bucket overlap
		if startBucket < 0 {
			startBucket = 0
		}
	} else {
		var minTS sql.NullInt64
		_ = r.handle.RO.QueryRowContext(ctx, "SELECT MIN(ts) FROM sample_raw;").Scan(&minTS)
		if !minTS.Valid || minTS.Int64 == 0 {
			return nil
		}
		startBucket = (minTS.Int64 / 60) * 60
	}

	if startBucket >= endBucket {
		return nil
	}

	query := `INSERT OR REPLACE INTO sample_1m (series_id, ts, avg, min, max)
SELECT
    series_id,
    (ts / 60) * 60 AS bucket_ts,
    AVG(v)         AS avg,
    MIN(v)         AS min,
    MAX(v)         AS max
FROM sample_raw
WHERE ts >= ? AND ts < ?
GROUP BY series_id, bucket_ts;`

	err := r.handle.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(txCtx, query, startBucket, endBucket)
		return execErr
	})
	if err != nil {
		return err
	}

	r.last1mBucket = endBucket - 60
	return nil
}

// rollup5m aggregates completed 5m buckets from sample_1m into sample_5m with a 1-bucket overlap.
func (r *RollupEngine) rollup5m(ctx context.Context, nowUnix int64) error {
	endBucket := (nowUnix / 300) * 300
	if endBucket <= 0 {
		return nil
	}

	var startBucket int64
	if r.last5mBucket > 0 {
		startBucket = r.last5mBucket - 300 // 1-bucket overlap
		if startBucket < 0 {
			startBucket = 0
		}
	} else {
		var minTS sql.NullInt64
		_ = r.handle.RO.QueryRowContext(ctx, "SELECT MIN(ts) FROM sample_1m;").Scan(&minTS)
		if !minTS.Valid || minTS.Int64 == 0 {
			return nil
		}
		startBucket = (minTS.Int64 / 300) * 300
	}

	if startBucket >= endBucket {
		return nil
	}

	query := `INSERT OR REPLACE INTO sample_5m (series_id, ts, avg, min, max)
SELECT
    series_id,
    (ts / 300) * 300 AS bucket_ts,
    AVG(avg)         AS avg,
    MIN(min)         AS min,
    MAX(max)         AS max
FROM sample_1m
WHERE ts >= ? AND ts < ?
GROUP BY series_id, bucket_ts;`

	err := r.handle.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(txCtx, query, startBucket, endBucket)
		return execErr
	})
	if err != nil {
		return err
	}

	r.last5mBucket = endBucket - 300
	return nil
}

// rollup1d aggregates completed UTC day buckets from sample_5m into sample_1d with a 1-day overlap.
func (r *RollupEngine) rollup1d(ctx context.Context, nowUnix int64) error {
	endDay := (nowUnix / 86400) * 86400
	if endDay <= 0 {
		return nil
	}

	var startDay int64
	if r.last1dBucket > 0 {
		startDay = r.last1dBucket - 86400 // 1-day overlap
		if startDay < 0 {
			startDay = 0
		}
	} else {
		var minTS sql.NullInt64
		_ = r.handle.RO.QueryRowContext(ctx, "SELECT MIN(ts) FROM sample_5m;").Scan(&minTS)
		if !minTS.Valid || minTS.Int64 == 0 {
			return nil
		}
		startDay = (minTS.Int64 / 86400) * 86400
	}

	if startDay >= endDay {
		return nil
	}

	query := `INSERT OR REPLACE INTO sample_1d (series_id, ts, avg, min, max)
SELECT
    series_id,
    (ts / 86400) * 86400 AS day_ts,
    AVG(avg)             AS avg,
    MIN(min)             AS min,
    MAX(max)             AS max
FROM sample_5m
WHERE ts >= ? AND ts < ?
GROUP BY series_id, day_ts;`

	err := r.handle.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(txCtx, query, startDay, endDay)
		return execErr
	})
	if err != nil {
		return err
	}

	r.last1dBucket = endDay - 86400
	return nil
}

// QueryTodaySummary computes today's running live Average and Peak from 00:00:00 UTC up to now.
// It queries sample_1m, falling back to sample_raw if sample_1m does not have rows yet.
func (r *RollupEngine) QueryTodaySummary(ctx context.Context, entity string) ([]DailyStat, error) {
	now := r.clock.Now()
	todayStart := (now.Unix() / 86400) * 86400

	query1m := `SELECT
    s.id,
    s.entity,
    s.metric,
    AVG(m.avg) AS avg,
    MIN(m.min) AS min,
    MAX(m.max) AS max
FROM sample_1m m
JOIN series s ON m.series_id = s.id
WHERE m.ts >= ?`

	args := []any{todayStart}
	if entity != "" {
		query1m += " AND s.entity = ?"
		args = append(args, entity)
	}
	query1m += " GROUP BY s.id, s.entity, s.metric;"

	rows, err := r.handle.RO.QueryContext(ctx, query1m, args...)
	if err != nil {
		return nil, fmt.Errorf("query today summary (1m): %w", err)
	}
	defer rows.Close()

	var stats []DailyStat
	for rows.Next() {
		var stat DailyStat
		stat.DayTS = todayStart
		if err := rows.Scan(&stat.SeriesID, &stat.Entity, &stat.Metric, &stat.Avg, &stat.Min, &stat.Max); err != nil {
			return nil, fmt.Errorf("scan 1m today stat: %w", err)
		}
		stats = append(stats, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(stats) > 0 {
		return stats, nil
	}

	// Fallback to sample_raw for sub-minute freshness on newly started days
	queryRaw := `SELECT
    s.id,
    s.entity,
    s.metric,
    AVG(r.v) AS avg,
    MIN(r.v) AS min,
    MAX(r.v) AS max
FROM sample_raw r
JOIN series s ON r.series_id = s.id
WHERE r.ts >= ?`

	argsRaw := []any{todayStart}
	if entity != "" {
		queryRaw += " AND s.entity = ?"
		argsRaw = append(argsRaw, entity)
	}
	queryRaw += " GROUP BY s.id, s.entity, s.metric;"

	rawRows, err := r.handle.RO.QueryContext(ctx, queryRaw, argsRaw...)
	if err != nil {
		return nil, fmt.Errorf("query today summary (raw): %w", err)
	}
	defer rawRows.Close()

	for rawRows.Next() {
		var stat DailyStat
		stat.DayTS = todayStart
		if err := rawRows.Scan(&stat.SeriesID, &stat.Entity, &stat.Metric, &stat.Avg, &stat.Min, &stat.Max); err != nil {
			return nil, fmt.Errorf("scan raw today stat: %w", err)
		}
		stats = append(stats, stat)
	}

	return stats, rawRows.Err()
}

// Start launches the background rollup loop, executing an immediate catch-up pass followed by 1-minute ticks.
func (r *RollupEngine) Start(ctx context.Context) {
	// Catch-up on startup
	_ = r.RunOnce(ctx)

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.RunOnce(ctx)
		}
	}
}
