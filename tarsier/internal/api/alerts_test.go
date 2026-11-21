package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

type mockNotifier struct {
	sent []alert.Notification
}

func (m *mockNotifier) Name() string { return "email" }
func (m *mockNotifier) Send(ctx context.Context, n alert.Notification) error {
	m.sent = append(m.sent, n)
	return nil
}

func setupAlertsTestServer(t *testing.T) (*Server, *db.Store, *http.Cookie, *mockNotifier) {
	t.Helper()
	srv, store, cookie := setupMetricsTestServer(t, nil)

	mockN := &mockNotifier{}
	dispatcher, err := alert.NewDispatcher(
		config.NotifiersConfig{},
		store,
		alert.WithNotifier("email", mockN),
	)
	if err != nil {
		t.Fatalf("failed to create dispatcher: %v", err)
	}

	ruleStore := alert.NewRuleStore(store.Config)
	ruleCache := alert.NewRuleCache(ruleStore)
	_ = ruleCache.Reload(context.Background())

	stateMachine := alert.NewStateMachine(ruleCache, ruleStore, store.Config, dispatcher, nil)
	_ = stateMachine.Restore(context.Background())

	hbWorker, err := alert.NewHeartbeatWorker(config.HeartbeatConfig{
		Enabled:  true,
		URL:      "http://localhost:8080/ping",
		Interval: "1m",
		Timeout:  "2s",
	})
	if err != nil {
		t.Fatalf("failed to create heartbeat worker: %v", err)
	}

	srv.ruleStore = ruleStore
	srv.ruleCache = ruleCache
	srv.stateMachine = stateMachine
	srv.dispatcher = dispatcher
	srv.heartbeat = hbWorker

	return srv, store, cookie, mockN
}

