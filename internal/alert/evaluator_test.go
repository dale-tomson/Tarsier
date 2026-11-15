package alert

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/docker"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// fakeClock provides a controllable clock for tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// recordingSink records emitted observations for assertions.
type recordingSink struct {
	mu           sync.Mutex
	observations []EvalObservation
}

func (s *recordingSink) Observe(ctx context.Context, obs EvalObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, obs)
}

func (s *recordingSink) Observations() []EvalObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EvalObservation, len(s.observations))
	copy(out, s.observations)
	return out
}

func (s *recordingSink) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = nil
}

func TestMetricEvaluator_ThresholdAndForDuration(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// 1. Instant rule: cpu_pct > 85%, for_sec = 0
	rInstant, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "host-cpu-instant",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "warning",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":85.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create instant rule: %v", err)
	}

	// 2. Sustained rule: cpu_pct > 85%, for_sec = 30s
	rSustained, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "host-cpu-sustained",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":85.0}`,
		ForSec:        30,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create sustained rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	if err := ruleCache.Reload(ctx); err != nil {
		t.Fatalf("cache.Reload: %v", err)
	}

	hotCache := metrics.NewHotCache(100, 180)
	startTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)
	sink := &recordingSink{}

	evaluator := NewMetricEvaluator(ruleCache, hotCache, sink, clock)

	// Step 1: t=0, push 70% (healthy)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 70.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	obs := sink.Observations()
	for _, o := range obs {
		if o.Breached {
			t.Errorf("t=0: unexpected breach: %+v", o)
		}
	}
	sink.Clear()

	// Step 2: t=10s, push 92% (breach starts)
	clock.Advance(10 * time.Second)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 92.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	obs = sink.Observations()
	var instantFired, sustainedFired bool
	for _, o := range obs {
		if o.Rule.ID == rInstant.ID && o.Breached {
			instantFired = true
		}
		if o.Rule.ID == rSustained.ID && o.Breached {
			sustainedFired = true
		}
	}
	if !instantFired {
		t.Error("t=10s: expected host-cpu-instant to breach")
	}
	if sustainedFired {
		t.Error("t=10s: host-cpu-sustained should NOT breach yet (only 1 sample, window 30s not met)")
	}
	sink.Clear()

	// Step 3: t=20s (91%), t=30s (94%), t=40s (93%) -> sustained 30s window satisfied at t=40s
	clock.Advance(10 * time.Second) // t=20s
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 91.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)
	sink.Clear()

	clock.Advance(10 * time.Second) // t=30s
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 94.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	// At t=30s, breach duration is 20s (from t=10s to t=30s), so sustained 30s must NOT fire yet
	for _, o := range sink.Observations() {
		if o.Rule.ID == rSustained.ID && o.Breached {
			t.Errorf("t=30s: host-cpu-sustained should not fire yet at 20s breach duration: %+v", o)
		}
	}
	sink.Clear()

	clock.Advance(10 * time.Second) // t=40s
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 93.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	obs = sink.Observations()
	sustainedFired = false
	for _, o := range obs {
		if o.Rule.ID == rSustained.ID && o.Breached {
			sustainedFired = true
		}
	}
	if !sustainedFired {
		t.Error("t=40s: expected host-cpu-sustained to breach after full 30s sustained window")
	}
	sink.Clear()

	// Step 4: t=50s, push 45% (recovery)
	clock.Advance(10 * time.Second) // t=50s
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 45.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	obs = sink.Observations()
	var instantRecovered, sustainedRecovered bool
	for _, o := range obs {
		if o.Rule.ID == rInstant.ID && !o.Breached {
			instantRecovered = true
		}
		if o.Rule.ID == rSustained.ID && !o.Breached {
			sustainedRecovered = true
		}
	}
	if !instantRecovered {
		t.Error("t=50s: expected host-cpu-instant to emit recovery observation (Breached: false)")
	}
	if !sustainedRecovered {
		t.Error("t=50s: expected host-cpu-sustained to emit recovery observation (Breached: false)")
	}
}

