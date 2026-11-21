package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/cleanup"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
)

func setupCleanupTestServer(t *testing.T) (*Server, *db.Store, *http.Cookie, string, *cleanup.CleanupManager, *cleanup.ReportDeliverer) {
	t.Helper()
	srv, store, cookie := setupMetricsTestServer(t, nil)

	dataDir := store.Config.Path
	baseDir := filepath.Dir(dataDir)

	configFilePath := filepath.Join(baseDir, "tarsier.yaml")
	cfgContent := `
server:
  listen: ":8080"
  timezone: "UTC"
storage:
  path: "` + baseDir + `"
  logs_max_size: "500MB"
retention:
  logs: "7d"
  metrics_raw: "7d"
`
	if err := os.WriteFile(configFilePath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("failed to write test config file: %v", err)
	}

	cfg, err := config.Load(configFilePath)
	if err != nil {
		t.Fatalf("failed to load test config: %v", err)
	}

	deliverer := cleanup.NewReportDeliverer(cfg.Cleanup.Report, nil)
	maintainer := cleanup.NewDatabaseMaintainer(cleanup.DatabaseMaintainerConfig{
		Store: store,
	})
	tmplEngine, _ := cleanup.NewTemplateEngine("", "")
	reportBuilder, err := cleanup.NewReportBuilder(cleanup.ReportBuilderConfig{
		DataDir:   baseDir,
		MetricsRO: store.Metrics.RO,
		ConfigRO:  store.Config.RO,
	})
	if err != nil {
		t.Fatalf("failed to create report builder: %v", err)
	}

	cleanupMgr, err := cleanup.NewCleanupManager(cleanup.CleanupManagerConfig{
		Config:         cfg,
		Store:          store,
		ReportBuilder:  reportBuilder,
		TemplateEngine: tmplEngine,
		Deliverer:      deliverer,
		Maintainer:     maintainer,
	})
	if err != nil {
		t.Fatalf("failed to create cleanup manager: %v", err)
	}

	srv.mu.Lock()
	srv.cfg = cfg
	srv.cleanupManager = cleanupMgr
	srv.reportBuilder = reportBuilder
	srv.deliverer = deliverer
	srv.maintainer = maintainer
	srv.configPath = configFilePath
	srv.mu.Unlock()

	return srv, store, cookie, configFilePath, cleanupMgr, deliverer
}

func TestCleanupAPI_DryRunPreviewDoesNotDeleteData(t *testing.T) {
	srv, store, cookie, _, _, _ := setupCleanupTestServer(t)

	// Create an expired log chunk on disk: /data/logs/1/0000000000000001.chunk
	logsDir := filepath.Join(srv.cfg.Storage.Path, "logs", "1")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("mkdir logs failed: %v", err)
	}
	chunkPath := filepath.Join(logsDir, "0000000000000001.chunk")
	idxPath := filepath.Join(logsDir, "0000000000000001.idx")
	if err := os.WriteFile(chunkPath, []byte("fake-chunk-bytes"), 0644); err != nil {
		t.Fatalf("write chunk failed: %v", err)
	}
	if err := os.WriteFile(idxPath, []byte("fake-idx-bytes"), 0644); err != nil {
		t.Fatalf("write idx failed: %v", err)
	}

	// Insert an expired metric sample in metrics.db
	expiredSec := time.Now().UTC().Add(-30 * 24 * time.Hour).Unix()
	err := store.Metrics.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO series(id, entity, metric) VALUES(1, 'host', 'cpu');")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sample_raw(series_id, ts, v) VALUES(1, ?, 42.0);", expiredSec)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert sample_raw: %v", err)
	}

	// Execute Dry-Run via API: POST /api/v1/system/cleanup/run?dry_run=true
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/cleanup/run?dry_run=true", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var res cleanup.CleanupRunResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}

	if !res.DryRun {
		t.Errorf("expected dry_run=true in response, got false")
	}

	// Verify log files STILL exist on disk (unmodified)
	if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
		t.Errorf("chunk file %s was deleted during dry run!", chunkPath)
	}
	if _, err := os.Stat(idxPath); os.IsNotExist(err) {
		t.Errorf("idx file %s was deleted during dry run!", idxPath)
	}

	// Verify sample_raw row STILL exists in SQLite
	var count int
	_ = store.Metrics.RO.QueryRow("SELECT COUNT(*) FROM sample_raw WHERE series_id=1 AND ts=?", expiredSec).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 sample_raw row remaining, got %d", count)
	}

	// Verify cleanup_run audit row has dry_run=1
	var dbDryRun int
	err = store.Config.RO.QueryRow("SELECT dry_run FROM cleanup_run WHERE id=?", res.ID).Scan(&dbDryRun)
	if err != nil {
		t.Fatalf("failed to query cleanup_run row: %v", err)
	}
	if dbDryRun != 1 {
		t.Errorf("expected cleanup_run dry_run=1, got %d", dbDryRun)
	}
}