func TestAlertsAPI_ActiveAlerts(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	// Initial active alerts should be empty
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/active", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var active []alert.RuleTargetState
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil {
		t.Fatalf("failed to decode active alerts: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("expected 0 active alerts, got %d", len(active))
	}
}

func TestAlertsAPI_RuleCRUDAndToggle(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	// 1. List rules (initially empty)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/rules", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules: expected 200, got %d", rec.Code)
	}
	var rules []RuleWithState
	_ = json.Unmarshal(rec.Body.Bytes(), &rules)
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(rules))
	}

	// 2. Create rule
	createPayload := `{
		"name": "High CPU Usage",
		"kind": "metric",
		"target": "host",
		"severity": "critical",
		"condition_json": "{\"metric\":\"host_cpu_pct\",\"operator\":\">\",\"threshold\":85.0}",
		"for_sec": 30,
		"cooldown_sec": 300,
		"channels": ["email"],
		"enabled": true
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules", bytes.NewBufferString(createPayload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create rule: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created alert.AlertRule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode created rule: %v", err)
	}
	if created.ID <= 0 || created.Name != "High CPU Usage" {
		t.Fatalf("unexpected created rule ID=%d Name=%s", created.ID, created.Name)
	}

	// Verify cached
	cachedRule, ok := srv.ruleCache.Snapshot().ByID[created.ID]
	if !ok || cachedRule.Name != "High CPU Usage" {
		t.Errorf("rule not present in live ruleCache")
	}

	// 3. Get rule by ID
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/alerts/rules/%d", created.ID), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get rule: expected 200, got %d", rec.Code)
	}
	var fetched alert.AlertRule
	_ = json.Unmarshal(rec.Body.Bytes(), &fetched)
	if fetched.ID != created.ID || fetched.Name != "High CPU Usage" {
		t.Errorf("unexpected fetched rule: %+v", fetched)
	}

	// 4. Update rule
	updatePayload := fmt.Sprintf(`{
		"id": %d,
		"name": "High CPU Usage Updated",
		"kind": "metric",
		"target": "host",
		"severity": "warning",
		"condition_json": "{\"metric\":\"host_cpu_pct\",\"operator\":\">\",\"threshold\":90.0}",
		"for_sec": 60,
		"cooldown_sec": 600,
		"channels": ["email"],
		"enabled": true
	}`, created.ID)
	req = httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/alerts/rules/%d", created.ID), bytes.NewBufferString(updatePayload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update rule: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated alert.AlertRule
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Name != "High CPU Usage Updated" || updated.Severity != "warning" {
		t.Errorf("unexpected updated rule: %+v", updated)
	}

	// 5. Disable rule
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/alerts/rules/%d/disable", created.ID), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable rule: expected 200, got %d", rec.Code)
	}
	cachedRule = srv.ruleCache.Snapshot().ByID[created.ID]
	if cachedRule != nil && cachedRule.Enabled {
		t.Errorf("expected cached rule to be disabled")
	}

	// 6. Enable rule
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/alerts/rules/%d/enable", created.ID), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable rule: expected 200, got %d", rec.Code)
	}
	cachedRule = srv.ruleCache.Snapshot().ByID[created.ID]
	if cachedRule == nil || !cachedRule.Enabled {
		t.Errorf("expected cached rule to be enabled")
	}

	// 7. Delete rule
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/alerts/rules/%d", created.ID), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete rule: expected 200, got %d", rec.Code)
	}

	// Verify rule is gone from cache and database
	_, ok = srv.ruleCache.Snapshot().ByID[created.ID]
	if ok {
		t.Errorf("expected deleted rule to be purged from cache")
	}

	// 8. Get deleted rule returns 404
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/alerts/rules/%d", created.ID), nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted rule: expected 404, got %d", rec.Code)
	}
}

func TestAlertsAPI_TemplatesAndPreview(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	// 1. Starter templates
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/templates", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("templates: expected 200, got %d", rec.Code)
	}
	var templates []StarterTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &templates); err != nil {
		t.Fatalf("failed to decode templates: %v", err)
	}
	if len(templates) < 6 {
		t.Errorf("expected at least 6 starter templates, got %d", len(templates))
	}

	// 2. Metric preview with hot cache sample
	now := time.Now().UTC()
	srv.hotCache.Record(model.MetricSample{
		Entity: "host",
		Metric: "host_cpu_pct",
		TS:     now.Unix(),
		Value:  95.5,
	})

	previewPayload := `{
		"kind": "metric",
		"target": "host",
		"severity": "critical",
		"condition_json": "{\"metric\":\"host_cpu_pct\",\"operator\":\">\",\"threshold\":80.0}",
		"for_sec": 0
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules/preview", bytes.NewBufferString(previewPayload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview metric: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var previewResp RulePreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &previewResp); err != nil {
		t.Fatalf("failed to decode preview resp: %v", err)
	}
	if !previewResp.WouldFireNow {
		t.Errorf("expected would_fire_now = true for cpu=95.5 > 80.0")
	}
	if len(previewResp.MatchedTargets) != 1 {
		t.Errorf("expected 1 matched target, got %d", len(previewResp.MatchedTargets))
	}

	// 3. Log preview with tail cache
	tailCache := logs.NewTailCache(5, 100)
	srv.tailCache = tailCache
	tailCache.Push(model.LogEntry{
		SourceID:  "app-1",
		Timestamp: now,
		Level:     model.LevelError,
		Message:   []byte("connection refused to db"),
	})

	logPreviewPayload := `{
		"kind": "log",
		"target": "*",
		"severity": "warning",
		"condition_json": "{\"pattern\":\"connection refused\",\"is_regex\":false,\"threshold\":1,\"window_sec\":60}",
		"for_sec": 0
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules/preview", bytes.NewBufferString(logPreviewPayload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview log: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var logPreviewResp RulePreviewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &logPreviewResp)
	if !logPreviewResp.WouldFireNow {
		t.Errorf("expected log preview would_fire_now = true")
	}
	if len(logPreviewResp.SampleLines) == 0 {
		t.Errorf("expected sample lines in log preview")
	}
}

func TestAlertsAPI_ExportAndImport(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	// Create 2 rules
	_, err := srv.ruleStore.CreateRule(context.Background(), alert.AlertRule{
		Name:          "Export Rule 1",
		Kind:          "metric",
		Target:        "host",
		Severity:      "warning",
		ConditionJSON: `{"metric":"host_cpu_pct","operator":">","threshold":80.0}`,
		ForSec:        30,
		CooldownSec:   300,
		Channels:      []string{"email"},
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("failed to create rule 1: %v", err)
	}

	// 1. Export rules
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/rules/export", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("export rules: expected 200, got %d", rec.Code)
	}
	var bundle RulesExportBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("failed to decode export bundle: %v", err)
	}
	if bundle.Version != 1 || len(bundle.Rules) != 1 {
		t.Fatalf("unexpected export bundle: Version=%d Rules=%d", bundle.Version, len(bundle.Rules))
	}

	// 2. Modify bundle and import
	bundle.Rules[0].Severity = "critical"
	bundle.Rules = append(bundle.Rules, alert.AlertRule{
		Name:          "Imported Rule 2",
		Kind:          "container",
		Target:        "api-*",
		Severity:      "critical",
		ConditionJSON: `{"events":["oom"],"threshold":1,"window_sec":0}`,
		ForSec:        0,
		CooldownSec:   120,
		Channels:      []string{"email"},
		Enabled:       true,
	})

	importBody, _ := json.Marshal(bundle)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules/import", bytes.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("import rules: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var importResult map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &importResult)
	if int(importResult["imported"].(float64)) != 2 {
		t.Errorf("expected 2 imported rules, got %v", importResult["imported"])
	}

	// Verify updated rule 1 severity
	allRules, _ := srv.ruleStore.ListRules(context.Background())
	if len(allRules) != 2 {
		t.Errorf("expected 2 rules in store after import, got %d", len(allRules))
	}
}

func TestAlertsAPI_HistoryAndTestNotification(t *testing.T) {
	srv, store, cookie, mockN := setupAlertsTestServer(t)
	defer store.Close()

	// 1. Seed alert_event entry
	nowSec := time.Now().UTC().Unix()
	_, err := srv.ruleStore.InsertAlertEvent(context.Background(), alert.AlertEvent{
		RuleID:       1,
		RuleName:     "CPU High",
		Target:       "host",
		Severity:     "critical",
		State:        "resolved",
		Started:      nowSec - 120,
		Resolved:     nowSec - 60,
		Summary:      "CPU breached 90%",
		NotifyStatus: "delivered:mock",
	})
	if err != nil {
		t.Fatalf("failed to insert alert event: %v", err)
	}

	// 2. Query history
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/history?limit=10&severity=critical", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("history: expected 200, got %d", rec.Code)
	}
	var events []alert.AlertEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].RuleName != "CPU High" || events[0].State != "resolved" {
		t.Errorf("unexpected event: %+v", events[0])
	}

	// 3. Test notification
	testPayload := `{"channel":"email"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/test", bytes.NewBufferString(testPayload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("test notification: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mockN.sent) != 1 {
		t.Errorf("expected 1 test notification dispatched, got %d", len(mockN.sent))
	}

	// 4. System Health includes Heartbeat
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system/health", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system health: expected 200, got %d", rec.Code)
	}
	var healthResp SystemHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &healthResp); err != nil {
		t.Fatalf("failed to decode system health: %v", err)
	}
	if healthResp.Heartbeat == nil || !healthResp.Heartbeat.Enabled {
		t.Errorf("expected heartbeat status in system health")
	}
}
