package alert

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

type mockDispatcher struct {
	mu            sync.Mutex
	notifications []Notification
}

func (m *mockDispatcher) Dispatch(channels []string, n Notification) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, n)
}

func (m *mockDispatcher) Notifications() []Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Notification, len(m.notifications))
	copy(out, m.notifications)
	return out
}

func (m *mockDispatcher) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = nil
}

func TestStateMachine_PendingHoldAndTransientBlipReset(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Rule with for_sec: 30
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Sustained CPU",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":90.0}`,
		ForSec:        30,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)
	compiledRule := ruleCache.Snapshot().ByID[r.ID]

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}

	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)

	// Step 1: t=0, Breached: true -> enters StatePending, 0 notifications
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    92.0,
		Summary:  "cpu 92%",
		At:       t0,
	})

	if len(disp.Notifications()) != 0 {
		t.Fatalf("expected 0 notifications on pending, got %d", len(disp.Notifications()))
	}
	states := sm.AllStates()
	if len(states) != 1 || states[0].State != StatePending {
		t.Fatalf("expected 1 pending state, got %+v", states)
	}

	// Step 2: t=20s (before 30s elapsed), Breached: false -> transient blip resets to OK
	clock.Advance(20 * time.Second)
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: false,
		Value:    50.0,
		Summary:  "cpu 50%",
		At:       clock.Now(),
	})

	if len(disp.Notifications()) != 0 {
		t.Fatalf("expected 0 notifications on transient recovery, got %d", len(disp.Notifications()))
	}
	if len(sm.AllStates()) != 0 {
		t.Fatalf("expected 0 active states after transient reset, got %+v", sm.AllStates())
	}

	// Check DB: 0 alert_event rows
	events, _ := ruleStore.ListAlertEvents(ctx, EventFilter{})
	if len(events) != 0 {
		t.Fatalf("expected 0 alert_event rows, got %d", len(events))
	}

	// Step 3: t=30s breach begins again, feeds at t=30, t=45, t=60 (sustained for 30s)
	clock.Advance(10 * time.Second) // now t=30s
	tBreachStart := clock.Now()
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    93.0,
		Summary:  "cpu 93%",
		At:       tBreachStart,
	})

	clock.Advance(15 * time.Second) // t=45s (15s elapsed, still pending)
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    94.0,
		Summary:  "cpu 94%",
		At:       clock.Now(),
	})
	if len(disp.Notifications()) != 0 {
		t.Fatalf("t=45s: expected 0 notifications, got %d", len(disp.Notifications()))
	}

	clock.Advance(15 * time.Second) // t=60s (30s elapsed >= for_sec) -> transitions to StateFiring!
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    95.0,
		Summary:  "cpu 95%",
		At:       clock.Now(),
	})

	notifs := disp.Notifications()
	if len(notifs) != 1 {
		t.Fatalf("expected 1 firing notification at t=60s, got %d", len(notifs))
	}
	if notifs[0].State != "firing" || notifs[0].Rule != "Sustained CPU" {
		t.Errorf("unexpected notification: %+v", notifs[0])
	}

	firing := sm.ActiveAlerts()
	if len(firing) != 1 || firing[0].State != StateFiring {
		t.Fatalf("expected 1 active alert, got %+v", firing)
	}

	events, _ = ruleStore.ListAlertEvents(ctx, EventFilter{})
	if len(events) != 1 || events[0].State != "firing" {
		t.Fatalf("expected 1 open alert_event, got %+v", events)
	}
}

func TestStateMachine_CooldownSuppressionAndSeverityEscalation(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Rule: for_sec: 0, cooldown_sec: 300, severity: warning
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "Memory Warning",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "warning",
		Target:        "host",
		ConditionJSON: `{"metric":"mem_used_pct","operator":">","threshold":80.0}`,
		ForSec:        0,
		CooldownSec:   300,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)
	compiledRule := ruleCache.Snapshot().ByID[r.ID]

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}
	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)

	// 1. Fire at t=100s -> notifies (count == 1)
	clock.Advance(100 * time.Second)
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    82.0,
		Summary:  "mem 82%",
		At:       clock.Now(),
	})
	if len(disp.Notifications()) != 1 {
		t.Fatalf("expected 1 notification at t=100s, got %d", len(disp.Notifications()))
	}

	// 2. Continues breaching at t=160s and t=280s (within 300s cooldown) -> suppressed (count == 1)
	clock.Advance(60 * time.Second) // t=160s
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    84.0,
		Summary:  "mem 84%",
		At:       clock.Now(),
	})

	clock.Advance(120 * time.Second) // t=280s
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    85.0,
		Summary:  "mem 85%",
		At:       clock.Now(),
	})

	if len(disp.Notifications()) != 1 {
		t.Fatalf("expected notification to be suppressed within cooldown, count=%d", len(disp.Notifications()))
	}

	// 3. Escalate severity to critical at t=310s (still within 300s cooldown since t=100s) -> bypasses cooldown!
	clock.Advance(30 * time.Second) // t=310s (210s since last notification < 300s)
	escalatedRule := *compiledRule
	escalatedRule.Severity = "critical"

	sm.Observe(ctx, EvalObservation{
		Rule:     &escalatedRule,
		Target:   "host",
		Breached: true,
		Value:    95.0,
		Summary:  "mem 95% (critical)",
		At:       clock.Now(),
	})

	if len(disp.Notifications()) != 2 {
		t.Fatalf("expected severity escalation to bypass cooldown, count=%d", len(disp.Notifications()))
	}
	if disp.Notifications()[1].Severity != "critical" {
		t.Errorf("expected escalated severity critical, got %s", disp.Notifications()[1].Severity)
	}

	// 4. Continue breaching at t=620s (> 300s since t=310s) -> notifies again!
	clock.Advance(310 * time.Second) // t=620s (310s since last notification)
	sm.Observe(ctx, EvalObservation{
		Rule:     &escalatedRule,
		Target:   "host",
		Breached: true,
		Value:    96.0,
		Summary:  "mem 96%",
		At:       clock.Now(),
	})

	if len(disp.Notifications()) != 3 {
		t.Fatalf("expected repeat notification after cooldown expired, count=%d", len(disp.Notifications()))
	}
}

func TestStateMachine_RecoveryNotificationAndDBSync(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Rule with notify_recovery: true
	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:           "Disk Full Alert",
		Kind:           "metric",
		Enabled:        true,
		Severity:       "critical",
		Target:         "disk:/",
		ConditionJSON:  `{"metric":"disk_used_pct","operator":">=","threshold":90.0}`,
		ForSec:         0,
		CooldownSec:    300,
		Channels:       []string{"email"},
		NotifyRecovery: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)
	compiledRule := ruleCache.Snapshot().ByID[r.ID]

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}
	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)

	// Fire alert
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "disk:/",
		Breached: true,
		Value:    92.0,
		Summary:  "disk 92%",
		At:       t0,
	})

	events, _ := ruleStore.ListAlertEvents(ctx, EventFilter{})
	if len(events) != 1 || events[0].State != "firing" || events[0].Resolved != 0 {
		t.Fatalf("expected firing event in DB, got %+v", events)
	}

	// Send Breached: false (recovery)
	clock.Advance(60 * time.Second)
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "disk:/",
		Breached: false,
		Value:    75.0,
		Summary:  "disk returned to 75%",
		At:       clock.Now(),
	})

	notifs := disp.Notifications()
	if len(notifs) != 2 {
		t.Fatalf("expected 2 notifications (1 firing, 1 resolved), got %d", len(notifs))
	}
	if notifs[1].State != "resolved" {
		t.Errorf("expected resolved notification, got %s", notifs[1].State)
	}

	// Assert alert_event was updated to resolved
	events, _ = ruleStore.ListAlertEvents(ctx, EventFilter{})
	if len(events) != 1 || events[0].State != "resolved" || events[0].Resolved == 0 {
		t.Fatalf("expected resolved alert_event in DB, got %+v", events)
	}

	// Assert active alerts empty
	if len(sm.ActiveAlerts()) != 0 {
		t.Errorf("expected 0 active alerts, got %+v", sm.ActiveAlerts())
	}
}

func TestStateMachine_StartupRestoreAndRuleEditReset(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer dbStore.Close()

	ruleStore := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	r, err := ruleStore.CreateRule(ctx, AlertRule{
		Name:          "CPU Alert",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "critical",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":80.0}`,
		Channels:      []string{"discord"},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	ruleCache := NewRuleCache(ruleStore)
	_ = ruleCache.Reload(ctx)
	compiledRule := ruleCache.Snapshot().ByID[r.ID]

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}
	sm := NewStateMachine(ruleCache, ruleStore, dbStore.Config, disp, clock)

	// Fire alert
	sm.Observe(ctx, EvalObservation{
		Rule:     compiledRule,
		Target:   "host",
		Breached: true,
		Value:    90.0,
		Summary:  "cpu 90%",
		At:       t0,
	})
	disp.Clear()

	// Instantiate fresh StateMachine backed by same DB
	freshDisp := &mockDispatcher{}
	freshSM := NewStateMachine(ruleCache, ruleStore, dbStore.Config, freshDisp, clock)
	if err := freshSM.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	active := freshSM.ActiveAlerts()
	if len(active) != 1 || active[0].State != StateFiring || active[0].RuleID != r.ID {
		t.Fatalf("expected restored active firing alert, got %+v", active)
	}
	if len(freshDisp.Notifications()) != 0 {
		t.Errorf("restore should not re-dispatch firing notification")
	}

	// Update rule -> triggers OnRulesSwapped -> resets state cleanly
	r.UpdatedAt = r.UpdatedAt.Add(1 * time.Minute)
	r.ForSec = 60
	updatedRule, err := ruleStore.UpdateRule(ctx, r)
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}

	// Reload ruleCache which triggers OnRulesSwapped
	_ = ruleCache.Reload(ctx)

	// Verify state was reset
	if len(freshSM.ActiveAlerts()) != 0 {
		t.Errorf("expected active alerts to be purged on rule swap, got %+v", freshSM.ActiveAlerts())
	}
	_ = updatedRule
}