func TestMetricEvaluator_WildcardDiskAndContainerExpansion(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// disk:* rule
	_, err = ruleStore.CreateRule(ctx, AlertRule{
		Name:          "disk-full-wildcard",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "disk:*",
		ConditionJSON: `{"metric":"disk_used_pct","operator":">=","threshold":90.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)

	hotCache := metrics.NewHotCache(100, 180)
	clock := newFakeClock(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	nowSec := clock.Now().Unix()

	// Seed multiple disk entities in HotCache
	hotCache.Record(
		model.MetricSample{Entity: "disk:/", Metric: "disk_used_pct", Value: 40.0, TS: nowSec},
		model.MetricSample{Entity: "disk:/mnt/data", Metric: "disk_used_pct", Value: 93.4, TS: nowSec},
		model.MetricSample{Entity: "container:api", Metric: "cpu_pct", Value: 12.0, TS: nowSec},
	)

	sink := &recordingSink{}
	evaluator := NewMetricEvaluator(ruleCache, hotCache, sink, clock)

	evaluator.EvalOnce(ctx)

	obs := sink.Observations()
	var breachedTargets []string
	for _, o := range obs {
		if o.Breached {
			breachedTargets = append(breachedTargets, o.Target)
		}
	}

	if len(breachedTargets) != 1 || breachedTargets[0] != "disk:/mnt/data" {
		t.Fatalf("expected only 'disk:/mnt/data' to breach, got %v", breachedTargets)
	}
}

func TestContainerEvaluator_OOMRestartLoopAndRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// 1. Immediate OOM Rule
	rOOM, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Container OOM Killed",
		Kind:          "container",
		Enabled:       true,
		Severity:      "critical",
		Target:        "container:*",
		ConditionJSON: `{"events":["oom"],"threshold":1,"window_sec":0}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create oom rule: %v", err)
	}

	// 2. Restart Loop Rule: threshold 3 restarts in 120s
	rRestart, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Container Restart Loop",
		Kind:          "container",
		Enabled:       true,
		Severity:      "warning",
		Target:        "container:*",
		ConditionJSON: `{"events":["restart","die"],"threshold":3,"window_sec":120}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create restart loop rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)

	clock := newFakeClock(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	sink := &recordingSink{}
	evaluator := NewContainerEvaluator(ruleCache, sink, clock)

	// Part A: Test Instant OOM Trigger + Start Recovery
	evaluator.OnContainerEvent(ctx, docker.ContainerEvent{
		ContainerID:   "c123",
		ContainerName: "api-service",
		Image:         "api:v1",
		Action:        "oom",
		ExitCode:      137,
		TS:            clock.Now().Unix(),
	})

	obs := sink.Observations()
	var oomFired bool
	for _, o := range obs {
		if o.Rule.ID == rOOM.ID && o.Breached && o.Target == "container:api-service" {
			oomFired = true
		}
	}
	if !oomFired {
		t.Fatal("expected OOM event to trigger breach observation")
	}
	sink.Clear()

	// Part B: Test Restart Loop (3 events in 120s)
	t0 := clock.Now().Unix()

	// Event 1 at t=0
	evaluator.OnContainerEvent(ctx, docker.ContainerEvent{
		ContainerID: "c123", ContainerName: "api-service", Action: "die", TS: t0,
	})
	// Event 2 at t=30
	evaluator.OnContainerEvent(ctx, docker.ContainerEvent{
		ContainerID: "c123", ContainerName: "api-service", Action: "restart", TS: t0 + 30,
	})

	// Neither should have fired restart loop yet (count 2 < 3)
	for _, o := range sink.Observations() {
		if o.Rule.ID == rRestart.ID && o.Breached {
			t.Fatalf("restart loop should not breach with 2 events: %+v", o)
		}
	}

	// Event 3 at t=60 (within 120s) -> threshold 3 met!
	evaluator.OnContainerEvent(ctx, docker.ContainerEvent{
		ContainerID: "c123", ContainerName: "api-service", Action: "restart", TS: t0 + 60,
	})

	obs = sink.Observations()
	var restartLoopFired bool
	for _, o := range obs {
		if o.Rule.ID == rRestart.ID && o.Breached && o.Value == 3 {
			restartLoopFired = true
		}
	}
	if !restartLoopFired {
		t.Fatal("expected restart loop to breach on 3rd event")
	}
	sink.Clear()

	// Advance clock past 120s window (t=190s) and call SweepWindows
	clock.Advance(130 * time.Second)
	evaluator.SweepWindows(ctx)

	obs = sink.Observations()
	var restartLoopRecovered bool
	for _, o := range obs {
		if o.Rule.ID == rRestart.ID && !o.Breached {
			restartLoopRecovered = true
		}
	}
	if !restartLoopRecovered {
		t.Fatal("expected SweepWindows to emit recovery observation once window expired")
	}
}
