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

	// Step 2: t=10s, push 92% (breach starts for both instant and sustained rules at point-in-time)
	clock.Advance(10 * time.Second)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 92.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	obs = sink.Observations()
	var instantObs, sustainedObs *EvalObservation
	for i := range obs {
		o := &obs[i]
		if o.Rule.ID == rInstant.ID && o.Breached {
			instantObs = o
		}
		if o.Rule.ID == rSustained.ID && o.Breached {
			sustainedObs = o
		}
	}
	if instantObs == nil {
		t.Fatal("t=10s: expected host-cpu-instant to emit point-in-time breach")
	}
	if instantObs.Summary != "host cpu_pct is 92.0 (> 85.0)" {
		t.Errorf("t=10s: unexpected instant summary: %q", instantObs.Summary)
	}
	if instantObs.Details["value"] != "92.0" || instantObs.Details["for"] != "" {
		t.Errorf("t=10s: unexpected instant details: %+v", instantObs.Details)
	}
	if sustainedObs == nil {
		t.Fatal("t=10s: expected host-cpu-sustained to emit point-in-time breach so StateMachine can enter StatePending")
	}
	if sustainedObs.Summary != "host cpu_pct is 92.0 (> 85.0 for 30s)" {
		t.Errorf("t=10s: unexpected sustained summary: %q", sustainedObs.Summary)
	}
	if sustainedObs.Details["value"] != "92.0" || sustainedObs.Details["for"] != "30s" ||
		sustainedObs.Details["threshold"] != "85.0" || sustainedObs.Details["operator"] != ">" {
		t.Errorf("t=10s: unexpected sustained details: %+v", sustainedObs.Details)
	}
	sink.Clear()

	// Step 3: t=20s (91%), t=30s (94%), t=40s (93%) -> sustained continues emitting point-in-time breaches on each tick
	for _, val := range []float64{91.0, 94.0, 93.0} {
		clock.Advance(10 * time.Second)
		hotCache.Record(model.MetricSample{
			Entity: "host", Metric: "cpu_pct", Value: val, TS: clock.Now().Unix(),
		})
		evaluator.EvalOnce(ctx)

		var gotInstant, gotSustained bool
		for _, o := range sink.Observations() {
			if o.Rule.ID == rInstant.ID && o.Breached && o.Value == val {
				gotInstant = true
			}
			if o.Rule.ID == rSustained.ID && o.Breached && o.Value == val {
				gotSustained = true
			}
		}
		if !gotInstant || !gotSustained {
			t.Errorf("t=%v: expected both rules to emit breach for value %.1f, got instant=%v sustained=%v",
				clock.Now(), val, gotInstant, gotSustained)
		}
		sink.Clear()
	}

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

