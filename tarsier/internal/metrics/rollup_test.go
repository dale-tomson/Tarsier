package metrics_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/metrics"
)

type testClock struct {
	t time.Time
}

func (c *testClock) Now() time.Time {
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.t = t
}

func (c *testClock) Advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func TestRollup_1mAnd5mAggregation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Base time: 2026-11-13 10:00:00 UTC (epoch = 1794564000)
	baseTime := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	clock := &testClock{t: baseTime}

	// Create 2 series: "host"/"cpu_pct" and "container:api"/"mem_used"
	reg := metrics.NewSeriesRegistry(store.Metrics, 100)
	s1, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil {
		t.Fatalf("create series 1 failed: %v", err)
	}
	s2, err := reg.GetOrCreate(ctx, "container:api", "mem_used")
	if err != nil {
		t.Fatalf("create series 2 failed: %v", err)
	}

	// Insert 30 raw 10-second ticks spanning 5 full completed minutes: [10:00, 10:05)
	// In minute m (0..4), 6 samples (values 10, 20, 30, 40, 50, 60) -> min=10, max=60, avg=35
	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, "INSERT INTO sample_raw (series_id, ts, v) VALUES (?, ?, ?);")
		if err != nil {
			return err
		}
		defer stmt.Close()

		for m := 0; m < 5; m++ {
			for s := 0; s < 6; s++ {
				ts := baseTime.Unix() + int64(m*60+s*10)
				val := float64((s + 1) * 10)
				if _, err := stmt.ExecContext(txCtx, s1, ts, val); err != nil {
					return err
				}
				if _, err := stmt.ExecContext(txCtx, s2, ts, val*100); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed sample_raw failed: %v", err)
	}

	// Clock is at 10:05:05 UTC (5 completed minutes)
	clock.Set(baseTime.Add(5*time.Minute + 5*time.Second))

	engine := metrics.NewRollupEngine(store.Metrics, clock)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	// Verify sample_1m has 5 rows per series
	var count1m int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_1m;").Scan(&count1m); err != nil {
		t.Fatalf("count sample_1m failed: %v", err)
	}
	if count1m != 10 { // 5 minutes * 2 series
		t.Fatalf("expected 10 rows in sample_1m, got %d", count1m)
	}

	// Verify values for s1 in first minute (10:00:00)
	var avg1m, min1m, max1m float64
	err = store.Metrics.RO.QueryRowContext(ctx,
		"SELECT avg, min, max FROM sample_1m WHERE series_id = ? AND ts = ?;",
		s1, baseTime.Unix(),
	).Scan(&avg1m, &min1m, &max1m)
	if err != nil {
		t.Fatalf("query sample_1m row failed: %v", err)
	}
	if avg1m != 35.0 || min1m != 10.0 || max1m != 60.0 {
		t.Fatalf("unexpected sample_1m values: avg=%f, min=%f, max=%f", avg1m, min1m, max1m)
	}

	// Verify sample_5m has 1 row per series (1 completed 5m bucket: 10:00:00)
	var count5m int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_5m;").Scan(&count5m); err != nil {
		t.Fatalf("count sample_5m failed: %v", err)
	}
	if count5m != 2 {
		t.Fatalf("expected 2 rows in sample_5m (1 per series), got %d", count5m)
	}

	var avg5m, min5m, max5m float64
	err = store.Metrics.RO.QueryRowContext(ctx,
		"SELECT avg, min, max FROM sample_5m WHERE series_id = ? AND ts = ?;",
		s1, baseTime.Unix(),
	).Scan(&avg5m, &min5m, &max5m)
	if err != nil {
		t.Fatalf("query sample_5m row failed: %v", err)
	}
	if avg5m != 35.0 || min5m != 10.0 || max5m != 60.0 {
		t.Fatalf("unexpected sample_5m values: avg=%f, min=%f, max=%f", avg5m, min5m, max5m)
	}
}