func TestCleanupAPI_HistoryAndStoredReportRetrieval(t *testing.T) {
	srv, store, cookie, _, _, _ := setupCleanupTestServer(t)

	// Insert mock history row
	nowUnix := time.Now().UTC().Unix()
	err := store.Config.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		query := `
			INSERT INTO cleanup_run (
				trigger, dry_run, started_at, finished_at,
				logs_deleted, metrics_deleted, events_deleted, bytes_reclaimed,
				report_status, report_detail, report_body, status
			) VALUES (
				'scheduled', 0, ?, ?,
				10, 50, 2, 1024,
				'sent', 'discord:ok', '# Test Report Body', 'completed'
			);
		`
		_, execErr := tx.ExecContext(ctx, query, nowUnix-5, nowUnix)
		return execErr
	})
	if err != nil {
		t.Fatalf("failed to insert mock cleanup_run: %v", err)
	}

	// 1. GET /api/v1/system/cleanup/history
	reqHist := httptest.NewRequest(http.MethodGet, "/api/v1/system/cleanup/history", nil)
	reqHist.AddCookie(cookie)
	recHist := httptest.NewRecorder()

	srv.Router().ServeHTTP(recHist, reqHist)

	if recHist.Code != http.StatusOK {
		t.Fatalf("history: expected 200 OK, got %d (%s)", recHist.Code, recHist.Body.String())
	}

	var items []CleanupHistoryItem
	if err := json.NewDecoder(recHist.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode history: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("expected at least 1 history item, got 0")
	}

	targetID := items[0].ID

	// 2. GET /api/v1/system/cleanup/{id}/report
	reqRep := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/system/cleanup/%d/report", targetID), nil)
	reqRep.AddCookie(cookie)
	recRep := httptest.NewRecorder()

	srv.Router().ServeHTTP(recRep, reqRep)

	if recRep.Code != http.StatusOK {
		t.Fatalf("report: expected 200 OK, got %d (%s)", recRep.Code, recRep.Body.String())
	}

	var reportResp CleanupReportDetailResponse
	if err := json.NewDecoder(recRep.Body).Decode(&reportResp); err != nil {
		t.Fatalf("failed to decode report response: %v", err)
	}

	if reportResp.ReportBody != "# Test Report Body" {
		t.Errorf("unexpected report body: %q", reportResp.ReportBody)
	}
	if reportResp.ReportFormat != "text" {
		t.Errorf("expected format text, got %s", reportResp.ReportFormat)
	}

	// 3. GET /api/v1/system/cleanup/99999/report -> 404
	req404 := httptest.NewRequest(http.MethodGet, "/api/v1/system/cleanup/99999/report", nil)
	req404.AddCookie(cookie)
	rec404 := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %d", rec404.Code)
	}
}

func TestCleanupAPI_TemplateDownloadAndCustomPreview(t *testing.T) {
	srv, _, cookie, _, _, _ := setupCleanupTestServer(t)

	// 1. Download Text template
	reqText := httptest.NewRequest(http.MethodGet, "/api/v1/system/cleanup/template?format=text", nil)
	reqText.AddCookie(cookie)
	recText := httptest.NewRecorder()

	srv.Router().ServeHTTP(recText, reqText)
	if recText.Code != http.StatusOK {
		t.Fatalf("download text: expected 200, got %d", recText.Code)
	}
	if ct := recText.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("expected text/plain; charset=utf-8, got %q", ct)
	}
	if !bytes.Contains(recText.Body.Bytes(), []byte("Tarsier Pre-Cleanup Report")) {
		t.Errorf("expected default text content, got: %s", recText.Body.String())
	}

	// 2. Download HTML template
	reqHTML := httptest.NewRequest(http.MethodGet, "/api/v1/system/cleanup/template?format=html", nil)
	reqHTML.AddCookie(cookie)
	recHTML := httptest.NewRecorder()

	srv.Router().ServeHTTP(recHTML, reqHTML)
	if recHTML.Code != http.StatusOK {
		t.Fatalf("download html: expected 200, got %d", recHTML.Code)
	}
	if !bytes.Contains(recHTML.Body.Bytes(), []byte("<!DOCTYPE html>")) {
		t.Errorf("expected default html content, got: %s", recHTML.Body.String())
	}

	// 3. Live Preview valid custom template
	bodyJSON := []byte(`{"format":"text","template":"Purged count: {{ .TotalLogsPurged }} lines"}`)
	reqPrev := httptest.NewRequest(http.MethodPost, "/api/v1/system/cleanup/preview", bytes.NewReader(bodyJSON))
	reqPrev.AddCookie(cookie)
	recPrev := httptest.NewRecorder()

	srv.Router().ServeHTTP(recPrev, reqPrev)
	if recPrev.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d (%s)", recPrev.Code, recPrev.Body.String())
	}

	var prevResp map[string]string
	_ = json.NewDecoder(recPrev.Body).Decode(&prevResp)
	if !bytes.Contains([]byte(prevResp["rendered"]), []byte("Purged count:")) {
		t.Errorf("unexpected preview output: %s", prevResp["rendered"])
	}

	// 4. Live Preview invalid syntax template -> 400
	badJSON := []byte(`{"format":"text","template":"Broken template {{ .TotalLogsPurged "}`)
	reqBad := httptest.NewRequest(http.MethodPost, "/api/v1/system/cleanup/preview", bytes.NewReader(badJSON))
	reqBad.AddCookie(cookie)
	recBad := httptest.NewRecorder()

	srv.Router().ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad template syntax, got %d", recBad.Code)
	}
}

