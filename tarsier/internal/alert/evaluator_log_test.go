package alert

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestLogEvaluator_SlidingWindowThresholdAndRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Configure rule: pattern "connection refused", min_level Error (4), threshold 3, window 60s
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "DB Connection Refused",
		Kind:          "log",
		Enabled:       true,
		Severity:      "critical",
		Target:        "api-service",
		ConditionJSON: `{"pattern":"connection refused","is_regex":false,"ignore_case":true,"min_level":4,"threshold":3,"window_sec":60}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	sink := &recordingSink{}

	evaluator := NewLogEvaluator(ruleCache, nil, sink, clock)

	// Step 1: t=10s, Line 1 (LevelError)
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "api-service",
		Level:     model.LevelError,
		Message:   []byte("ERROR: [postgres] connection refused on port 5432"),
		Timestamp: t0.Add(10 * time.Second),
	})

	// Step 2: t=20s, Line 2 (LevelError)
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "api-service",
		Level:     model.LevelError,
		Message:   []byte("ERROR: [redis] connection refused on port 6379"),
		Timestamp: t0.Add(20 * time.Second),
	})

	// Assert no breach yet (count 2 < threshold 3)
	for _, o := range sink.Observations() {
		if o.Rule.ID == r.ID && o.Breached {
			t.Fatalf("unexpected breach at 2 hits: %+v", o)
		}
	}

	// Step 3: t=25s, Line with LevelInfo -> must be ignored due to min_level 4
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "api-service",
		Level:     model.LevelInfo,
		Message:   []byte("INFO: [redis] connection refused notice"),
		Timestamp: t0.Add(25 * time.Second),
	})

	for _, o := range sink.Observations() {
		if o.Rule.ID == r.ID && o.Breached {
			t.Fatalf("unexpected breach with LevelInfo: %+v", o)
		}
	}

	// Step 4: t=30s, Line 3 (LevelError) -> threshold 3 met!
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "api-service",
		Level:     model.LevelError,
		Message:   []byte("ERROR: [mysql] connection refused on port 3306"),
		Timestamp: t0.Add(30 * time.Second),
	})

	obs := sink.Observations()
	var breachFound bool
	for _, o := range obs {
		if o.Rule.ID == r.ID && o.Breached && o.Target == "api-service" && o.Value == 3 {
			breachFound = true
			if o.Details["sample_1"] == "" {
				t.Error("expected sample line in details")
			}
		}
	}
	if !breachFound {
		t.Fatal("expected breach observation at 3rd matching line")
	}
	sink.Clear()

	// Step 5: Advance clock past 60s window (to t=95s) and call SweepWindows
	clock.Advance(95 * time.Second)
	evaluator.SweepWindows(ctx)

	obs = sink.Observations()
	var recoveryFound bool
	for _, o := range obs {
		if o.Rule.ID == r.ID && !o.Breached && o.Target == "api-service" {
			recoveryFound = true
		}
	}
	if !recoveryFound {
		t.Fatal("expected recovery observation when sliding window hits aged out")
	}
}

func TestLogEvaluator_RegexAndWildcardTarget(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Regex rule with wildcard target
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Timeout Regex Wildcard",
		Kind:          "log",
		Enabled:       true,
		Severity:      "warning",
		Target:        "*",
		ConditionJSON: `{"pattern":"timeout after \\d+ms","is_regex":true,"ignore_case":true,"threshold":2,"window_sec":30}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create regex rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	sink := &recordingSink{}

	evaluator := NewLogEvaluator(ruleCache, nil, sink, clock)

	// worker-1 hit 1
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "worker-1",
		Level:     model.LevelError,
		Message:   []byte("request failed: timeout after 500ms"),
		Timestamp: t0,
	})

	// worker-2 hit 1
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "worker-2",
		Level:     model.LevelError,
		Message:   []byte("upstream error: timeout after 1200ms"),
		Timestamp: t0,
	})

	// Neither should fire yet (both have 1 hit, threshold is 2)
	for _, o := range sink.Observations() {
		if o.Breached {
			t.Fatalf("neither worker should have breached yet: %+v", o)
		}
	}

	// worker-1 hit 2 -> worker-1 must breach!
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "worker-1",
		Level:     model.LevelError,
		Message:   []byte("retry failed: timeout after 1500ms"),
		Timestamp: t0.Add(5 * time.Second),
	})

	obs := sink.Observations()
	var worker1Fired bool
	for _, o := range obs {
		if o.Rule.ID == r.ID && o.Target == "worker-1" && o.Breached {
			worker1Fired = true
		}
		if o.Target == "worker-2" && o.Breached {
			t.Errorf("worker-2 should not have breached yet: %+v", o)
		}
	}
	if !worker1Fired {
		t.Fatal("expected worker-1 to breach after 2 hits")
	}
	sink.Clear()

	// worker-2 hit 2 -> worker-2 now breaches independently!
	evaluator.ProcessEntry(ctx, model.LogEntry{
		SourceID:  "worker-2",
		Level:     model.LevelError,
		Message:   []byte("secondary timeout after 300ms"),
		Timestamp: t0.Add(10 * time.Second),
	})

	obs = sink.Observations()
	var worker2Fired bool
	for _, o := range obs {
		if o.Rule.ID == r.ID && o.Target == "worker-2" && o.Breached {
			worker2Fired = true
		}
	}
	if !worker2Fired {
		t.Fatal("expected worker-2 to breach independently after its 2nd hit")
	}
}

