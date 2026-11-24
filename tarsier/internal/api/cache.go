package api

import (
	"sync"
	"time"
)

type sfCall struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

type singleflightGroup struct {
	mu sync.Mutex
	m  map[string]*sfCall
}

func (g *singleflightGroup) Do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*sfCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(sfCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}

type cacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// QueryCache provides bounded TTL caching and singleflight coalescing for expensive database queries.
type QueryCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	order   []string
	maxSize int
	ttl     time.Duration
	sf      singleflightGroup
}

// NewQueryCache creates a QueryCache with an upper entry bound and item TTL.
func NewQueryCache(maxSize int, ttl time.Duration) *QueryCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	if ttl <= 0 {
		ttl = 3 * time.Second
	}
	return &QueryCache{
		entries: make(map[string]cacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// GetOrCompute returns cached JSON bytes or computes them via computeFn, coalescing concurrent requests.
func (c *QueryCache) GetOrCompute(key string, computeFn func() ([]byte, error)) ([]byte, error) {
	now := time.Now()

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if now.Before(entry.expiresAt) {
			data := entry.data
			c.mu.Unlock()
			return data, nil
		}
		// Expired
		delete(c.entries, key)
		c.removeOrderKey(key)
	}
	c.mu.Unlock()

	// Singleflight deduplication
	return c.sf.Do(key, func() ([]byte, error) {
		// Double-check cache inside singleflight
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok {
			if time.Now().Before(entry.expiresAt) {
				data := entry.data
				c.mu.Unlock()
				return data, nil
			}
			delete(c.entries, key)
			c.removeOrderKey(key)
		}
		c.mu.Unlock()

		data, err := computeFn()
		if err != nil {
			return nil, err
		}

		c.mu.Lock()
		if _, exists := c.entries[key]; exists {
			delete(c.entries, key)
			c.removeOrderKey(key)
		}

		// Evict oldest if capacity reached
		for len(c.entries) >= c.maxSize && len(c.order) > 0 {
			oldestKey := c.order[0]
			c.order[0] = ""
			c.order = c.order[1:]
			delete(c.entries, oldestKey)
		}
		c.maybeCompactOrder()

		c.entries[key] = cacheEntry{
			data:      data,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.order = append(c.order, key)
		c.mu.Unlock()

		return data, nil
	})
}

// removeOrderKey deletes a single key from c.order while preserving slice element zeroing.
// Caller must hold c.mu.
func (c *QueryCache) removeOrderKey(key string) {
	for i, k := range c.order {
		if k == key {
			copy(c.order[i:], c.order[i+1:])
			c.order[len(c.order)-1] = ""
			c.order = c.order[:len(c.order)-1]
			break
		}
	}
	c.maybeCompactOrder()
}

// maybeCompactOrder shrinks an oversized backing array when active length drops well below capacity.
// Caller must hold c.mu.
func (c *QueryCache) maybeCompactOrder() {
	if cap(c.order) > c.maxSize && len(c.order) <= c.maxSize/2 {
		compacted := make([]string, len(c.order), c.maxSize)
		copy(compacted, c.order)
		c.order = compacted
	}
}

// Invalidate clears all entries from the query cache.
func (c *QueryCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]cacheEntry, c.maxSize)
	c.order = make([]string, 0, c.maxSize)
}

// Len returns the current number of entries in the cache.
func (c *QueryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
