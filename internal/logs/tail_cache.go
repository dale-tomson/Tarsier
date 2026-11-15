package logs

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

const (
	// DefaultMaxSources is the default maximum number of active source rings retained in memory.
	DefaultMaxSources = 500

	// DefaultLogTailLines is the default circular buffer capacity per source.
	DefaultLogTailLines = 300

	// MinLogTailLines is the minimum allowed ring buffer capacity per source.
	MinLogTailLines = 50

	// MaxLogTailLines is the maximum allowed ring buffer capacity per source.
	MaxLogTailLines = 2000

	// DefaultTailLimit is the default result limit for QueryTail.
	DefaultTailLimit = 200

	// MaxTailLimit is the upper bound on result limits for QueryTail.
	MaxTailLimit = 1000
)

// TailEntry represents a single stored log record in the in-memory ring buffer.
type TailEntry struct {
	Seq      uint64                // Monotonically increasing per-source sequence ID
	SourceID int64
	TS       int64                 // Normalized UTC nanoseconds
	TSSrc    model.TimestampSource // 0..3
	Level    model.Level           // 0..5
	LevelSrc model.LevelSource     // 0..5
	Stream   model.Stream          // 0=NA, 1=Stdout, 2=Stderr
	Line     string                // Raw log line (capped at 64 KiB by collectors)
}

// SourceTailRing is a fixed-capacity thread-safe circular buffer for a single log source.
type SourceTailRing struct {
	mu         sync.RWMutex
	sourceID   int64
	capacity   int
	entries    []TailEntry
	head       int
	count      int
	nextSeq    uint64
	lastActive int64
}

func newSourceTailRing(sourceID int64, capacity int) *SourceTailRing {
	if capacity <= 0 {
		capacity = DefaultLogTailLines
	}

	return &SourceTailRing{
		sourceID:   sourceID,
		capacity:   capacity,
		entries:    make([]TailEntry, capacity),
		nextSeq:    1,
		lastActive: time.Now().UnixNano(),
	}
}

// Push appends a parsed log entry into the circular buffer, overwriting oldest entry on wrap-around.
func (r *SourceTailRing) Push(entry model.LogEntry) TailEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	ts := entry.Timestamp.UnixNano()
	if entry.Timestamp.IsZero() {
		ts = time.Now().UTC().UnixNano()
	}

	lineStr := string(entry.Message)
	if len(lineStr) == 0 && len(entry.Raw) > 0 {
		lineStr = string(entry.Raw)
	}

	seq := r.nextSeq
	r.nextSeq++

	te := TailEntry{
		Seq:      seq,
		SourceID: r.sourceID,
		TS:       ts,
		TSSrc:    entry.TimestampSource,
		Level:    entry.Level,
		LevelSrc: entry.LevelSource,
		Stream:   entry.Stream,
		Line:     lineStr,
	}

	r.entries[r.head] = te
	r.head = (r.head + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
	}
	r.lastActive = time.Now().UnixNano()

	return te
}

// LastActive returns the Unix nanosecond timestamp of the most recent push.
func (r *SourceTailRing) LastActive() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastActive
}

// Count returns the number of valid items currently retained in the ring.
func (r *SourceTailRing) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// TailQuery specifies filtering and cursor pagination options for hot tail reading.
type TailQuery struct {
	SourceID  int64
	MinLevel  model.Level  // 0 = all levels
	Stream    model.Stream // 0 = all streams, 1 = stdout, 2 = stderr
	FromTS    int64        // 0 = unbounded
	ToTS      int64        // 0 = unbounded
	BeforeTS  int64        // Keyset pagination cursor: return entries strictly older than (BeforeTS, BeforeSeq)
	BeforeSeq uint64
	Limit     int // Default 200, clamped to 1..1000
}

// TailResult encapsulates the results of QueryTail.
type TailResult struct {
	Entries        []TailEntry // Ordered newest-first
	HasMoreOlder   bool        // True if older entries exist beyond what was returned
	OldestBuffered int64       // Lowest TS currently retained in this source's ring buffer
}

// queryTailLocked traverses the ring from newest to oldest under read lock.
func (r *SourceTailRing) queryTailLocked(q TailQuery) TailResult {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultTailLimit
	} else if limit > MaxTailLimit {
		limit = MaxTailLimit
	}

	if r.count == 0 {
		return TailResult{
			Entries:        nil,
			HasMoreOlder:   false,
			OldestBuffered: 0,
		}
	}

	oldestIdx := (r.head - r.count + r.capacity) % r.capacity
	oldestBuffered := r.entries[oldestIdx].TS

	out := make([]TailEntry, 0, min(limit, r.count))
	hitLimit := false
	remainingMatches := false

	// Walk backward from newest (head - 1) to oldest
	for k := 0; k < r.count; k++ {
		idx := (r.head - 1 - k + r.capacity*2) % r.capacity
		e := r.entries[idx]

		// Keyset cursor filter: strictly older than (BeforeTS, BeforeSeq)
		if q.BeforeTS > 0 || q.BeforeSeq > 0 {
			if q.BeforeTS > 0 {
				if e.TS > q.BeforeTS {
					continue
				}
				if e.TS == q.BeforeTS && q.BeforeSeq > 0 && e.Seq >= q.BeforeSeq {
					continue
				}
			} else if q.BeforeSeq > 0 && e.Seq >= q.BeforeSeq {
				continue
			}
		}

		// Level filter
		if q.MinLevel > 0 && e.Level < q.MinLevel {
			continue
		}

		// Stream filter
		if q.Stream != model.StreamNA && e.Stream != q.Stream {
			continue
		}

		// Time range filter
		if q.FromTS > 0 && e.TS < q.FromTS {
			continue
		}
		if q.ToTS > 0 && e.TS > q.ToTS {
			continue
		}

		if len(out) < limit {
			out = append(out, e)
		} else {
			hitLimit = true
			remainingMatches = true
			break
		}
	}

	hasMoreOlder := false
	if hitLimit && remainingMatches {
		hasMoreOlder = true
	} else if r.count == r.capacity {
		hasMoreOlder = true
	} else if q.FromTS > 0 && q.FromTS < oldestBuffered {
		hasMoreOlder = true
	}

	return TailResult{
		Entries:        out,
		HasMoreOlder:   hasMoreOlder,
		OldestBuffered: oldestBuffered,
	}
}