func TestMetricEvaluator_WithStateMachine_SustainedDuration(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	rule, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "host-cpu-sustained-60s",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":85.0}`,
		ForSec:        60,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	if err := ruleCache.Reload(ctx); err != nil {
		t.Fatalf("cache.Reload: %v", err)
	}

	hotCache := metrics.NewHotCache(100, 180)
	startTime := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(startTime)
	disp := &mockDispatcher{}

	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)
	evaluator := NewMetricEvaluator(ruleCache, hotCache, sm, clock)

	// t=0s: initial breach transitions StateOK -> StatePending (0 notifications)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 92.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	states := sm.AllStates()
	if len(states) != 1 || states[0].RuleID != rule.ID || states[0].State != StatePending {
		t.Fatalf("t=0s: expected 1 StatePending state, got %+v", states)
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("t=0s: expected 0 notifications, got %d", len(disp.Notifications()))
	}

	// t=30s: sustained breach remains in StatePending (0 notifications)
	clock.Advance(30 * time.Second)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 94.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("t=30s: expected StatePending, got %+v", states)
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("t=30s: expected 0 notifications, got %d", len(disp.Notifications()))
	}

	// t=60s: after 60s sustained breach, transitions to StateFiring and dispatches notification
	clock.Advance(30 * time.Second)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 96.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StateFiring {
		t.Fatalf("t=60s: expected StateFiring, got %+v", states)
	}
	notifs := disp.Notifications()
	if len(notifs) != 1 {
		t.Fatalf("t=60s: expected 1 notification, got %d", len(notifs))
	}
	if notifs[0].State != "firing" || notifs[0].Rule != rule.Name || notifs[0].Target != "host" {
		t.Errorf("t=60s: unexpected notification payload: %+v", notifs[0])
	}
	if notifs[0].Details["for"] != "1m" {
		t.Errorf("t=60s: expected notification details[\"for\"] == \"1m\", got %+v", notifs[0].Details)
	}

	// Recover at t=70s -> StateOK
	disp.Clear()
	clock.Advance(10 * time.Second)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 45.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	if len(sm.AllStates()) != 0 {
		t.Fatalf("t=70s: expected StateOK (0 active states) after recovery, got %+v", sm.AllStates())
	}

	// Transient breach & recovery before 60s resets state to StateOK with 0 notifications
	clock.Advance(10 * time.Second) // t=80s (new breach start)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 91.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("t=80s: expected StatePending on transient breach start, got %+v", states)
	}

	clock.Advance(30 * time.Second) // t=110s (30s < 60s)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 93.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("t=110s: expected StatePending before 60s elapsed, got %+v", states)
	}

	clock.Advance(10 * time.Second) // t=120s (40s elapsed, recover before 60s)
	hotCache.Record(model.MetricSample{
		Entity: "host", Metric: "cpu_pct", Value: 50.0, TS: clock.Now().Unix(),
	})
	evaluator.EvalOnce(ctx)

	if len(sm.AllStates()) != 0 {
		t.Fatalf("t=120s: expected StateOK (0 active states) after transient recovery before 60s, got %+v", sm.AllStates())
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("t=120s: expected 0 notifications for transient breach, got %d", len(disp.Notifications()))
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

func TestMetricEvaluator_ContainerTargetAndMetricNormalization(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// 1. Bare container target with legacy container_cpu_pct metric
	rContainer, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "spell-cpu-test",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "spell-app-admin", // bare container name without "container:"
		ConditionJSON: `{"metric":"container_cpu_pct","operator":">","threshold":90.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create spell-cpu-test rule: %v", err)
	}

	// 2. Wildcard container target with container_mem_pct
	rWildcard, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "api-mem-test",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "warning",
		Target:        "api-*",
		ConditionJSON: `{"metric":"container_mem_pct","operator":">","threshold":80.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create api-mem-test rule: %v", err)
	}

	// 3. Host legacy host_cpu_pct
	rHost, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "host-cpu-legacy",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "warning",
		Target:        "host",
		ConditionJSON: `{"metric":"host_cpu_pct","operator":">","threshold":85.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create host-cpu-legacy rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	if err := ruleCache.Reload(ctx); err != nil {
		t.Fatalf("reload ruleCache: %v", err)
	}

	hotCache := metrics.NewHotCache(100, 180)
	clock := newFakeClock(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	nowSec := clock.Now().Unix()

	// Ingest metrics as Docker collector does:
	// - container:spell-app-admin with cpu_pct
	// - container:api-srv with mem_pct
	// - host with cpu_pct
	hotCache.Record(
		model.MetricSample{Entity: "container:spell-app-admin", Metric: "cpu_pct", Value: 92.4, TS: nowSec},
		model.MetricSample{Entity: "container:api-srv", Metric: "mem_pct", Value: 85.0, TS: nowSec},
		model.MetricSample{Entity: "host", Metric: "cpu_pct", Value: 91.0, TS: nowSec},
	)

	sink := &recordingSink{}
	evaluator := NewMetricEvaluator(ruleCache, hotCache, sink, clock)

	evaluator.EvalOnce(ctx)

	obs := sink.Observations()
	var spellBreached, apiBreached, hostBreached bool
	for _, o := range obs {
		if o.Rule.ID == rContainer.ID && o.Breached && o.Target == "container:spell-app-admin" && o.Value == 92.4 {
			spellBreached = true
		}
		if o.Rule.ID == rWildcard.ID && o.Breached && o.Target == "container:api-srv" && o.Value == 85.0 {
			apiBreached = true
		}
		if o.Rule.ID == rHost.ID && o.Breached && o.Target == "host" && o.Value == 91.0 {
			hostBreached = true
		}
	}

	if !spellBreached {
		t.Errorf("expected spell-app-admin container alert to breach with 92.4 CPU, got obs: %+v", obs)
	}
	if !apiBreached {
		t.Errorf("expected api-* container alert to breach with 85.0 mem_pct, got obs: %+v", obs)
	}
	if !hostBreached {
		t.Errorf("expected host alert to breach with 91.0 cpu_pct, got obs: %+v", obs)
	}
}
