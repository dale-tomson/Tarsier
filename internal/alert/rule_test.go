package alert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func TestCompileRule_ValidationAndLimits(t *testing.T) {
	validMetricJSON := `{"metric":"cpu_pct","operator":">","threshold":85.5}`
	validLogSubJSON := `{"pattern":"error: connection refused","is_regex":false,"ignore_case":true,"threshold":5,"window_sec":60}`
	validLogRegJSON := `{"pattern":"timeout after \\d+ms","is_regex":true,"ignore_case":true,"threshold":2,"window_sec":30}`
	validContainerJSON := `{"events":["die","oom"],"threshold":1,"window_sec":0}`

	t.Run("Valid Rules", func(t *testing.T) {
		tests := []struct {
			name string
			rule AlertRule
		}{
			{
				name: "Valid Metric Rule",
				rule: AlertRule{
					Name:          "High CPU",
					Kind:          "metric",
					Enabled:       true,
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					ForSec:        60,
					CooldownSec:   300,
					Channels:      []string{"discord"},
				},
			},
			{
				name: "Valid Log Substring Rule",
				rule: AlertRule{
					Name:          "Log Connection Refused",
					Kind:          "log",
					Enabled:       true,
					Severity:      "warning",
					Target:        "api-service",
					ConditionJSON: validLogSubJSON,
					ForSec:        0,
					CooldownSec:   600,
					Channels:      []string{"email", "discord"},
				},
			},
			{
				name: "Valid Log Regex Rule",
				rule: AlertRule{
					Name:          "Log Timeout Regex",
					Kind:          "log",
					Enabled:       true,
					Severity:      "warning",
					Target:        "*",
					ConditionJSON: validLogRegJSON,
					ForSec:        0,
					CooldownSec:   300,
					Channels:      []string{"discord"},
				},
			},
			{
				name: "Valid Container Event Rule",
				rule: AlertRule{
					Name:          "Container OOM or Die",
					Kind:          "container",
					Enabled:       true,
					Severity:      "critical",
					Target:        "container:*",
					ConditionJSON: validContainerJSON,
					ForSec:        0,
					CooldownSec:   300,
					Channels:      []string{"email"},
				},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				compiled, err := CompileRule(tc.rule)
				if err != nil {
					t.Fatalf("unexpected compile error: %v", err)
				}
				if compiled == nil {
					t.Fatal("expected non-nil CompiledRule")
				}
				if compiled.Name != tc.rule.Name {
					t.Errorf("expected name %q, got %q", tc.rule.Name, compiled.Name)
				}
			})
		}
	})

	t.Run("Rejection Traps", func(t *testing.T) {
		tests := []struct {
			name        string
			rule        AlertRule
			errContains string
		}{
			{
				name: "Empty Name",
				rule: AlertRule{
					Name:          "",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{"discord"},
				},
				errContains: "name cannot be empty",
			},
			{
				name: "Oversized Name",
				rule: AlertRule{
					Name:          strings.Repeat("A", MaxRuleNameLen+1),
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{"discord"},
				},
				errContains: "name exceeds maximum length",
			},
			{
				name: "Invalid Kind",
				rule: AlertRule{
					Name:          "Bad Kind",
					Kind:          "telemetry",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{"discord"},
				},
				errContains: "invalid rule kind",
			},
			{
				name: "Invalid Severity",
				rule: AlertRule{
					Name:          "Bad Severity",
					Kind:          "metric",
					Severity:      "urgent",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{"discord"},
				},
				errContains: "invalid severity",
			},
			{
				name: "Negative ForSec",
				rule: AlertRule{
					Name:          "Negative ForSec",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					ForSec:        -1,
					Channels:      []string{"discord"},
				},
				errContains: "for_sec -1 out of range",
			},
			{
				name: "Negative CooldownSec",
				rule: AlertRule{
					Name:          "Negative CooldownSec",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					CooldownSec:   -5,
					Channels:      []string{"discord"},
				},
				errContains: "cooldown_sec -5 out of range",
			},
			{
				name: "No Channels",
				rule: AlertRule{
					Name:          "No Channels",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{},
				},
				errContains: "at least one notification channel",
			},
			{
				name: "Invalid Channel",
				rule: AlertRule{
					Name:          "Slack Channel",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: validMetricJSON,
					Channels:      []string{"slack"},
				},
				errContains: "unsupported notification channel",
			},
			{
				name: "Metric Unknown Operator",
				rule: AlertRule{
					Name:          "Bad Operator",
					Kind:          "metric",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: `{"metric":"cpu_pct","operator":"~=","threshold":50}`,
					Channels:      []string{"discord"},
				},
				errContains: "invalid operator",
			},
			{
				name: "Log Invalid Regex",
				rule: AlertRule{
					Name:          "Broken Regex",
					Kind:          "log",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: `{"pattern":"[unclosed","is_regex":true,"threshold":1,"window_sec":60}`,
					Channels:      []string{"discord"},
				},
				errContains: "invalid regex pattern",
			},
			{
				name: "Log Oversized Pattern",
				rule: AlertRule{
					Name:          "Huge Pattern",
					Kind:          "log",
					Severity:      "critical",
					Target:        "host",
					ConditionJSON: fmt.Sprintf(`{"pattern":%q,"is_regex":false,"threshold":1,"window_sec":60}`, strings.Repeat("x", MaxPatternLen+1)),
					Channels:      []string{"discord"},
				},
				errContains: "pattern exceeds maximum length",
			},
			{
				name: "Container Invalid Event",
				rule: AlertRule{
					Name:          "Bad Container Event",
					Kind:          "container",
					Severity:      "critical",
					Target:        "*",
					ConditionJSON: `{"events":["crash"],"threshold":1,"window_sec":0}`,
					Channels:      []string{"discord"},
				},
				errContains: "invalid event \"crash\"",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := CompileRule(tc.rule)
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
			})
		}
	})

	t.Run("Target and Log Matching", func(t *testing.T) {
		// Target matcher
		wildcardRule, _ := CompileRule(AlertRule{
			Name: "All", Kind: "metric", Severity: "info", Target: "*", ConditionJSON: validMetricJSON, Channels: []string{"discord"},
		})
		if !wildcardRule.MatchesTarget("host") || !wildcardRule.MatchesTarget("disk:/data") {
			t.Error("wildcard target should match anything")
		}

		diskRule, _ := CompileRule(AlertRule{
			Name: "Disks", Kind: "metric", Severity: "info", Target: "disk:*", ConditionJSON: validMetricJSON, Channels: []string{"discord"},
		})
		if !diskRule.MatchesTarget("disk:/mnt") || !diskRule.MatchesTarget("disk:/") {
			t.Error("disk:* should match disk:/mnt and disk:/")
		}
		if diskRule.MatchesTarget("container:api") || diskRule.MatchesTarget("host") {
			t.Error("disk:* should not match container:api or host")
		}

		// Substring matching (case-insensitive)
		subRule, _ := CompileRule(AlertRule{
			Name: "Sub", Kind: "log", Severity: "info", Target: "*",
			ConditionJSON: `{"pattern":"Connection Refused","is_regex":false,"ignore_case":true,"threshold":1,"window_sec":30}`,
			Channels:      []string{"discord"},
		})
		if !subRule.MatchLogLine([]byte("WARN: [db] connection refused at socket")) {
			t.Error("case-insensitive substring should match")
		}
		if subRule.MatchLogLine([]byte("INFO: [db] connection accepted")) {
			t.Error("substring should not match")
		}

		// Regex matching
		regRule, _ := CompileRule(AlertRule{
			Name: "Reg", Kind: "log", Severity: "info", Target: "*",
			ConditionJSON: `{"pattern":"status=(4\\d\\d|5\\d\\d)","is_regex":true,"threshold":1,"window_sec":30}`,
			Channels:      []string{"discord"},
		})
		if !regRule.MatchLogLine([]byte("POST /api/v1/checkout status=503 latency=12ms")) {
			t.Error("regex should match 503")
		}
		if regRule.MatchLogLine([]byte("GET /healthz status=200 latency=1ms")) {
			t.Error("regex should not match 200")
		}
	})
}