func TestLogEvaluator_BoundedMemoryUnderFlood(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Threshold 5, Window 60s
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Flood Test",
		Kind:          "log",
		Enabled:       true,
		Severity:      "critical",
		Target:        "flood-source",
		ConditionJSON: `{"pattern":"panic","is_regex":false,"threshold":5,"window_sec":60}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	sink := &recordingSink{}

	evaluator := NewLogEvaluator(ruleCache, nil, sink, clock)

	// Flood 50,000 lines
	for i := 0; i < 50000; i++ {
		evaluator.ProcessEntry(ctx, model.LogEntry{
			SourceID:  "flood-source",
			Level:     model.LevelError,
			Message:   []byte(fmt.Sprintf("FATAL: panic in thread %d", i)),
			Timestamp: t0,
		})
	}

	// Verify internal buffer is strictly capped at threshold (5)
	evaluator.mu.Lock()
	key := logWindowKey{ruleID: r.ID, sourceName: "flood-source"}
	st := evaluator.windows[key]
	evaluator.mu.Unlock()

	if st == nil {
		t.Fatal("expected window state to exist")
	}
	if len(st.hits) > 5 {
		t.Fatalf("expected hits buffer length <= 5, got %d", len(st.hits))
	}
	if len(st.sampleLines) > 2 {
		t.Fatalf("expected sampleLines length <= 2, got %d", len(st.sampleLines))
	}
}

func TestLogEvaluator_NonBlockingEnqueue(t *testing.T) {
	evaluator := NewLogEvaluator(nil, nil, nil, nil)

	// Enqueue more than 2048 items without a runner -> verify non-blocking drop
	for i := 0; i < 2100; i++ {
		evaluator.Enqueue(model.LogEntry{
			SourceID: "test",
			Message:  []byte("test line"),
		})
	}

	if evaluator.DroppedCount() == 0 {
		t.Errorf("expected dropped count > 0 after queue overflow")
	}
}

func TestLogEvaluator_SustainedDurationTransition(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Rule with threshold 3 in 60s window and sustained duration ForSec = 30s
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Sustained Error Burst",
		Kind:          "log",
		Enabled:       true,
		Severity:      "critical",
		Target:        "container:api",
		ConditionJSON: `{"pattern":"error","is_regex":false,"ignore_case":true,"threshold":3,"window_sec":60}`,
		ForSec:        30,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	if err := ruleCache.Reload(ctx); err != nil {
		t.Fatalf("reload ruleCache: %v", err)
	}

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}
	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)
	resolver := SourceResolverFunc(func(sourceID string) string {
		if sourceID == "1" {
			return "container:api"
		}
		return sourceID
	})

	evaluator := NewLogEvaluator(ruleCache, resolver, sm, clock)

	// t=0s: 3 matching entries arrive -> threshold breached, StateMachine enters StatePending
	for i := 0; i < 3; i++ {
		evaluator.ProcessEntry(ctx, model.LogEntry{
			SourceID:  "1",
			Level:     model.LevelError,
			Message:   []byte(fmt.Sprintf("ERROR: failure #%d", i+1)),
			Timestamp: t0,
		})
	}

	states := sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("expected 1 StatePending after initial breach, got %+v", states)
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("expected 0 notifications while pending, got %d", len(disp.Notifications()))
	}

	// t=10s: SweepWindows -> still < 30s elapsed, remains StatePending
	clock.Advance(10 * time.Second)
	evaluator.SweepWindows(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("expected StatePending at t=10s, got %+v", states)
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("expected 0 notifications at t=10s, got %d", len(disp.Notifications()))
	}

	// t=20s: SweepWindows -> still < 30s elapsed, remains StatePending
	clock.Advance(10 * time.Second)
	evaluator.SweepWindows(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("expected StatePending at t=20s, got %+v", states)
	}
	if len(disp.Notifications()) != 0 {
		t.Fatalf("expected 0 notifications at t=20s, got %d", len(disp.Notifications()))
	}

	// t=30s: SweepWindows -> 30s elapsed (>= ForSec), transitions StatePending -> StateFiring
	clock.Advance(10 * time.Second)
	evaluator.SweepWindows(ctx)

	states = sm.AllStates()
	if len(states) != 1 || states[0].State != StateFiring {
		t.Fatalf("expected StateFiring at t=30s, got %+v", states)
	}
	notifs := disp.Notifications()
	if len(notifs) != 1 {
		t.Fatalf("expected 1 firing notification at t=30s, got %d", len(notifs))
	}
	if notifs[0].Rule != r.Name || notifs[0].Target != "container:api" || notifs[0].State != "firing" {
		t.Errorf("unexpected notification payload: %+v", notifs[0])
	}

	// t=100s: Window hits aged out (>60s) -> SweepWindows transitions StateFiring -> StateOK
	clock.Advance(70 * time.Second)
	evaluator.SweepWindows(ctx)

	if len(sm.AllStates()) != 0 {
		t.Fatalf("expected StateOK (0 active states) after window expiry, got %+v", sm.AllStates())
	}
}
