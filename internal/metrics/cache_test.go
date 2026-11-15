package metrics

import (
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestSeriesRing_WrapAroundAndOrder(t *testing.T) {
	ring := newSeriesRing(5)

	// Ring is initially empty
	if _, ok := ring.Latest(); ok {
		t.Fatalf("expected empty ring on Latest()")
	}
	if pts := ring.Range(0, 1000, nil); len(pts) != 0 {
		t.Fatalf("expected 0 points on empty Range(), got %d", len(pts))
	}

	// Push 12 sequential points: ts = 10, 20, ..., 120
	for i := 1; i <= 12; i++ {
		ring.Push(int64(i*10), float64(i*10))
	}

	// Latest should be ts=120, val=120
	latest, ok := ring.Latest()
	if !ok || latest.TS != 120 || latest.Value != 120 {
		t.Fatalf("expected latest {120, 120}, got %+v (ok=%v)", latest, ok)
	}

	// Range should return the 5 newest points: 80, 90, 100, 110, 120 in strict chronological order
	pts := ring.Range(0, 200, nil)
	if len(pts) != 5 {
		t.Fatalf("expected 5 points, got %d", len(pts))
	}

	expectedTS := []int64{80, 90, 100, 110, 120}
	for i, p := range pts {
		if p.TS != expectedTS[i] || p.Value != float64(expectedTS[i]) {
			t.Errorf("at index %d: expected TS %d, got %+v", i, expectedTS[i], p)
		}
	}

	// Partial sub-range
	subPts := ring.Range(95, 115, nil)
	if len(subPts) != 2 || subPts[0].TS != 100 || subPts[1].TS != 110 {
		t.Fatalf("expected points 100 and 110, got %+v", subPts)
	}
}

func TestSeriesRing_DuplicateAndOutOfOrderTicks(t *testing.T) {
	ring := newSeriesRing(5)

	ring.Push(100, 42.5)
	latest, ok := ring.Latest()
	if !ok || latest.Value != 42.5 {
		t.Fatalf("expected 42.5, got %+v", latest)
	}
	if ring.count != 1 {
		t.Fatalf("expected count 1, got %d", ring.count)
	}

	// Duplicate timestamp must update in-place without advancing count
	ring.Push(100, 45.0)
	latest, ok = ring.Latest()
	if !ok || latest.Value != 45.0 {
		t.Fatalf("expected updated value 45.0, got %+v", latest)
	}
	if ring.count != 1 {
		t.Fatalf("expected count to remain 1 after duplicate tick, got %d", ring.count)
	}

	// Stale tick must be ignored
	ring.Push(90, 10.0)
	latest, ok = ring.Latest()
	if !ok || latest.TS != 100 || latest.Value != 45.0 {
		t.Fatalf("stale tick should be ignored, got %+v", latest)
	}
	if ring.count != 1 {
		t.Fatalf("expected count to remain 1 after stale tick, got %d", ring.count)
	}
}

func TestSeriesRing_ZeroAllocWindowStats(t *testing.T) {
	ring := newSeriesRing(180)

	// Populate 180 points: ts = 10, 20, ..., 1800; value = 1.0, 2.0, ..., 180.0
	for i := 1; i <= 180; i++ {
		ring.Push(int64(i*10), float64(i))
	}

	// Window over the last 10 points: ts >= 1710 (points 171..180)
	stats, ok := ring.WindowStats(1710)
	if !ok {
		t.Fatalf("expected valid window stats")
	}

	if stats.Count != 10 {
		t.Errorf("expected count 10, got %d", stats.Count)
	}
	if stats.Min != 171.0 {
		t.Errorf("expected min 171.0, got %f", stats.Min)
	}
	if stats.Max != 180.0 {
		t.Errorf("expected max 180.0, got %f", stats.Max)
	}
	expectedAvg := (171.0 + 180.0) / 2.0 // 175.5
	if math.Abs(stats.Avg-expectedAvg) > 0.001 {
		t.Errorf("expected avg %f, got %f", expectedAvg, stats.Avg)
	}

	if !stats.AllAbove(170.0) {
		t.Errorf("expected AllAbove(170.0) to be true")
	}
	if stats.AllAbove(172.0) {
		t.Errorf("expected AllAbove(172.0) to be false")
	}
	if !stats.AllBelow(185.0) {
		t.Errorf("expected AllBelow(185.0) to be true")
	}
	if stats.AllBelow(175.0) {
		t.Errorf("expected AllBelow(175.0) to be false")
	}

	// Verify 0 allocations/op for WindowStats
	allocsStats := testing.AllocsPerRun(1000, func() {
		_, _ = ring.WindowStats(1710)
	})
	if allocsStats > 0 {
		t.Errorf("WindowStats performed %f allocations/run, expected 0", allocsStats)
	}

	// Verify 0 allocations/op for Range when passing pre-sized slice
	dst := make([]Point, 0, 100)
	allocsRange := testing.AllocsPerRun(1000, func() {
		_ = ring.Range(1700, 1800, dst[:0])
	})
	if allocsRange > 0 {
		t.Errorf("Range with pre-allocated buffer performed %f allocations/run, expected 0", allocsRange)
	}
}

func TestHotCache_MaxSeriesBoundAndConcurrency(t *testing.T) {
	const maxSeries = 50
	cache := NewHotCache(maxSeries, 180)

	var wg sync.WaitGroup
	// 20 concurrent writer goroutines pushing across 100 distinct series
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				seriesIdx := (writerID*10 + i) % 100
				sample := model.MetricSample{
					Entity: fmt.Sprintf("entity-%d", seriesIdx),
					Metric: "cpu_pct",
					TS:     int64(1000 + i*10),
					Value:  float64(i),
				}
				cache.Record(sample)
			}
		}(w)
	}

	// 20 concurrent reader goroutines
	for r := 0; r < 20; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			buf := make([]Point, 0, 30)
			for i := 0; i < 50; i++ {
				seriesIdx := (readerID*10 + i) % 100
				entity := fmt.Sprintf("entity-%d", seriesIdx)

				_, _ = cache.GetLatest(entity, "cpu_pct")
				_ = cache.GetEntityLatest(entity)
				_ = cache.QueryRange(entity, "cpu_pct", 1000, 2000, buf[:0])
				_, _ = cache.EvalWindow(entity, "cpu_pct", 1000)
			}
		}(r)
	}

	wg.Wait()

	stats := cache.Stats()
	if stats.ActiveSeries > maxSeries {
		t.Fatalf("ActiveSeries (%d) exceeded maxSeries cap (%d)", stats.ActiveSeries, maxSeries)
	}
	if stats.DroppedSeries == 0 {
		t.Fatalf("expected dropped series when attempting 100 series against 50 cap")
	}
}
