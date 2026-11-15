package metrics

import (
	"sync"
	"sync/atomic"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// Point represents a compact 16-byte numeric time-series measurement in memory.
type Point struct {
	TS    int64   // Unix timestamp in seconds
	Value float64
}

// Stats captures aggregate statistics computed over a time window without heap allocations.
type Stats struct {
	Count int
	Min   float64
	Max   float64
	Avg   float64
}

// AllAbove returns true if every point in the window is strictly above the given threshold.
func (s Stats) AllAbove(threshold float64) bool {
	return s.Count > 0 && s.Min > threshold
}

// AllBelow returns true if every point in the window is strictly below the given threshold.
func (s Stats) AllBelow(threshold float64) bool {
	return s.Count > 0 && s.Max < threshold
}

// SeriesRing is a thread-safe, fixed-capacity circular ring buffer for a single time series.
type SeriesRing struct {
	mu    sync.RWMutex
	buf   []Point
	head  int // next write index
	count int // number of active valid points stored
}

// newSeriesRing allocates a fixed-size circular ring buffer.
func newSeriesRing(capacity int) *SeriesRing {
	if capacity <= 0 {
		capacity = 180
	}
	return &SeriesRing{
		buf: make([]Point, capacity),
	}
}

// Push records a point into the ring buffer with duplicate-tick and stale-tick handling.
func (r *SeriesRing) Push(ts int64, val float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	capLen := len(r.buf)
	if r.count > 0 {
		lastIdx := (r.head - 1 + capLen) % capLen
		last := r.buf[lastIdx]

		// Idempotent duplicate tick update
		if ts == last.TS {
			r.buf[lastIdx].Value = val
			return
		}

		// Ignore stale out-of-order tick
		if ts < last.TS {
			return
		}
	}

	r.buf[r.head] = Point{TS: ts, Value: val}
	r.head = (r.head + 1) % capLen
	if r.count < capLen {
		r.count++
	}
}

// Latest returns the most recent point in O(1) time.
func (r *SeriesRing) Latest() (Point, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.count == 0 {
		return Point{}, false
	}
	lastIdx := (r.head - 1 + len(r.buf)) % len(r.buf)
	return r.buf[lastIdx], true
}

// Oldest returns the oldest point currently held in the ring buffer.
func (r *SeriesRing) Oldest() (Point, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.count == 0 {
		return Point{}, false
	}
	startIdx := 0
	if r.count == len(r.buf) {
		startIdx = r.head
	}
	return r.buf[startIdx], true
}

// Range appends points in chronological order (oldest to newest) with fromTS <= TS <= toTS into dst.
func (r *SeriesRing) Range(fromTS, toTS int64, dst []Point) []Point {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.count == 0 {
		return dst
	}

	capLen := len(r.buf)
	startIdx := 0
	if r.count == capLen {
		startIdx = r.head
	}

	for i := 0; i < r.count; i++ {
		idx := (startIdx + i) % capLen
		p := r.buf[idx]
		if p.TS >= fromTS && p.TS <= toTS {
			dst = append(dst, p)
		}
	}

	return dst
}

// WindowStats computes Count, Min, Max, and Avg over points with TS >= sinceTS with zero heap allocations.
func (r *SeriesRing) WindowStats(sinceTS int64) (Stats, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.count == 0 {
		return Stats{}, false
	}

	capLen := len(r.buf)
	matched := 0
	var sum float64
	var minVal, maxVal float64

	// Traverse newest to oldest
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + capLen*2) % capLen
		p := r.buf[idx]
		if p.TS < sinceTS {
			break
		}

		if matched == 0 {
			minVal = p.Value
			maxVal = p.Value
		} else {
			if p.Value < minVal {
				minVal = p.Value
			}
			if p.Value > maxVal {
				maxVal = p.Value
			}
		}
		sum += p.Value
		matched++
	}

	if matched == 0 {
		return Stats{}, false
	}

	return Stats{
		Count: matched,
		Min:   minVal,
		Max:   maxVal,
		Avg:   sum / float64(matched),
	}, true
}