func TestCleanupAPI_ConfigReload(t *testing.T) {
	srv, _, cookie, configPath, _, _ := setupCleanupTestServer(t)

	// Valid config reload
	newConfig := `
server:
  listen: ":9090"
  timezone: "UTC"
storage:
  path: "` + srv.cfg.Storage.Path + `"
  logs_max_size: "1GiB"
retention:
  logs: "14d"
  metrics_raw: "7d"
`
	if err := os.WriteFile(configPath, []byte(newConfig), 0644); err != nil {
		t.Fatalf("failed to update config file: %v", err)
	}

	reqReload := httptest.NewRequest(http.MethodPost, "/api/v1/system/reload", nil)
	reqReload.AddCookie(cookie)
	recReload := httptest.NewRecorder()

	srv.Router().ServeHTTP(recReload, reqReload)
	if recReload.Code != http.StatusOK {
		t.Fatalf("reload: expected 200 OK, got %d (%s)", recReload.Code, recReload.Body.String())
	}

	srv.mu.RLock()
	activeListen := srv.cfg.Server.Listen
	srv.mu.RUnlock()
	if activeListen != ":9090" {
		t.Errorf("expected reloaded listen :9090, got %s", activeListen)
	}

	// Invalid config reload (syntax error)
	if err := os.WriteFile(configPath, []byte("invalid: [yaml: broken"), 0644); err != nil {
		t.Fatalf("failed to write corrupted config: %v", err)
	}

	reqCorrupt := httptest.NewRequest(http.MethodPost, "/api/v1/system/reload", nil)
	reqCorrupt.AddCookie(cookie)
	recCorrupt := httptest.NewRecorder()

	srv.Router().ServeHTTP(recCorrupt, reqCorrupt)
	if recCorrupt.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for corrupted config, got %d", recCorrupt.Code)
	}

	// Assert original config retained
	srv.mu.RLock()
	retainedListen := srv.cfg.Server.Listen
	srv.mu.RUnlock()
	if retainedListen != ":9090" {
		t.Errorf("expected retained listen :9090, got %s", retainedListen)
	}
}

func TestCleanupAPI_StorageHealthAndWarning(t *testing.T) {
	srv, _, cookie, _, _, deliverer := setupCleanupTestServer(t)

	// Normal health
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/health", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health: expected 200 OK, got %d", rec.Code)
	}

	var health SystemHealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&health); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}

	if health.Storage == nil {
		t.Fatalf("expected Storage to be populated in health response")
	}
	if health.CleanupWarning == nil || health.CleanupWarning.Active {
		t.Errorf("expected cleanup warning inactive initially, got %+v", health.CleanupWarning)
	}

	// Trigger warning
	deliverer.SetWarningActive(true)

	reqWarn := httptest.NewRequest(http.MethodGet, "/api/v1/system/health", nil)
	reqWarn.AddCookie(cookie)
	recWarn := httptest.NewRecorder()

	srv.Router().ServeHTTP(recWarn, reqWarn)
	var warnHealth SystemHealthResponse
	_ = json.NewDecoder(recWarn.Body).Decode(&warnHealth)

	if warnHealth.CleanupWarning == nil || !warnHealth.CleanupWarning.Active {
		t.Errorf("expected cleanup warning active, got %+v", warnHealth.CleanupWarning)
	}
}
