package alert

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// EvalObservation represents a point-in-time evaluation signal passed to the Alert State Machine.
type EvalObservation struct {
	Rule     *CompiledRule
	Target   string            // Concrete resolved target e.g. "host", "disk:/mnt/data", "container:api"
	Breached bool              // true if condition is currently violated; false if healthy/resolved
	Value    float64           // Observed metric value or event count
	Summary  string            // Concise description e.g. "host cpu_pct is 94.2% (> 90.0% for 5m)"
	Details  map[string]string // Structured key/value context for notifications
	At       time.Time
}

// ObservationSink defines the interface for consuming evaluation observations.
type ObservationSink interface {
	Observe(ctx context.Context, obs EvalObservation)
}

type metricTargetKey struct {
	ruleID int64
	target string
}

// MetricEvaluator evaluates active metric threshold rules against metrics.HotCache on a periodic 10s ticker.
type MetricEvaluator struct {
	cache       *RuleCache
	hotCache    *metrics.HotCache
	sink        ObservationSink
	clock       model.Clock
	mu          sync.Mutex
	activeBreach map[metricTargetKey]bool // tracks rules/targets currently in breached state
}

// NewMetricEvaluator initializes a MetricEvaluator.
func NewMetricEvaluator(cache *RuleCache, hot *metrics.HotCache, sink ObservationSink, clock model.Clock) *MetricEvaluator {
	if clock == nil {
		clock = model.SystemClock{}
	}
	return &MetricEvaluator{
		cache:        cache,
		hotCache:     hot,
		sink:         sink,
		clock:        clock,
		activeBreach: make(map[metricTargetKey]bool),
	}
}

// Run executes EvalOnce periodically every 10 seconds until ctx is canceled.
func (e *MetricEvaluator) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Initial immediate evaluation
	e.EvalOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.EvalOnce(ctx)
		}
	}
}

// EvalOnce executes a single evaluation pass across all active metric rules.
func (e *MetricEvaluator) EvalOnce(ctx context.Context) {
	if e.cache == nil || e.hotCache == nil || e.sink == nil {
		return
	}

	snap := e.cache.Snapshot()
	if snap == nil || len(snap.MetricRules) == 0 {
		return
	}

	now := e.clock.Now().UTC()
	currentTickTargets := make(map[metricTargetKey]bool)

	for _, rule := range snap.MetricRules {
		if rule.MetricCond == nil {
			continue
		}
		cond := rule.MetricCond
		targets := e.resolveTargets(rule.Target, cond.Metric)

		for _, target := range targets {
			key := metricTargetKey{ruleID: rule.ID, target: target}
			currentTickTargets[key] = true

			breached, val, summary, details, ok := e.evaluateTarget(rule, target, now)
			if !ok {
				// No recent sample or insufficient points
				continue
			}

			e.mu.Lock()
			wasBreached := e.activeBreach[key]
			if breached {
				e.activeBreach[key] = true
			} else {
				delete(e.activeBreach, key)
			}
			e.mu.Unlock()

			// Emit if breached, or if recovering from a previous breach
			if breached || wasBreached {
				e.sink.Observe(ctx, EvalObservation{
					Rule:     rule,
					Target:   target,
					Breached: breached,
					Value:    val,
					Summary:  summary,
					Details:  details,
					At:       now,
				})
			}
		}
	}

	// Automatic recovery for disappeared targets (e.g. unmounted disk)
	e.mu.Lock()
	for key := range e.activeBreach {
		if !currentTickTargets[key] {
			delete(e.activeBreach, key)
			// Target disappeared; emit recovery observation
			rule := snap.ByID[key.ruleID]
			if rule != nil {
				e.sink.Observe(ctx, EvalObservation{
					Rule:     rule,
					Target:   key.target,
					Breached: false,
					Value:    0,
					Summary:  fmt.Sprintf("%s recovered (target no longer active)", key.target),
					Details: map[string]string{
						"status": "target inactive",
					},
					At: now,
				})
			}
		}
	}
	e.mu.Unlock()
}

