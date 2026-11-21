package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

func seedTestSource(t *testing.T, srv *Server, kind, name string) int64 {
	t.Helper()
	var sourceID int64
	nowSec := time.Now().UTC().Unix()
	err := srv.configDB.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES (?, ?, 1, ?, ?);
		`, kind, name, nowSec, nowSec)
		if execErr != nil {
			return execErr
		}
		id, idErr := res.LastInsertId()
		if idErr != nil {
			return idErr
		}
		sourceID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seedTestSource(%s, %s): %v", kind, name, err)
	}
	return sourceID
}

func postRulePreview(t *testing.T, srv *Server, cookie *http.Cookie, payload RulePreviewRequest) RulePreviewResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal preview payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp RulePreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	return resp
}

func TestPreviewLogRule_WindowEnforcement(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	sid := seedTestSource(t, srv, "container", "api-svc")
	tailCache := logs.NewTailCache(10, 200)
	srv.tailCache = tailCache

	now := time.Now().UTC()

	// Push 6 matching lines spread over 10 minutes (2 minutes apart), so at most 1 hit in any 60s window
	for i := 0; i < 6; i++ {
		tailCache.Push(model.LogEntry{
			SourceID:  fmt.Sprintf("%d", sid),
			Timestamp: now.Add(-time.Duration(i*2+2) * time.Minute),
			Level:     model.LevelError,
			Message:   []byte(fmt.Sprintf("ERROR: database connection failed #%d", i)),
		})
	}

	resp := postRulePreview(t, srv, cookie, RulePreviewRequest{
		Kind:          "log",
		Target:        "container:*",
		Severity:      "critical",
		ConditionJSON: `{"pattern":"error","is_regex":false,"ignore_case":true,"threshold":5,"window_sec":60}`,
		ForSec:        0,
		Channels:      []string{"email"},
	})

	if resp.WouldFireNow {
		t.Errorf("expected would_fire_now=false when matches are spread outside window_sec, got true")
	}
	if resp.RecentMatches24h != 6 {
		t.Errorf("expected recent_matches_24h=6, got %d", resp.RecentMatches24h)
	}
	if len(resp.MatchedTargets) != 0 {
		t.Errorf("expected 0 matched targets, got %+v", resp.MatchedTargets)
	}
	if len(resp.SampleLines) == 0 {
		t.Errorf("expected diagnostic sample lines from tail")
	}
}

func TestPreviewLogRule_WithinWindowAndSustainedNote(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	sid := seedTestSource(t, srv, "container", "worker-1")
	tailCache := logs.NewTailCache(10, 200)
	srv.tailCache = tailCache

	now := time.Now().UTC()

	// Push 5 matching lines within the last 30 seconds
	for i := 0; i < 5; i++ {
		tailCache.Push(model.LogEntry{
			SourceID:  fmt.Sprintf("%d", sid),
			Timestamp: now.Add(-time.Duration(i*5) * time.Second),
			Level:     model.LevelError,
			Message:   []byte(fmt.Sprintf("ERROR: timeout talking to upstream #%d", i)),
		})
	}

	resp := postRulePreview(t, srv, cookie, RulePreviewRequest{
		Kind:          "log",
		Target:        "container:*",
		Severity:      "critical",
		ConditionJSON: `{"pattern":"timeout","is_regex":false,"ignore_case":true,"threshold":5,"window_sec":60}`,
		ForSec:        300,
		Channels:      []string{"email"},
	})

	if !resp.WouldFireNow {
		t.Fatalf("expected would_fire_now=true when 5 matches are within 60s window")
	}
	if resp.RecentMatches24h != 5 {
		t.Errorf("expected recent_matches_24h=5, got %d", resp.RecentMatches24h)
	}
	if len(resp.MatchedTargets) != 1 {
		t.Fatalf("expected 1 matched target, got %d (%+v)", len(resp.MatchedTargets), resp.MatchedTargets)
	}
	mt := resp.MatchedTargets[0]
	if mt.Target != "container:worker-1" {
		t.Errorf("expected matched target container:worker-1, got %q", mt.Target)
	}
	if mt.Value != 5 {
		t.Errorf("expected matched target value 5, got %v", mt.Value)
	}
	if !strings.Contains(mt.Summary, "requires 5m sustained") {
		t.Errorf("expected sustained duration note in summary, got %q", mt.Summary)
	}
}

func TestPreviewLogRule_TargetFilteringAndPerSourceThreshold(t *testing.T) {
	srv, store, cookie, _ := setupAlertsTestServer(t)
	defer store.Close()

	sidAPI := seedTestSource(t, srv, "container", "api-1")
	sidWorker := seedTestSource(t, srv, "container", "worker-1")
	sidFile := seedTestSource(t, srv, "file", "/var/log/syslog")

	tailCache := logs.NewTailCache(10, 200)
	srv.tailCache = tailCache

	now := time.Now().UTC()

	// 3 matches in api-1, 3 matches in worker-1 (total 6, but each < threshold 5), 10 matches in file source
	for i := 0; i < 3; i++ {
		tailCache.Push(model.LogEntry{
			SourceID:  fmt.Sprintf("%d", sidAPI),
			Timestamp: now.Add(-time.Duration(i) * time.Second),
			Level:     model.LevelError,
			Message:   []byte("ERROR: panic in api"),
		})
		tailCache.Push(model.LogEntry{
			SourceID:  fmt.Sprintf("%d", sidWorker),
			Timestamp: now.Add(-time.Duration(i) * time.Second),
			Level:     model.LevelError,
			Message:   []byte("ERROR: panic in worker"),
		})
	}
	for i := 0; i < 10; i++ {
		tailCache.Push(model.LogEntry{
			SourceID:  fmt.Sprintf("%d", sidFile),
			Timestamp: now.Add(-time.Duration(i) * time.Second),
			Level:     model.LevelError,
			Message:   []byte("ERROR: panic in syslog"),
		})
	}

	// 1. Target container:* must ignore file:/var/log/syslog and not aggregate api-1 + worker-1 across sources
	resp := postRulePreview(t, srv, cookie, RulePreviewRequest{
		Kind:          "log",
		Target:        "container:*",
		Severity:      "critical",
		ConditionJSON: `{"pattern":"panic","is_regex":false,"ignore_case":true,"threshold":5,"window_sec":60}`,
		ForSec:        0,
		Channels:      []string{"email"},
	})
	if resp.WouldFireNow {
		t.Errorf("expected would_fire_now=false when file source is excluded and each container has 3 < 5 hits")
	}
	if resp.RecentMatches24h != 6 {
		t.Errorf("expected recent_matches_24h=6 (only matching container targets), got %d", resp.RecentMatches24h)
	}

	// 2. Mismatched specific target must return 0 matches and would_fire_now=false
	respMismatch := postRulePreview(t, srv, cookie, RulePreviewRequest{
		Kind:          "log",
		Target:        "container:nonexistent",
		Severity:      "critical",
		ConditionJSON: `{"pattern":"panic","is_regex":false,"ignore_case":true,"threshold":1,"window_sec":60}`,
		ForSec:        0,
		Channels:      []string{"email"},
	})
	if respMismatch.WouldFireNow {
		t.Errorf("expected would_fire_now=false for mismatched target")
	}
	if respMismatch.RecentMatches24h != 0 {
		t.Errorf("expected recent_matches_24h=0 for mismatched target, got %d", respMismatch.RecentMatches24h)
	}
	if len(respMismatch.MatchedTargets) != 0 {
		t.Errorf("expected 0 matched targets for mismatched target, got %+v", respMismatch.MatchedTargets)
	}
}
