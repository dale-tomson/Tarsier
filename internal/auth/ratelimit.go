package auth

import (
	"sync"
	"time"
)

const (
	// DefaultMaxAttempts is the default consecutive failed logins before an IP is locked out.
	DefaultMaxAttempts = 5

	// DefaultLockout is the default lockout duration once max attempts are reached.
	DefaultLockout = 15 * time.Minute

	// MaxTrackedIPs is the strict upper bound on tracked IPs in memory to protect against memory exhaustion.
	MaxTrackedIPs = 4096
)

type ipState struct {
	failures    int
	lockedUntil time.Time
	lastAttempt time.Time
}

// RateLimiter tracks per-IP login failure counts and enforces lockout durations
// with bounded memory capacity.
type RateLimiter struct {
	maxAttempts int
	lockout     time.Duration
	maxTracked  int
	clock       func() time.Time

	mu      sync.Mutex
	entries map[string]*ipState
}

// RateLimiterConfig configures RateLimiter.
type RateLimiterConfig struct {
	MaxAttempts int
	Lockout     time.Duration
	MaxTracked  int
	Clock       func() time.Time
}

// NewRateLimiter creates a thread-safe RateLimiter with bounded memory capacity.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	lockout := cfg.Lockout
	if lockout <= 0 {
		lockout = DefaultLockout
	}

	maxTracked := cfg.MaxTracked
	if maxTracked <= 0 {
		maxTracked = MaxTrackedIPs
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	return &RateLimiter{
		maxAttempts: maxAttempts,
		lockout:     lockout,
		maxTracked:  maxTracked,
		clock:       clock,
		entries:     make(map[string]*ipState),
	}
}

// SetClock updates the internal clock source (primarily for deterministic unit testing).
func (rl *RateLimiter) SetClock(clock func() time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.clock = clock
}

// Check inspects whether ip is currently locked out.
// Returns (true, 0) if allowed, or (false, retryAfter) if currently locked out.
func (rl *RateLimiter) Check(ip string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock()
	entry, ok := rl.entries[ip]
	if !ok {
		return true, 0
	}

	// Check if still locked out
	if now.Before(entry.lockedUntil) {
		remaining := entry.lockedUntil.Sub(now)
		return false, remaining
	}

	// If lockout expired and idle beyond lockout window, reset failures
	if !entry.lockedUntil.IsZero() && now.Sub(entry.lockedUntil) > 0 {
		entry.failures = 0
		entry.lockedUntil = time.Time{}
	}

	return true, 0
}

// RecordFailure increments the failure count for ip and locks it out if maxAttempts is reached.
func (rl *RateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock()
	entry, exists := rl.entries[ip]
	if !exists {
		// Enforce memory bounds before inserting new entry
		if len(rl.entries) >= rl.maxTracked {
			rl.evictLocked(now)
		}

		entry = &ipState{}
		rl.entries[ip] = entry
	}

	entry.lastAttempt = now
	entry.failures++

	if entry.failures >= rl.maxAttempts {
		entry.lockedUntil = now.Add(rl.lockout)
	}
}

// RecordSuccess clears failure state for ip upon successful authentication.
func (rl *RateLimiter) RecordSuccess(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	delete(rl.entries, ip)
}

// Len returns the number of currently tracked IPs (useful in tests).
func (rl *RateLimiter) Len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.entries)
}

// evictLocked purges expired entries; if still at max capacity, evicts the oldest entry.
// Must be called with rl.mu held.
func (rl *RateLimiter) evictLocked(now time.Time) {
	// First pass: purge all expired entries that are no longer locked out
	for ip, entry := range rl.entries {
		if now.After(entry.lockedUntil) && now.Sub(entry.lastAttempt) > rl.lockout {
			delete(rl.entries, ip)
		}
	}

	// If still at capacity, evict the entry with the oldest lastAttempt
	if len(rl.entries) >= rl.maxTracked {
		var oldestIP string
		var oldestTime time.Time

		for ip, entry := range rl.entries {
			if oldestTime.IsZero() || entry.lastAttempt.Before(oldestTime) {
				oldestTime = entry.lastAttempt
				oldestIP = ip
			}
		}

		if oldestIP != "" {
			delete(rl.entries, oldestIP)
		}
	}
}
