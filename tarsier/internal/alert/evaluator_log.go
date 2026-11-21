package alert

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// SourceResolver resolves a raw source ID into a human-readable source or container name.
type SourceResolver interface {
	ResolveSource(sourceID string) string
}

// SourceResolverFunc allows using a function as a SourceResolver.
type SourceResolverFunc func(sourceID string) string

func (f SourceResolverFunc) ResolveSource(sourceID string) string {
	return f(sourceID)
}

type logWindowKey struct {
	ruleID     int64
	sourceName string
}

type logWindowState struct {
	hits        []int64  // match timestamps in epoch seconds, capped at min(threshold, 10000)
	sampleLines []string // up to 2 recent matching lines truncated to 512 bytes
	breached    bool     // tracks whether this window is currently in breached state
}

// LogEvaluator taps the log ingestion stream to evaluate log pattern alert rules.
type LogEvaluator struct {
	cache    *RuleCache
	resolver SourceResolver
	sink     ObservationSink
	clock    model.Clock
	tapChan  chan model.LogEntry
	dropped  atomic.Uint64
	mu       sync.Mutex
	windows  map[logWindowKey]*logWindowState
}

// NewLogEvaluator initializes a new LogEvaluator with a bounded 2048-capacity tap queue.
func NewLogEvaluator(cache *RuleCache, resolver SourceResolver, sink ObservationSink, clock model.Clock) *LogEvaluator {
	if clock == nil {
		clock = model.SystemClock{}
	}
	return &LogEvaluator{
		cache:    cache,
		resolver: resolver,
		sink:     sink,
		clock:    clock,
		tapChan:  make(chan model.LogEntry, 2048),
		windows:  make(map[logWindowKey]*logWindowState),
	}
}

// Enqueue adds a log entry to the evaluator queue. Non-blocking; drops if queue is saturated.
func (e *LogEvaluator) Enqueue(entry model.LogEntry) {
	select {
	case e.tapChan <- entry:
	default:
		e.dropped.Add(1)
	}
}

// DroppedCount returns the total number of log entries dropped due to full tap queue.
func (e *LogEvaluator) DroppedCount() uint64 {
	return e.dropped.Load()
}

// Run drains incoming log entries and executes periodic 10s sliding-window sweeps.
func (e *LogEvaluator) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-e.tapChan:
			if !ok {
				return
			}
			e.ProcessEntry(ctx, entry)
		case <-ticker.C:
			e.SweepWindows(ctx)
		}
	}
}

// ProcessEntry evaluates a single LogEntry against all active log pattern rules.
func (e *LogEvaluator) ProcessEntry(ctx context.Context, entry model.LogEntry) {
	if e.cache == nil || e.sink == nil {
		return
	}

	snap := e.cache.Snapshot()
	if snap == nil || len(snap.LogRules) == 0 {
		return
	}

	sourceName := entry.SourceID
	if e.resolver != nil {
		if resolved := e.resolver.ResolveSource(entry.SourceID); resolved != "" {
			sourceName = resolved
		}
	}

	lineBytes := entry.Message
	if len(lineBytes) == 0 {
		lineBytes = entry.Raw
	}
	if len(lineBytes) == 0 {
		return
	}

	evSec := entry.Timestamp.UTC().Unix()
	if evSec <= 0 {
		evSec = e.clock.Now().UTC().Unix()
	}
	evTime := time.Unix(evSec, 0).UTC()

	for _, rule := range snap.LogRules {
		if rule.LogCond == nil {
			continue
		}

		// 1. Target check
		if !rule.MatchesTarget(sourceName) {
			continue
		}

		// 2. Minimum level gate
		if rule.LogCond.MinLevel > 0 && uint8(entry.Level) < rule.LogCond.MinLevel {
			continue
		}

		// 3. Pattern match
		if !rule.MatchLogLine(lineBytes) {
			continue
		}

		// 4. Update sliding-window state
		key := logWindowKey{ruleID: rule.ID, sourceName: sourceName}
		cond := rule.LogCond

		e.mu.Lock()
		st, exists := e.windows[key]
		if !exists {
			st = &logWindowState{
				hits:        make([]int64, 0, cond.Threshold),
				sampleLines: make([]string, 0, 2),
			}
			e.windows[key] = st
		}

		// Append timestamp (capped at threshold, max 10000)
		capLimit := cond.Threshold
		if capLimit > 10000 {
			capLimit = 10000
		}
		if len(st.hits) >= capLimit {
			st.hits = st.hits[1:]
		}
		st.hits = append(st.hits, evSec)

		// Record sample line (truncated to 512 bytes)
		sample := string(lineBytes)
		if len(sample) > 512 {
			sample = sample[:512]
		}
		if len(st.sampleLines) < 2 {
			st.sampleLines = append(st.sampleLines, sample)
		} else {
			st.sampleLines[0] = st.sampleLines[1]
			st.sampleLines[1] = sample
		}

		// Prune hits outside window
		cutoff := evSec - int64(cond.WindowSec)
		validCount := 0
		var pruned []int64
		for _, ts := range st.hits {
			if ts >= cutoff {
				validCount++
				pruned = append(pruned, ts)
			}
		}
		st.hits = pruned

		shouldFire := validCount >= cond.Threshold
		wasBreached := st.breached
		if shouldFire {
			st.breached = true
		}
		samplesCopy := make([]string, len(st.sampleLines))
		copy(samplesCopy, st.sampleLines)
		e.mu.Unlock()

		if shouldFire && !wasBreached {
			e.sink.Observe(ctx, buildLogBreachObservation(rule, sourceName, validCount, samplesCopy, evTime))
		}
	}
}

