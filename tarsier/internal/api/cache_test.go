package api

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQueryCache_NoSliceLeakUnderContinuousPolling(t *testing.T) {
	const maxSize = 16
	c := NewQueryCache(maxSize, time.Millisecond)
	// Force immediate expiration on every access without sleeping 20,000 times.
	c.ttl = -time.Millisecond

	keys := []string{"poll-metrics", "poll-containers", "poll-alerts"}
	payload := []byte(`{"ok":true}`)

	for i := 0; i < 20000; i++ {
		key := keys[i%len(keys)]
		got, err := c.GetOrCompute(key, func() ([]byte, error) {
			return payload, nil
		})
		if err != nil {
			t.Fatalf("iteration %d: GetOrCompute failed: %v", i, err)
		}
		if len(got) == 0 {
			t.Fatalf("iteration %d: expected non-empty payload", i)
		}

		c.mu.Lock()
		orderLen := len(c.order)
		orderCap := cap(c.order)
		entriesLen := len(c.entries)
		c.mu.Unlock()

		if orderLen > 2*c.maxSize {
			t.Fatalf("iteration %d: len(c.order)=%d exceeded 2*maxSize (%d)", i, orderLen, 2*c.maxSize)
		}
		if orderCap > 2*c.maxSize {
			t.Fatalf("iteration %d: cap(c.order)=%d exceeded 2*maxSize (%d)", i, orderCap, 2*c.maxSize)
		}
		if orderLen != entriesLen {
			t.Fatalf("iteration %d: len(c.order)=%d != len(c.entries)=%d", i, orderLen, entriesLen)
		}
	}
}

func TestQueryCache_EvictionZeroingAndCapacity(t *testing.T) {
	const maxSize = 32
	c := NewQueryCache(maxSize, time.Minute)

	// Fill to capacity first so we can observe the backing array slot at index 0.
	for i := 0; i < maxSize; i++ {
		key := fmt.Sprintf("key-%04d", i)
		_, err := c.GetOrCompute(key, func() ([]byte, error) {
			return []byte(key), nil
		})
		if err != nil {
			t.Fatalf("GetOrCompute(%s) failed: %v", key, err)
		}
	}

	c.mu.Lock()
	headView := c.order[:1]
	c.mu.Unlock()

	// Insert one more key to trigger eviction of c.order[0].
	_, err := c.GetOrCompute("key-evict-trigger", func() ([]byte, error) {
		return []byte("trigger"), nil
	})
	if err != nil {
		t.Fatalf("GetOrCompute(trigger) failed: %v", err)
	}

	if headView[0] != "" {
		t.Fatalf("expected evicted c.order[0] slot to be zeroed, got %q", headView[0])
	}

	// Continue inserting up to 2,000 unique keys.
	for i := maxSize; i < 2000; i++ {
		key := fmt.Sprintf("unique-key-%04d", i)
		_, err := c.GetOrCompute(key, func() ([]byte, error) {
			return []byte(key), nil
		})
		if err != nil {
			t.Fatalf("GetOrCompute(%s) failed: %v", key, err)
		}
	}

	if got := c.Len(); got > c.maxSize {
		t.Fatalf("c.Len()=%d exceeded c.maxSize=%d", got, c.maxSize)
	}

	c.mu.Lock()
	orderLen := len(c.order)
	orderCap := cap(c.order)
	c.mu.Unlock()

	if orderLen > c.maxSize {
		t.Fatalf("len(c.order)=%d exceeded c.maxSize=%d", orderLen, c.maxSize)
	}
	if orderCap > 2*c.maxSize {
		t.Fatalf("cap(c.order)=%d exceeded 2*c.maxSize=%d", orderCap, 2*c.maxSize)
	}
}

func TestQueryCache_ConcurrentGetOrComputeNoRace(t *testing.T) {
	c := NewQueryCache(32, 2*time.Millisecond)

	const workers = 12
	const iters = 300

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("shared-%d", (workerID+i)%10)
				if i%15 == 0 {
					key = fmt.Sprintf("unique-%d-%d", workerID, i)
				}
				val, err := c.GetOrCompute(key, func() ([]byte, error) {
					return []byte(key), nil
				})
				if err != nil {
					t.Errorf("GetOrCompute(%s) failed: %v", key, err)
					return
				}
				if len(val) == 0 {
					t.Errorf("GetOrCompute(%s) returned empty slice", key)
					return
				}
				if i%50 == 0 {
					_ = c.Len()
				}
				if i%120 == 0 {
					c.Invalidate()
				}
			}
		}(w)
	}
	wg.Wait()

	if got := c.Len(); got > c.maxSize {
		t.Fatalf("c.Len()=%d exceeded c.maxSize=%d", got, c.maxSize)
	}

	c.mu.Lock()
	orderLen := len(c.order)
	orderCap := cap(c.order)
	c.mu.Unlock()

	if orderLen > c.maxSize {
		t.Fatalf("len(c.order)=%d exceeded c.maxSize=%d", orderLen, c.maxSize)
	}
	if orderCap > 2*c.maxSize {
		t.Fatalf("cap(c.order)=%d exceeded 2*c.maxSize=%d", orderCap, 2*c.maxSize)
	}
}

func TestQueryCache_HeapAllocBound(t *testing.T) {
	const maxSize = 32
	c := NewQueryCache(maxSize, time.Millisecond)
	c.ttl = -time.Millisecond

	keyPad := strings.Repeat("k", 256)

	// Warm up before measuring baseline heap.
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("warmup-%d-%s", i%8, keyPad)
		_, _ = c.GetOrCompute(key, func() ([]byte, error) {
			return make([]byte, 4096), nil
		})
	}

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("query-%d-%s", i%16, keyPad)
		_, err := c.GetOrCompute(key, func() ([]byte, error) {
			buf := make([]byte, 4096)
			buf[0] = byte(i)
			return buf, nil
		})
		if err != nil {
			t.Fatalf("iteration %d: GetOrCompute failed: %v", i, err)
		}
	}

	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	const maxHeapGrowthBytes = 4 * 1024 * 1024 // 4 MiB epsilon
	if after.HeapAlloc > before.HeapAlloc+maxHeapGrowthBytes {
		t.Fatalf("heap grew excessively across 10,000 expired queries: before=%d after=%d delta=%d",
			before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
	}

	c.mu.Lock()
	orderLen := len(c.order)
	orderCap := cap(c.order)
	c.mu.Unlock()

	if orderLen > c.maxSize || orderCap > 2*c.maxSize {
		t.Fatalf("unexpected order slice dimensions: len=%d cap=%d maxSize=%d", orderLen, orderCap, c.maxSize)
	}
}
