package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	htmltmpl "html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	texttmpl "text/template"
	"time"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/cleanup"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/go-chi/chi/v5"
)

// StorageHealth reports disk consumption across chunk files and SQLite stores.
type StorageHealth struct {
	LogsDirBytes   int64          `json:"logs_dir_bytes"`
	LogsMaxBytes   int64          `json:"logs_max_bytes"`
	LogsChunkFiles int            `json:"logs_chunk_files"`
	MetricsDB      DBStorageStats `json:"metrics_db"`
	ConfigDB       DBStorageStats `json:"config_db"`
}

// DBStorageStats provides storage size metrics for one SQLite database.
type DBStorageStats struct {
	TotalBytes    int64 `json:"total_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
	FreelistBytes int64 `json:"freelist_bytes"`
}

// CleanupWarningStatus captures whether the delivery failure banner should be displayed.
type CleanupWarningStatus struct {
	Active  bool   `json:"active"`
	Message string `json:"message"`
}

// CleanupHistoryItem summarizes an execution row from cleanup_run.
type CleanupHistoryItem struct {
	ID             int64  `json:"id"`
	Trigger        string `json:"trigger"`
	DryRun         bool   `json:"dry_run"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	DurationMs     int64  `json:"duration_ms"`
	LogsDeleted    int64  `json:"logs_deleted"`
	MetricsDeleted int64  `json:"metrics_deleted"`
	EventsDeleted  int64  `json:"events_deleted"`
	BytesReclaimed int64  `json:"bytes_reclaimed"`
	ReportStatus   string `json:"report_status"`
	ReportDetail   string `json:"report_detail"`
	Status         string `json:"status"`
}

// CleanupReportDetailResponse returns stored report text and metadata for a run.
type CleanupReportDetailResponse struct {
	ID           int64  `json:"id"`
	Trigger      string `json:"trigger"`
	DryRun       bool   `json:"dry_run"`
	StartedAt    string `json:"started_at"`
	ReportFormat string `json:"report_format"`
	ReportStatus string `json:"report_status"`
	ReportBody   string `json:"report_body"`
}

// PreviewTemplateRequest defines the input payload for custom template dry-run validation.
type PreviewTemplateRequest struct {
	Format   string `json:"format"`
	Template string `json:"template"`
}

func (s *Server) inspectStorage(ctx context.Context) (*StorageHealth, *CleanupWarningStatus) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	storage := &StorageHealth{}

	if cfg != nil {
		logsDir := filepath.Join(cfg.Storage.Path, "logs")
		_ = filepath.Walk(logsDir, func(_ string, info os.FileInfo, _ error) error {
			if info != nil && !info.IsDir() {
				storage.LogsDirBytes += info.Size()
				if filepath.Ext(info.Name()) == ".chunk" {
					storage.LogsChunkFiles++
				}
			}
			return nil
		})

		if maxBytes, err := config.ParseBytes(cfg.Storage.LogsMaxSize); err == nil {
			storage.LogsMaxBytes = maxBytes
		}
	}

	if s.metricsDB != nil {
		if stats, err := cleanup.InspectHandle(ctx, s.metricsDB); err == nil {
			storage.MetricsDB = DBStorageStats{
				TotalBytes:    stats.TotalBytes,
				WALBytes:      stats.WALBytes,
				FreelistBytes: stats.FreelistCount * stats.PageSize,
			}
		}
	}

	if s.configDB != nil {
		if stats, err := cleanup.InspectHandle(ctx, s.configDB); err == nil {
			storage.ConfigDB = DBStorageStats{
				TotalBytes:    stats.TotalBytes,
				WALBytes:      stats.WALBytes,
				FreelistBytes: stats.FreelistCount * stats.PageSize,
			}
		}
	}

	warning := &CleanupWarningStatus{
		Active: false,
	}

	if s.deliverer != nil && s.deliverer.IsWarningActive() {
		warning.Active = true
		warning.Message = "Last pre-cleanup report delivery failed — view cleanup history"
	}

	return storage, warning
}

func (s *Server) handleCleanupHistory(w http.ResponseWriter, r *http.Request) {
	if s.configDB == nil {
		auth.WriteJSON(w, http.StatusOK, []CleanupHistoryItem{})
		return
	}

	limit := 50
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			limit = l
			if limit > 200 {
				limit = 200
			}
		}
	}

	query := `
		SELECT id, trigger, dry_run, started_at, finished_at,
		       logs_deleted, metrics_deleted, events_deleted, bytes_reclaimed,
		       report_status, report_detail, status
		FROM cleanup_run
		ORDER BY id DESC
		LIMIT ?;
	`

	rows, err := s.configDB.RO.QueryContext(r.Context(), query, limit)
	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", fmt.Sprintf("failed to query history: %v", err))
		return
	}
	defer rows.Close()

	items := make([]CleanupHistoryItem, 0)
	for rows.Next() {
		var (
			item                     CleanupHistoryItem
			dryRunInt                int
			startedUnix, finishedUnix int64
		)

		err := rows.Scan(
			&item.ID, &item.Trigger, &dryRunInt, &startedUnix, &finishedUnix,
			&item.LogsDeleted, &item.MetricsDeleted, &item.EventsDeleted, &item.BytesReclaimed,
			&item.ReportStatus, &item.ReportDetail, &item.Status,
		)
		if err != nil {
			continue
		}

		item.DryRun = (dryRunInt == 1)
		startedTime := time.Unix(startedUnix, 0).UTC()
		finishedTime := time.Unix(finishedUnix, 0).UTC()
		item.StartedAt = startedTime.Format(time.RFC3339)
		item.FinishedAt = finishedTime.Format(time.RFC3339)
		item.DurationMs = finishedTime.Sub(startedTime).Milliseconds()
		if item.DurationMs < 0 {
			item.DurationMs = 0
		}

		items = append(items, item)
	}

	auth.WriteJSON(w, http.StatusOK, items)
}