// CacheStats captures runtime telemetry for HotCache.
type CacheStats struct {
	ActiveSeries      int
	DroppedSeries     uint64
	CapacityPerSeries int
}

// HotCache provides an in-memory registry of fixed-capacity ring buffers for fast dashboard queries and alerts.
type HotCache struct {
	mu        sync.RWMutex
	rings     map[SeriesKey]*SeriesRing
	ringCap   int
	maxSeries int
	dropped   atomic.Uint64
}

// NewHotCache creates a HotCache with the given max series limit and per-series point capacity.
func NewHotCache(maxSeries, ringCapacity int) *HotCache {
	if maxSeries <= 0 {
		maxSeries = 10000
	}
	if ringCapacity <= 0 {
		ringCapacity = 180 // 30 minutes at 10-second intervals
	}
	return &HotCache{
		rings:     make(map[SeriesKey]*SeriesRing),
		ringCap:   ringCapacity,
		maxSeries: maxSeries,
	}
}

// Record ingests metric samples directly into their respective in-memory circular ring buffers.
func (c *HotCache) Record(samples ...model.MetricSample) {
	for _, s := range samples {
		key := SeriesKey{Entity: s.Entity, Metric: s.Metric}

		// Fast path: RLock
		c.mu.RLock()
		ring, ok := c.rings[key]
		c.mu.RUnlock()

		if !ok {
			c.mu.Lock()
			ring, ok = c.rings[key]
			if !ok {
				if len(c.rings) >= c.maxSeries {
					c.dropped.Add(1)
					c.mu.Unlock()
					continue
				}
				ring = newSeriesRing(c.ringCap)
				c.rings[key] = ring
			}
			c.mu.Unlock()
		}

		ring.Push(s.TS, s.Value)
	}
}

// GetLatest returns the newest point for the specified entity and metric.
func (c *HotCache) GetLatest(entity, metric string) (Point, bool) {
	key := SeriesKey{Entity: entity, Metric: metric}
	c.mu.RLock()
	ring, ok := c.rings[key]
	c.mu.RUnlock()

	if !ok {
		return Point{}, false
	}
	return ring.Latest()
}

// GetEntityLatest returns the newest point for every metric associated with entity.
func (c *HotCache) GetEntityLatest(entity string) map[string]Point {
	c.mu.RLock()
	defer c.mu.RUnlock()

	res := make(map[string]Point)
	for key, ring := range c.rings {
		if key.Entity == entity {
			if pt, ok := ring.Latest(); ok {
				res[key.Metric] = pt
			}
		}
	}
	return res
}

// QueryRange appends points for entity and metric within [fromTS, toTS] into dst.
func (c *HotCache) QueryRange(entity, metric string, fromTS, toTS int64, dst []Point) []Point {
	key := SeriesKey{Entity: entity, Metric: metric}
	c.mu.RLock()
	ring, ok := c.rings[key]
	c.mu.RUnlock()

	if !ok {
		return dst
	}
	return ring.Range(fromTS, toTS, dst)
}

// EvalWindow computes aggregate window stats for entity and metric since sinceTS with zero allocations.
func (c *HotCache) EvalWindow(entity, metric string, sinceTS int64) (Stats, bool) {
	key := SeriesKey{Entity: entity, Metric: metric}
	c.mu.RLock()
	ring, ok := c.rings[key]
	c.mu.RUnlock()

	if !ok {
		return Stats{}, false
	}
	return ring.WindowStats(sinceTS)
}

// OldestTS returns the timestamp of the oldest point for entity and metric, or 0 if not found.
func (c *HotCache) OldestTS(entity, metric string) int64 {
	key := SeriesKey{Entity: entity, Metric: metric}
	c.mu.RLock()
	ring, ok := c.rings[key]
	c.mu.RUnlock()

	if !ok {
		return 0
	}
	if pt, ok := ring.Oldest(); ok {
		return pt.TS
	}
	return 0
}

// Stats returns a snapshot of cache metrics.
func (c *HotCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		ActiveSeries:      len(c.rings),
		DroppedSeries:     c.dropped.Load(),
		CapacityPerSeries: c.ringCap,
	}
}