func TestRuleStore_CRUDAndCap(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer dbStore.Close()

	store := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// 1. Create Rule
	created, err := store.CreateRule(ctx, AlertRule{
		Name:          "CPU Spike",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "warning",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":90}`,
		ForSec:        60,
		CooldownSec:   300,
		Channels:      []string{"email"},
	})
	if err != nil {
		t.Fatalf("CreateRule failed: %v", err)
	}
	if created.ID <= 0 {
		t.Fatalf("expected positive rule ID, got %d", created.ID)
	}

	// 2. Get Rule
	fetched, err := store.GetRule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRule failed: %v", err)
	}
	if fetched.Name != "CPU Spike" || fetched.Severity != "warning" || !fetched.Enabled {
		t.Errorf("unexpected fetched rule: %+v", fetched)
	}

	// 3. Update Rule
	fetched.Severity = "critical"
	fetched.ForSec = 120
	updated, err := store.UpdateRule(ctx, fetched)
	if err != nil {
		t.Fatalf("UpdateRule failed: %v", err)
	}
	if updated.Severity != "critical" || updated.ForSec != 120 {
		t.Errorf("unexpected updated rule: %+v", updated)
	}

	// 4. Toggle Enabled
	if err := store.SetRuleEnabled(ctx, created.ID, false); err != nil {
		t.Fatalf("SetRuleEnabled failed: %v", err)
	}
	toggled, _ := store.GetRule(ctx, created.ID)
	if toggled.Enabled {
		t.Error("expected rule to be disabled")
	}

	// 5. List Rules
	rules, err := store.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules failed: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}

	// 6. Alert Events CRUD
	eventID, err := store.InsertAlertEvent(ctx, AlertEvent{
		RuleID:      created.ID,
		RuleName:    "CPU Spike",
		Target:      "host",
		Severity:    "critical",
		State:       "firing",
		Started:     1700000000,
		Summary:     "host CPU exceeded 90%",
		DetailsJSON: `{"cpu_pct":"92%"}`,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("InsertAlertEvent failed: %v", err)
	}
	if eventID <= 0 {
		t.Fatalf("expected positive event ID, got %d", eventID)
	}

	if err := store.UpdateAlertEventNotifyStatus(ctx, eventID, "sent: email"); err != nil {
		t.Fatalf("UpdateAlertEventNotifyStatus: %v", err)
	}
	if err := store.UpdateAlertEventResolved(ctx, eventID, 1700000060); err != nil {
		t.Fatalf("UpdateAlertEventResolved: %v", err)
	}

	events, err := store.ListAlertEvents(ctx, EventFilter{RuleID: created.ID})
	if err != nil {
		t.Fatalf("ListAlertEvents failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].State != "resolved" || events[0].NotifyStatus != "sent: email" {
		t.Errorf("unexpected event: %+v", events[0])
	}

	// 7. Delete Rule
	if err := store.DeleteRule(ctx, created.ID); err != nil {
		t.Fatalf("DeleteRule failed: %v", err)
	}
	_, err = store.GetRule(ctx, created.ID)
	if !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("expected ErrRuleNotFound after deletion, got %v", err)
	}

	// 8. Enforce 250 rule cap
	for i := 0; i < MaxRulesCount; i++ {
		_, err := store.CreateRule(ctx, AlertRule{
			Name:          fmt.Sprintf("Rule %03d", i),
			Kind:          "metric",
			Enabled:       true,
			Severity:      "info",
			Target:        "host",
			ConditionJSON: `{"metric":"load1","operator":">","threshold":5}`,
			Channels:      []string{"discord"},
		})
		if err != nil {
			t.Fatalf("create rule %d: %v", i, err)
		}
	}

	_, capErr := store.CreateRule(ctx, AlertRule{
		Name:          "One Too Many",
		Kind:          "metric",
		Severity:      "info",
		Target:        "host",
		ConditionJSON: `{"metric":"load1","operator":">","threshold":5}`,
		Channels:      []string{"discord"},
	})
	if !errors.Is(capErr, ErrRuleCapExceeded) {
		t.Fatalf("expected ErrRuleCapExceeded, got %v", capErr)
	}
}