func (s *Server) handleCleanupReport(w http.ResponseWriter, r *http.Request) {
	if s.configDB == nil {
		auth.WriteJSONError(w, http.StatusNotFound, "not_found", "database not available")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_id", "invalid cleanup run id")
		return
	}

	query := `
		SELECT id, trigger, dry_run, started_at, report_status, report_body
		FROM cleanup_run
		WHERE id = ?;
	`

	var (
		resp        CleanupReportDetailResponse
		dryRunInt   int
		startedUnix int64
	)

	err = s.configDB.RO.QueryRowContext(r.Context(), query, id).Scan(
		&resp.ID, &resp.Trigger, &dryRunInt, &startedUnix,
		&resp.ReportStatus, &resp.ReportBody,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "report not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", err.Error())
		return
	}

	resp.DryRun = (dryRunInt == 1)
	resp.StartedAt = time.Unix(startedUnix, 0).UTC().Format(time.RFC3339)
	if strings.Contains(resp.ReportBody, "<html") || strings.HasPrefix(strings.TrimSpace(resp.ReportBody), "<!DOCTYPE") {
		resp.ReportFormat = "html"
	} else {
		resp.ReportFormat = "markdown"
	}

	auth.WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCleanupRun(w http.ResponseWriter, r *http.Request) {
	if s.cleanupManager == nil {
		auth.WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "cleanup manager not initialized")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "true"
	result, err := s.cleanupManager.Run(r.Context(), "manual", dryRun)
	if err != nil {
		if errors.Is(err, cleanup.ErrCleanupAlreadyRunning) {
			auth.WriteJSONError(w, http.StatusConflict, "cleanup_in_progress", "a cleanup run is already active")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "execution_error", err.Error())
		return
	}

	auth.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) handleCleanupTemplateDownload(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="tarsier-report-default.html"`)
		_, _ = w.Write([]byte(cleanup.DefaultHTMLTemplate))
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tarsier-report-default.md"`)
	_, _ = w.Write([]byte(cleanup.DefaultMarkdownTemplate))
}

func (s *Server) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	var req PreviewTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "bad_request", "invalid json body")
		return
	}

	format := strings.ToLower(req.Format)
	if format == "" {
		format = "markdown"
	}

	// Validate template candidate
	var rendered string
	report := cleanup.PreCleanupReport{
		GeneratedAt:       time.Now().UTC(),
		DryRun:            true,
		WindowStart:       time.Now().UTC().Add(-7 * 24 * time.Hour),
		WindowEnd:         time.Now().UTC(),
		TotalLogsPurged:   1250,
		TotalErrors:       14,
		TotalWarnings:     6,
		TotalBytesReclaim: 1572864,
		AlertsFired:       1,
		Sources: []cleanup.SourceLogDigest{
			{SourceID: 1, SourceName: "api-service", FilesCount: 2, TotalLines: 1250, ErrorLines: 14, WarnLines: 6, BytesReclaim: 1572864},
		},
		TopErrorPatterns: []cleanup.PatternSummary{
			{Pattern: "database connection failed at {ip}:{num}", Count: 14, SampleLine: "database connection failed at 10.0.1.5:5432: timeout", SourceName: "api-service"},
		},
	}

	if s.cleanupManager != nil {
		if plan, err := s.cleanupManager.ComputeRetentionPlan(time.Now().UTC(), true); err == nil && s.reportBuilder != nil {
			if liveRep, rErr := s.reportBuilder.Build(r.Context(), plan); rErr == nil {
				report = liveRep
			}
		}
	}

	funcMap := texttmpl.FuncMap{
		"formatBytes": cleanup.FormatBytes,
		"add": func(a, b int) int {
			return a + b
		},
	}

	var buf bytes.Buffer
	if format == "html" {
		tmpl, err := htmltmpl.New("preview_html").Funcs(htmltmpl.FuncMap(funcMap)).Parse(req.Template)
		if err != nil {
			auth.WriteJSONError(w, http.StatusBadRequest, "template_syntax_error", fmt.Sprintf("html template syntax error: %v", err))
			return
		}
		if err := tmpl.Execute(&buf, report); err != nil {
			auth.WriteJSONError(w, http.StatusBadRequest, "template_execution_error", err.Error())
			return
		}
	} else {
		tmpl, err := texttmpl.New("preview_md").Funcs(funcMap).Parse(req.Template)
		if err != nil {
			auth.WriteJSONError(w, http.StatusBadRequest, "template_syntax_error", fmt.Sprintf("markdown template syntax error: %v", err))
			return
		}
		if err := tmpl.Execute(&buf, report); err != nil {
			auth.WriteJSONError(w, http.StatusBadRequest, "template_execution_error", err.Error())
			return
		}
	}

	rendered = buf.String()
	auth.WriteJSON(w, http.StatusOK, map[string]string{
		"rendered": rendered,
	})
}

func (s *Server) handleSystemReload(w http.ResponseWriter, r *http.Request) {
	configPath := s.configPath
	if configPath == "" {
		configPath = config.ResolveConfigPath("")
	}

	newCfg, err := config.Load(configPath)
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "config_invalid", fmt.Sprintf("configuration reload failed: %v", err))
		return
	}

	s.mu.Lock()
	s.cfg = newCfg
	s.mu.Unlock()

	if s.queryCache != nil {
		s.queryCache.Invalidate()
	}

	auth.WriteJSON(w, http.StatusOK, newCfg.Sanitized())
}