func TestRollup_Daily1dAverageAndPeak(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Day 1: 2026-11-10 00:00:00 UTC
	day1 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	day3 := day2.Add(24 * time.Hour) // current active day (incomplete)

	reg := metrics.NewSeriesRegistry(store.Metrics, 10)
	sID, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil {
		t.Fatalf("create series failed: %v", err)
	}

	// Seed sample_5m rows:
	// Day 1: 288 buckets (avg=20.0, min=5.0, max=50.0 peak)
	// Day 2: 288 buckets (avg=40.0, min=10.0, max=80.0 peak)
	// Day 3: 50 buckets (partial, avg=60.0, min=15.0, max=95.0 peak)
	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, "INSERT INTO sample_5m (series_id, ts, avg, min, max) VALUES (?, ?, ?, ?, ?);")
		if err != nil {
			return err
		}
		defer stmt.Close()

		for i := 0; i < 288; i++ {
			ts := day1.Unix() + int64(i*300)
			if _, err := stmt.ExecContext(txCtx, sID, ts, 20.0, 5.0, 50.0); err != nil {
				return err
			}
		}
		for i := 0; i < 288; i++ {
			ts := day2.Unix() + int64(i*300)
			if _, err := stmt.ExecContext(txCtx, sID, ts, 40.0, 10.0, 80.0); err != nil {
				return err
			}
		}
		for i := 0; i < 50; i++ {
			ts := day3.Unix() + int64(i*300)
			if _, err := stmt.ExecContext(txCtx, sID, ts, 60.0, 15.0, 95.0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed sample_5m failed: %v", err)
	}

	// Clock is during Day 3 at 08:00:00 UTC
	clock := &testClock{t: day3.Add(8 * time.Hour)}
	engine := metrics.NewRollupEngine(store.Metrics, clock)

	if err := engine.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	// sample_1d must contain finalized rows for Day 1 and Day 2 only (2 rows total)
	var count1d int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_1d;").Scan(&count1d); err != nil {
		t.Fatalf("count sample_1d failed: %v", err)
	}
	if count1d != 2 {
		t.Fatalf("expected 2 completed days in sample_1d, got %d", count1d)
	}

	// Verify Day 1 stats
	var avg1, min1, max1 float64
	err = store.Metrics.RO.QueryRowContext(ctx,
		"SELECT avg, min, max FROM sample_1d WHERE series_id = ? AND ts = ?;",
		sID, day1.Unix(),
	).Scan(&avg1, &min1, &max1)
	if err != nil {
		t.Fatalf("query Day 1 sample_1d failed: %v", err)
	}
	if avg1 != 20.0 || min1 != 5.0 || max1 != 50.0 {
		t.Fatalf("unexpected Day 1 stats: avg=%f, min=%f, max=%f", avg1, min1, max1)
	}

	// Verify Day 2 stats
	var avg2, min2, max2 float64
	err = store.Metrics.RO.QueryRowContext(ctx,
		"SELECT avg, min, max FROM sample_1d WHERE series_id = ? AND ts = ?;",
		sID, day2.Unix(),
	).Scan(&avg2, &min2, &max2)
	if err != nil {
		t.Fatalf("query Day 2 sample_1d failed: %v", err)
	}
	if avg2 != 40.0 || min2 != 10.0 || max2 != 80.0 {
		t.Fatalf("unexpected Day 2 stats: avg=%f, min=%f, max=%f", avg2, min2, max2)
	}

	// Verify Day 3 is NOT in sample_1d
	var day3Count int
	err = store.Metrics.RO.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sample_1d WHERE series_id = ? AND ts = ?;",
		sID, day3.Unix(),
	).Scan(&day3Count)
	if err != nil || day3Count != 0 {
		t.Fatalf("Day 3 must not be prematurely finalized in sample_1d (count=%d)", day3Count)
	}
}

func TestRollup_IdempotencyAndLateTickOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	baseTime := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	clock := &testClock{t: baseTime.Add(2 * time.Minute)}

	reg := metrics.NewSeriesRegistry(store.Metrics, 10)
	sID, _ := reg.GetOrCreate(ctx, "host", "cpu_pct")

	// Insert minute 0 (10:00:00 - 10:00:50, 6 ticks of 20.0)
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		for s := 0; s < 6; s++ {
			ts := baseTime.Unix() + int64(s*10)
			if _, err := tx.ExecContext(txCtx, "INSERT INTO sample_raw VALUES (?, ?, ?);", sID, ts, 20.0); err != nil {
				return err
			}
		}
		return nil
	})

	engine := metrics.NewRollupEngine(store.Metrics, clock)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 1 failed: %v", err)
	}

	var initialAvg float64
	_ = store.Metrics.RO.QueryRowContext(ctx, "SELECT avg FROM sample_1m WHERE series_id = ? AND ts = ?;", sID, baseTime.Unix()).Scan(&initialAvg)
	if initialAvg != 20.0 {
		t.Fatalf("expected initial avg 20.0, got %f", initialAvg)
	}

	// Insert a late sample at boundary: 10:00:55 with value 80.0
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, "INSERT INTO sample_raw VALUES (?, ?, ?);", sID, baseTime.Unix()+55, 80.0)
		return err
	})

	// Run rollup again: 1-bucket overlap re-evaluates 10:00:00 bucket
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 2 failed: %v", err)
	}

	var rowCount int
	_ = store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_1m WHERE series_id = ?;", sID).Scan(&rowCount)
	if rowCount != 1 {
		t.Fatalf("expected row count to remain 1 without duplicates, got %d", rowCount)
	}

	var updatedAvg, updatedMax float64
	_ = store.Metrics.RO.QueryRowContext(ctx, "SELECT avg, max FROM sample_1m WHERE series_id = ? AND ts = ?;", sID, baseTime.Unix()).Scan(&updatedAvg, &updatedMax)
	// 6 samples of 20 + 1 sample of 80 = 200 / 7 = 28.5714
	if math.Abs(updatedAvg-(200.0/7.0)) > 0.01 || updatedMax != 80.0 {
		t.Fatalf("expected updated avg ~28.57 and max 80, got avg=%f, max=%f", updatedAvg, updatedMax)
	}
}

func TestRollup_QueryTodaySummary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Today: 2026-11-13 00:00:00 UTC
	today := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	clock := &testClock{t: today.Add(12 * time.Hour)} // 12:00:00 UTC

	reg := metrics.NewSeriesRegistry(store.Metrics, 10)
	hostCPU, _ := reg.GetOrCreate(ctx, "host", "cpu_pct")
	webCPU, _ := reg.GetOrCreate(ctx, "container:web", "cpu_pct")

	// Seed sample_1m rows for today
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		// host CPU: 2 buckets: (avg=10, min=5, max=20) and (avg=30, min=15, max=60)
		// Overall: avg = (10+30)/2 = 20.0, min = 5.0, peak = 60.0
		t1 := today.Unix() + 3600
		t2 := today.Unix() + 7200
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", hostCPU, t1, 10.0, 5.0, 20.0)
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", hostCPU, t2, 30.0, 15.0, 60.0)

		// web CPU: 1 bucket: (avg=15, min=10, max=25)
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", webCPU, t1, 15.0, 10.0, 25.0)
		return nil
	})

	engine := metrics.NewRollupEngine(store.Metrics, clock)

	// Query host summary
	stats, err := engine.QueryTodaySummary(ctx, "host")
	if err != nil {
		t.Fatalf("QueryTodaySummary failed: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 host stat, got %d", len(stats))
	}

	hostStat := stats[0]
	if hostStat.Entity != "host" || hostStat.Metric != "cpu_pct" {
		t.Fatalf("unexpected entity/metric: %+v", hostStat)
	}
	if hostStat.Avg != 20.0 || hostStat.Min != 5.0 || hostStat.Max != 60.0 {
		t.Fatalf("expected host avg=20, min=5, max=60, got %+v", hostStat)
	}

	// Query all entities (empty string)
	allStats, err := engine.QueryTodaySummary(ctx, "")
	if err != nil {
		t.Fatalf("QueryTodaySummary all failed: %v", err)
	}
	if len(allStats) != 2 {
		t.Fatalf("expected 2 stats for all entities, got %d", len(allStats))
	}
}