func buildLogBreachObservation(rule *CompiledRule, sourceName string, validCount int, samples []string, at time.Time) EvalObservation {
	cond := rule.LogCond
	summary := fmt.Sprintf("source %s matched pattern %q %d times in %ds",
		sourceName, cond.Pattern, validCount, cond.WindowSec)

	details := map[string]string{
		"source":    sourceName,
		"pattern":   cond.Pattern,
		"matches":   strconv.Itoa(validCount),
		"threshold": strconv.Itoa(cond.Threshold),
		"window":    fmt.Sprintf("%ds", cond.WindowSec),
	}
	for i, s := range samples {
		details[fmt.Sprintf("sample_%d", i+1)] = s
	}

	return EvalObservation{
		Rule:     rule,
		Target:   sourceName,
		Breached: true,
		Value:    float64(validCount),
		Summary:  summary,
		Details:  details,
		At:       at,
	}
}

// SweepWindows scans tracked sliding-window states, evicts expired hits,
// emits ongoing breach observations for sustained duration tracking, and emits recovery observations.
func (e *LogEvaluator) SweepWindows(ctx context.Context) {
	if e.cache == nil || e.sink == nil {
		return
	}

	snap := e.cache.Snapshot()
	if snap == nil {
		return
	}

	nowSec := e.clock.Now().UTC().Unix()
	nowTime := time.Unix(nowSec, 0).UTC()

	var observations []EvalObservation

	e.mu.Lock()
	for key, st := range e.windows {
		rule := snap.ByID[key.ruleID]
		if rule == nil || !rule.Enabled || rule.LogCond == nil {
			delete(e.windows, key)
			continue
		}

		cond := rule.LogCond
		cutoff := nowSec - int64(cond.WindowSec)
		validCount := 0
		var pruned []int64
		for _, ts := range st.hits {
			if ts >= cutoff {
				validCount++
				pruned = append(pruned, ts)
			}
		}
		st.hits = pruned

		if st.breached && validCount >= cond.Threshold {
			samplesCopy := make([]string, len(st.sampleLines))
			copy(samplesCopy, st.sampleLines)
			observations = append(observations, buildLogBreachObservation(rule, key.sourceName, validCount, samplesCopy, nowTime))
		} else if st.breached && validCount < cond.Threshold {
			st.breached = false
			st.sampleLines = nil

			observations = append(observations, EvalObservation{
				Rule:     rule,
				Target:   key.sourceName,
				Breached: false,
				Value:    float64(validCount),
				Summary:  fmt.Sprintf("source %s log pattern rate returned below threshold (%d matches in %ds)", key.sourceName, validCount, cond.WindowSec),
				Details: map[string]string{
					"source":    key.sourceName,
					"pattern":   cond.Pattern,
					"matches":   strconv.Itoa(validCount),
					"threshold": strconv.Itoa(cond.Threshold),
					"window":    fmt.Sprintf("%ds", cond.WindowSec),
				},
				At: nowTime,
			})
		}

		if len(st.hits) == 0 && !st.breached {
			delete(e.windows, key)
		}
	}
	e.mu.Unlock()

	for _, obs := range observations {
		e.sink.Observe(ctx, obs)
	}
}

// PurgeRule removes window tracking state for a specific rule ID.
func (e *LogEvaluator) PurgeRule(ruleID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for k := range e.windows {
		if k.ruleID == ruleID {
			delete(e.windows, k)
		}
	}
}