func (e *MetricEvaluator) resolveTargets(ruleTarget, metric string) []string {
	if ruleTarget == "host" {
		return []string{"host"}
	}

	activeEntities := e.hotCache.EntitiesWithMetric(metric)

	// If wildcard or prefix target
	if ruleTarget == "*" {
		sort.Strings(activeEntities)
		return activeEntities
	}

	if strings.HasSuffix(ruleTarget, ":*") {
		prefix := strings.TrimSuffix(ruleTarget, "*")
		var matched []string
		for _, ent := range activeEntities {
			if strings.HasPrefix(ent, prefix) {
				matched = append(matched, ent)
			}
		}
		sort.Strings(matched)
		return matched
	}

	// Exact target
	return []string{ruleTarget}
}

func (e *MetricEvaluator) evaluateTarget(rule *CompiledRule, target string, now time.Time) (bool, float64, string, map[string]string, bool) {
	cond := rule.MetricCond

	if rule.ForSec <= 0 {
		// Instant evaluation: fetch newest point
		pt, ok := e.hotCache.GetLatest(target, cond.Metric)
		if !ok || (now.Unix()-pt.TS > 60) {
			// Skip if no point or point is older than 60s
			return false, 0, "", nil, false
		}

		breached := compareFloat(pt.Value, cond.Operator, cond.Threshold)
		summary := fmt.Sprintf("%s %s is %.1f (%s %.1f)", target, cond.Metric, pt.Value, cond.Operator, cond.Threshold)
		details := map[string]string{
			"target":    target,
			"metric":    cond.Metric,
			"value":     fmt.Sprintf("%.1f", pt.Value),
			"threshold": fmt.Sprintf("%.1f", cond.Threshold),
			"operator":  cond.Operator,
		}
		return breached, pt.Value, summary, details, true
	}

	// Sustained evaluation window
	sinceTS := now.Unix() - int64(rule.ForSec)
	stats, ok := e.hotCache.EvalWindow(target, cond.Metric, sinceTS)
	if !ok {
		return false, 0, "", nil, false
	}

	// Require minimum sample count spanning the window
	minCount := rule.ForSec / 15
	if minCount < 2 {
		minCount = 2
	}
	if stats.Count < minCount {
		// Window not sufficiently populated yet
		return false, stats.Avg, "", nil, false
	}

	var breached bool
	switch cond.Operator {
	case ">", ">=":
		// All samples in window violated threshold: Min > Threshold
		breached = compareFloat(stats.Min, cond.Operator, cond.Threshold)
	case "<", "<=":
		// All samples in window violated threshold: Max < Threshold
		breached = compareFloat(stats.Max, cond.Operator, cond.Threshold)
	case "==":
		breached = stats.Min == cond.Threshold && stats.Max == cond.Threshold
	default:
		breached = false
	}

	summary := fmt.Sprintf("%s %s is %.1f (%s %.1f for %s)",
		target, cond.Metric, stats.Avg, cond.Operator, cond.Threshold, formatDuration(rule.ForSec))

	details := map[string]string{
		"target":    target,
		"metric":    cond.Metric,
		"avg":       fmt.Sprintf("%.1f", stats.Avg),
		"min":       fmt.Sprintf("%.1f", stats.Min),
		"max":       fmt.Sprintf("%.1f", stats.Max),
		"samples":   fmt.Sprintf("%d", stats.Count),
		"threshold": fmt.Sprintf("%.1f", cond.Threshold),
		"operator":  cond.Operator,
		"for":       formatDuration(rule.ForSec),
	}

	return breached, stats.Avg, summary, details, true
}

func compareFloat(val float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return val > threshold
	case ">=":
		return val >= threshold
	case "<":
		return val < threshold
	case "<=":
		return val <= threshold
	case "==":
		return val == threshold
	default:
		return false
	}
}

func formatDuration(sec int) string {
	if sec%3600 == 0 && sec >= 3600 {
		return fmt.Sprintf("%dh", sec/3600)
	}
	if sec%60 == 0 && sec >= 60 {
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%ds", sec)
}
