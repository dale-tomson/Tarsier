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
	cache        *RuleCache
	hotCache     *metrics.HotCache
	sink         ObservationSink
	clock        model.Clock
	mu           sync.Mutex
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

// NormalizeMetricName normalizes user-provided or legacy UI metric names to internal metric series names.
func NormalizeMetricName(metric string) string {
	m := strings.TrimSpace(metric)
	switch m {
	case "container_cpu_pct", "host_cpu_pct":
		return "cpu_pct"
	case "container_mem_pct":
		return "mem_pct"
	case "host_mem_used_pct":
		return "mem_used_pct"
	case "host_disk_used_pct":
		return "disk_used_pct"
	default:
		return m
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
		targets := e.resolveTargets(rule, cond.Metric)

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

func (e *MetricEvaluator) resolveTargets(rule *CompiledRule, rawMetric string) []string {
	metric := NormalizeMetricName(rawMetric)
	if rule.Target == "host" {
		return []string{"host"}
	}

	activeEntities := e.hotCache.EntitiesWithMetric(metric)
	if len(activeEntities) == 0 && metric != rawMetric {
		activeEntities = e.hotCache.EntitiesWithMetric(rawMetric)
	}

	// If wildcard
	if rule.Target == "*" {
		sort.Strings(activeEntities)
		return activeEntities
	}

	var matched []string
	for _, ent := range activeEntities {
		if rule.MatchesTarget(ent) {
			matched = append(matched, ent)
		}
	}
	sort.Strings(matched)
	if len(matched) > 0 {
		return matched
	}

	// Exact target fallback if not currently in active entities
	if strings.HasPrefix(rule.Target, "container:") || strings.HasPrefix(rule.Target, "disk:") || rule.Target == "*" {
		return []string{rule.Target}
	}
	return []string{"container:" + rule.Target}
}

func (e *MetricEvaluator) evaluateTarget(rule *CompiledRule, target string, now time.Time) (bool, float64, string, map[string]string, bool) {
	cond := rule.MetricCond
	metric := NormalizeMetricName(cond.Metric)

	// Point-in-time evaluation: fetch newest point; StateMachine.Observe handles ForSec duration holds.
	pt, ok := e.hotCache.GetLatest(target, metric)
	if !ok && metric != cond.Metric {
		pt, ok = e.hotCache.GetLatest(target, cond.Metric)
	}
	if !ok || (now.Unix()-pt.TS > 60) {
		// Skip if no point or point is older than 60s
		return false, 0, "", nil, false
	}

	breached := compareFloat(pt.Value, cond.Operator, cond.Threshold)

	var summary string
	if rule.ForSec > 0 {
		summary = fmt.Sprintf("%s %s is %.1f (%s %.1f for %s)",
			target, metric, pt.Value, cond.Operator, cond.Threshold, FormatDuration(rule.ForSec))
	} else {
		summary = fmt.Sprintf("%s %s is %.1f (%s %.1f)",
			target, metric, pt.Value, cond.Operator, cond.Threshold)
	}

	details := map[string]string{
		"target":    target,
		"metric":    metric,
		"value":     fmt.Sprintf("%.1f", pt.Value),
		"threshold": fmt.Sprintf("%.1f", cond.Threshold),
		"operator":  cond.Operator,
	}
	if rule.ForSec > 0 {
		details["for"] = FormatDuration(rule.ForSec)
	}

	return breached, pt.Value, summary, details, true
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

// FormatDuration formats a duration in seconds into a compact human-readable label.
func FormatDuration(sec int) string {
	if sec%3600 == 0 && sec >= 3600 {
		return fmt.Sprintf("%dh", sec/3600)
	}
	if sec%60 == 0 && sec >= 60 {
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%ds", sec)
}
