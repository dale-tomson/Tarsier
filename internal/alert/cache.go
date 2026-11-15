package alert

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// RuleSet represents an immutable snapshot of all active compiled alert rules.
type RuleSet struct {
	Version        uint64
	All            []*CompiledRule
	ByID           map[int64]*CompiledRule
	MetricRules    []*CompiledRule // Enabled only
	LogRules       []*CompiledRule // Enabled only
	ContainerRules []*CompiledRule // Enabled only
}

// RuleCacheOption provides configuration options for RuleCache.
type RuleCacheOption func(*RuleCache)

// WithOnSwap sets a callback invoked whenever a new RuleSet snapshot is atomically installed.
func WithOnSwap(fn func(oldSet, newSet *RuleSet)) RuleCacheOption {
	return func(c *RuleCache) {
		c.onSwap = fn
	}
}

// RuleCache manages an atomic in-memory snapshot of compiled alert rules.
type RuleCache struct {
	store    *RuleStore
	current  atomic.Pointer[RuleSet]
	reloadMu sync.Mutex
	onSwap   func(oldSet, newSet *RuleSet)
}

// NewRuleCache creates a new RuleCache initialized with an empty RuleSet snapshot.
func NewRuleCache(store *RuleStore, opts ...RuleCacheOption) *RuleCache {
	c := &RuleCache{
		store: store,
	}

	for _, opt := range opts {
		opt(c)
	}

	emptySet := &RuleSet{
		Version:        0,
		All:            []*CompiledRule{},
		ByID:           make(map[int64]*CompiledRule),
		MetricRules:    []*CompiledRule{},
		LogRules:       []*CompiledRule{},
		ContainerRules: []*CompiledRule{},
	}
	c.current.Store(emptySet)

	return c
}

// SetOnSwap sets or replaces the callback invoked when a new RuleSet is installed.
func (c *RuleCache) SetOnSwap(fn func(oldSet, newSet *RuleSet)) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	c.onSwap = fn
}

// Snapshot returns the current immutable RuleSet in a single lock-free atomic load.
func (c *RuleCache) Snapshot() *RuleSet {
	return c.current.Load()
}

// Reload reads all rules from config.db, compiles them, and atomically swaps the active snapshot.
func (c *RuleCache) Reload(ctx context.Context) error {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	rawRules, err := c.store.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("list rules from store: %w", err)
	}

	oldSet := c.current.Load()
	newVersion := uint64(1)
	if oldSet != nil {
		newVersion = oldSet.Version + 1
	}

	newSet := &RuleSet{
		Version:        newVersion,
		All:            make([]*CompiledRule, 0, len(rawRules)),
		ByID:           make(map[int64]*CompiledRule, len(rawRules)),
		MetricRules:    make([]*CompiledRule, 0),
		LogRules:       make([]*CompiledRule, 0),
		ContainerRules: make([]*CompiledRule, 0),
	}

	for _, raw := range rawRules {
		compiled, compErr := CompileRule(raw)
		if compErr != nil {
			// Skip rules that fail compilation (e.g. corrupted in DB) but log/record error
			continue
		}

		newSet.All = append(newSet.All, compiled)
		newSet.ByID[compiled.ID] = compiled

		if compiled.Enabled {
			switch compiled.Kind {
			case "metric":
				newSet.MetricRules = append(newSet.MetricRules, compiled)
			case "log":
				newSet.LogRules = append(newSet.LogRules, compiled)
			case "container":
				newSet.ContainerRules = append(newSet.ContainerRules, compiled)
			}
		}
	}

	c.current.Store(newSet)

	if c.onSwap != nil {
		c.onSwap(oldSet, newSet)
	}

	return nil
}