// queryContextLocked retrieves target entry plus preceding and following lines in chronological order.
func (r *SourceTailRing) queryContextLocked(targetTS int64, targetSeq uint64, before, after int) ([]TailEntry, bool) {
	if r.count == 0 {
		return nil, false
	}

	targetPos := -1

	// Iterate in chronological order from oldest (pos 0) to newest (pos count-1)
	for k := 0; k < r.count; k++ {
		idx := (r.head - r.count + k + r.capacity*2) % r.capacity
		e := r.entries[idx]

		if targetSeq > 0 && e.Seq == targetSeq {
			targetPos = k
			break
		} else if targetSeq == 0 && targetTS > 0 && e.TS == targetTS {
			targetPos = k
			break
		}
	}

	if targetPos == -1 {
		return nil, false
	}

	startPos := max(0, targetPos-before)
	endPos := min(r.count-1, targetPos+after)

	result := make([]TailEntry, 0, endPos-startPos+1)
	for k := startPos; k <= endPos; k++ {
		idx := (r.head - r.count + k + r.capacity*2) % r.capacity
		result = append(result, r.entries[idx])
	}

	return result, true
}

// TailCache maintains in-memory hot log ring buffers per source with LRU eviction and keyset pagination.
type TailCache struct {
	mu             sync.RWMutex
	maxSources     int
	linesPerSource int
	rings          map[int64]*SourceTailRing
	evictedSources atomic.Uint64
}

// NewTailCache constructs a TailCache with clamped capacity limits.
func NewTailCache(maxSources, linesPerSource int) *TailCache {
	if maxSources <= 0 {
		maxSources = DefaultMaxSources
	}
	if linesPerSource < MinLogTailLines {
		linesPerSource = MinLogTailLines
	} else if linesPerSource > MaxLogTailLines {
		linesPerSource = MaxLogTailLines
	}

	return &TailCache{
		maxSources:     maxSources,
		linesPerSource: linesPerSource,
		rings:          make(map[int64]*SourceTailRing),
	}
}

// Push routes a parsed LogEntry to its source ring buffer, evicting the LRU source ring if capacity is full.
func (c *TailCache) Push(entry model.LogEntry) TailEntry {
	sourceID := parseSourceID(entry.SourceID)

	// Fast path: existing ring
	c.mu.RLock()
	ring := c.rings[sourceID]
	c.mu.RUnlock()

	if ring != nil {
		return ring.Push(entry)
	}

	// Slow path: register new ring
	c.mu.Lock()
	ring = c.rings[sourceID]
	if ring == nil {
		for len(c.rings) >= c.maxSources {
			var oldestID int64
			var oldestTS int64 = math.MaxInt64
			var found bool
			for id, r := range c.rings {
				ts := r.LastActive()
				if ts < oldestTS {
					oldestTS = ts
					oldestID = id
					found = true
				}
			}
			if !found {
				break
			}
			delete(c.rings, oldestID)
			c.evictedSources.Add(1)
		}
		ring = newSourceTailRing(sourceID, c.linesPerSource)
		c.rings[sourceID] = ring
	}
	c.mu.Unlock()

	return ring.Push(entry)
}

// QueryTail retrieves filtered entries from RAM in newest-first order without touching disk.
func (c *TailCache) QueryTail(q TailQuery) TailResult {
	c.mu.RLock()
	ring := c.rings[q.SourceID]
	c.mu.RUnlock()

	if ring == nil {
		return TailResult{
			Entries:        nil,
			HasMoreOlder:   false,
			OldestBuffered: 0,
		}
	}

	ring.mu.RLock()
	defer ring.mu.RUnlock()
	return ring.queryTailLocked(q)
}

// QueryContext retrieves up to `before` lines and `after` lines surrounding targetTS/targetSeq in chronological order.
func (c *TailCache) QueryContext(sourceID int64, targetTS int64, targetSeq uint64, before, after int) ([]TailEntry, bool) {
	c.mu.RLock()
	ring := c.rings[sourceID]
	c.mu.RUnlock()

	if ring == nil {
		return nil, false
	}

	ring.mu.RLock()
	defer ring.mu.RUnlock()
	return ring.queryContextLocked(targetTS, targetSeq, before, after)
}

// ActiveSources returns the count of currently registered source rings in memory.
func (c *TailCache) ActiveSources() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.rings)
}

// EvictedSources returns the cumulative count of evicted source rings.
func (c *TailCache) EvictedSources() uint64 {
	return c.evictedSources.Load()
}

// Clear resets all in-memory rings.
func (c *TailCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rings = make(map[int64]*SourceTailRing)
}