func TestRuleCache_AtomicSwapAndConcurrency(t *testing.T) {
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer dbStore.Close()

	store := NewRuleStore(dbStore.Config)
	ctx := context.Background()

	// Seed 3 rules (1 metric, 1 log, 1 container)
	_, _ = store.CreateRule(ctx, AlertRule{
		Name:          "M1",
		Kind:          "metric",
		Enabled:       true,
		Severity:      "info",
		Target:        "host",
		ConditionJSON: `{"metric":"cpu_pct","operator":">","threshold":50}`,
		Channels:      []string{"discord"},
	})
	_, _ = store.CreateRule(ctx, AlertRule{
		Name:          "L1",
		Kind:          "log",
		Enabled:       true,
		Severity:      "warning",
		Target:        "*",
		ConditionJSON: `{"pattern":"error","is_regex":false,"threshold":5,"window_sec":60}`,
		Channels:      []string{"email"},
	})
	_, _ = store.CreateRule(ctx, AlertRule{
		Name:          "C1",
		Kind:          "container",
		Enabled:       true,
		Severity:      "critical",
		Target:        "container:*",
		ConditionJSON: `{"events":["oom"],"threshold":1,"window_sec":0}`,
		Channels:      []string{"discord"},
	})

	var swapCount atomic.Int32
	cache := NewRuleCache(store, WithOnSwap(func(oldSet, newSet *RuleSet) {
		swapCount.Add(1)
	}))

	// Initial reload
	if err := cache.Reload(ctx); err != nil {
		t.Fatalf("cache.Reload: %v", err)
	}

	snap := cache.Snapshot()
	if snap.Version != 1 {
		t.Errorf("expected version 1, got %d", snap.Version)
	}
	if len(snap.All) != 3 {
		t.Errorf("expected 3 total rules, got %d", len(snap.All))
	}
	if len(snap.MetricRules) != 1 || len(snap.LogRules) != 1 || len(snap.ContainerRules) != 1 {
		t.Errorf("unexpected categorized rule counts: M=%d, L=%d, C=%d",
			len(snap.MetricRules), len(snap.LogRules), len(snap.ContainerRules))
	}

	// Concurrency test: 8 reader goroutines continuously snapshotting while 2 writer goroutines reload
	var wg sync.WaitGroup
	stopChan := make(chan struct{})

	// Readers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopChan:
					return
				default:
					s := cache.Snapshot()
					if s == nil || s.Version == 0 {
						t.Errorf("snapshot was nil or zero version")
						return
					}
				}
			}
		}()
	}

	// Writers
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = cache.Reload(ctx)
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	// Run for a short window then stop
	time.Sleep(100 * time.Millisecond)
	close(stopChan)
	wg.Wait()

	finalSnap := cache.Snapshot()
	if finalSnap.Version < 2 {
		t.Errorf("expected version >= 2 after reloads, got %d", finalSnap.Version)
	}
	if swapCount.Load() < 2 {
		t.Errorf("expected at least 2 swaps, got %d", swapCount.Load())
	}
}