func TestStateMachine_EphemeralTargetNoMapLeak(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	disp := &mockDispatcher{}
	sm := NewStateMachine(nil, nil, nil, disp, clock)

	rule := &CompiledRule{
		AlertRule: AlertRule{
			ID:             42,
			Name:           "Container Restart Alert",
			Kind:           "container",
			Enabled:        true,
			Severity:       "warning",
			Target:         "container:*",
			ForSec:         0,
			CooldownSec:    300,
			Channels:       []string{"discord"},
			NotifyRecovery: true,
		},
	}

	// Simulate 5,000 healthy container events with unique ephemeral IDs.
	for i := 0; i < 5000; i++ {
		sm.Observe(ctx, EvalObservation{
			Rule:     rule,
			Target:   fmt.Sprintf("container:ephemeral-%04d", i),
			Breached: false,
			Value:    0,
			Summary:  "container healthy",
			At:       clock.Now(),
		})
	}

	sm.mu.RLock()
	statesCount := len(sm.states)
	sm.mu.RUnlock()
	if statesCount != 0 {
		t.Fatalf("expected 0 entries in sm.states after 5,000 healthy events, got %d", statesCount)
	}

	// Keep one target actively firing.
	sm.Observe(ctx, EvalObservation{
		Rule:     rule,
		Target:   "container:active-firing",
		Breached: true,
		Value:    1,
		Summary:  "container down",
		At:       clock.Now(),
	})

	// Simulate 100 ephemeral targets that breach and then recover.
	for i := 0; i < 100; i++ {
		target := fmt.Sprintf("container:flapping-%03d", i)
		sm.Observe(ctx, EvalObservation{
			Rule:     rule,
			Target:   target,
			Breached: true,
			Value:    1,
			Summary:  "container flapping",
			At:       clock.Now(),
		})
		sm.Observe(ctx, EvalObservation{
			Rule:     rule,
			Target:   target,
			Breached: false,
			Value:    0,
			Summary:  "container recovered",
			At:       clock.Now(),
		})
	}

	sm.mu.RLock()
	statesCount = len(sm.states)
	for k, st := range sm.states {
		if st.State == StateOK {
			sm.mu.RUnlock()
			t.Fatalf("found orphaned StateOK entry in sm.states for key %+v", k)
		}
	}
	sm.mu.RUnlock()

	if statesCount != 1 {
		t.Fatalf("expected len(sm.states) == 1 (only active firing target), got %d", statesCount)
	}
}
