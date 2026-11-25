package cleanup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
)

var (
	// ErrCleanupAlreadyRunning is returned when a cleanup run is already executing.
	ErrCleanupAlreadyRunning = errors.New("a cleanup run is already active")
)

// PostCleanupMaintainer executes database compaction and WAL truncation after prunes.
type PostCleanupMaintainer interface {
	MaintainAll(ctx context.Context) (MaintenanceSummary, error)
}

// CleanupRunResult captures the outcome and audit summary of a cleanup or dry-run.
type CleanupRunResult struct {
	ID             int64             `json:"id"`
	Trigger        string            `json:"trigger"` // "scheduled" | "manual" | "emergency"
	DryRun         bool              `json:"dry_run"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     time.Time         `json:"finished_at"`
	DurationMs     int64             `json:"duration_ms"`
	LogsDeleted    int64             `json:"logs_deleted"`
	MetricsDeleted int64             `json:"metrics_deleted"`
	EventsDeleted  int64             `json:"events_deleted"`
	BytesReclaimed int64             `json:"bytes_reclaimed"`
	ReportStatus   string            `json:"report_status"` // "sent" | "partial" | "failed" | "skipped"
	ReportDetail   string            `json:"report_detail"`
	ReportFormat   string            `json:"report_format"`
	ReportBody     string            `json:"report_body"`
	Status         string            `json:"status"` // "completed" | "blocked" | "failed"
	Report         *PreCleanupReport `json:"report,omitempty"`
	Delivery       *DeliverySummary  `json:"delivery,omitempty"`
}

// CleanupManagerConfig encapsulates dependencies for the end-to-end CleanupManager.
type CleanupManagerConfig struct {
	Config         *config.Config
	Store          *db.Store
	ReportBuilder  *ReportBuilder
	TemplateEngine *TemplateEngine
	Deliverer      *ReportDeliverer
	Maintainer     PostCleanupMaintainer
	BatchSize      int
	BatchPause     time.Duration
	Sleeper        func(context.Context, time.Duration) error
}

// CleanupManager coordinates planning, reporting, delivery, pruning, and compaction.
type CleanupManager struct {
	cfg            *config.Config
	store          *db.Store
	reportBuilder  *ReportBuilder
	templateEngine *TemplateEngine
	deliverer      *ReportDeliverer
	maintainer     PostCleanupMaintainer
	executor       *RetentionExecutor
	running        atomic.Bool
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewCleanupManager creates an initialized CleanupManager.
func NewCleanupManager(cfg CleanupManagerConfig) (*CleanupManager, error) {
	if cfg.Config == nil {
		cfg.Config = config.NewDefaultConfig()
	}

	dataDir := cfg.Config.Storage.Path
	if dataDir == "" {
		dataDir = "/data"
	}

	var metricsHandle, configHandle *db.Handle
	if cfg.Store != nil {
		metricsHandle = cfg.Store.Metrics
		configHandle = cfg.Store.Config
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = cfg.Config.Cleanup.BatchSize
	}
	if batchSize <= 0 {
		batchSize = 5000
	}

	batchPause := cfg.BatchPause
	if batchPause <= 0 && cfg.Config.Cleanup.BatchPause != "" {
		if d, err := config.ParseDuration(cfg.Config.Cleanup.BatchPause); err == nil && d > 0 {
			batchPause = d
		}
	}
	if batchPause <= 0 {
		batchPause = 50 * time.Millisecond
	}

	executor := NewRetentionExecutor(dataDir, metricsHandle, configHandle, batchSize, batchPause)
	if cfg.Sleeper != nil {
		executor.SetSleeper(cfg.Sleeper)
	}

	return &CleanupManager{
		cfg:            cfg.Config,
		store:          cfg.Store,
		reportBuilder:  cfg.ReportBuilder,
		templateEngine: cfg.TemplateEngine,
		deliverer:      cfg.Deliverer,
		maintainer:     cfg.Maintainer,
		executor:       executor,
		stopCh:         make(chan struct{}),
	}, nil
}

// FormatSummaryBody formats the concise plain-text summary body for notification emails and Discord embeds.
func FormatSummaryBody(report PreCleanupReport, trigger string) string {
	mode := "Auto"
	if strings.EqualFold(trigger, "manual") {
		mode = "Manual"
	}

	return fmt.Sprintf(
		"Generated : %s\n"+
			"Mode : %s\n"+
			"Cleanup Window: From %s To %s\n\n"+
			"Summary:\n"+
			"Logs Lines Purged: %d\n"+
			"    Errors: %d\n"+
			"    Warnings: %d\n\n"+
			"Disk Space Freed: %s\n"+
			"Alerts Fired: %d",
		report.GeneratedAt.Format("2006-01-02 15:04:05 MST"),
		mode,
		report.WindowStart.Format("2006-01-02 15:04:05"),
		report.WindowEnd.Format("2006-01-02 15:04:05 MST"),
		report.TotalLogsPurged,
		report.TotalErrors,
		report.TotalWarnings,
		FormatBytes(report.TotalBytesReclaim),
		report.AlertsFired,
	)
}

// Run executes the complete 7-step cleanup lifecycle.
func (m *CleanupManager) Run(ctx context.Context, trigger string, dryRun bool) (CleanupRunResult, error) {
	if !m.running.CompareAndSwap(false, true) {
		return CleanupRunResult{}, ErrCleanupAlreadyRunning
	}
	defer m.running.Store(false)

	startedAt := time.Now().UTC()
	log.Printf("[cleanup] [%s] cycle started (dry_run=%v)", trigger, dryRun)

	result := CleanupRunResult{
		Trigger:      trigger,
		DryRun:       dryRun,
		StartedAt:    startedAt,
		Status:       "failed",
		ReportStatus: "skipped",
	}

	// 1. Build RetentionPlan
	plan, err := m.ComputeRetentionPlan(startedAt, dryRun)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] planning failed: %v", trigger, err)
		return result, fmt.Errorf("plan error: %w", err)
	}
	log.Printf("[cleanup] [%s] plan computed: %d expired chunk files discovered", trigger, len(plan.ExpiredChunks))

	// 2. Build PreCleanupReport
	var report PreCleanupReport
	if m.reportBuilder != nil {
		report, err = m.reportBuilder.Build(ctx, plan)
		if err != nil {
			result.FinishedAt = time.Now().UTC()
			result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
			_ = m.persistRunRecord(ctx, &result)
			log.Printf("[cleanup] [%s] report generation failed: %v", trigger, err)
			return result, fmt.Errorf("build report error: %w", err)
		}
	} else {
		report = PreCleanupReport{
			GeneratedAt: startedAt,
			DryRun:      dryRun,
		}
	}
	result.Report = &report
	log.Printf("[cleanup] [%s] pre-cleanup report generated: %d lines slated for purge (%s across %d sources)",
		trigger, report.TotalLogsPurged, FormatBytes(report.TotalBytesReclaim), len(report.Sources))

	// Render report
	format := strings.ToLower(strings.TrimSpace(m.cfg.Cleanup.Report.Format))
	if format != "html" {
		format = "text"
	}
	result.ReportFormat = format

	var reportBytes []byte
	if m.templateEngine != nil {
		if format == "html" {
			reportBytes, _, _ = m.templateEngine.RenderHTML(report, 0)
		} else {
			reportBytes, _, _ = m.templateEngine.RenderText(report, 0)
		}
	} else {
		reportBytes = []byte(fmt.Sprintf("Pre-cleanup report: %d logs slated for purge", report.TotalLogsPurged))
	}
	result.ReportBody = string(reportBytes)

	// 3. Deliver Report (if !dryRun and Deliverer configured)
	if !dryRun && m.deliverer != nil {
		ext := "txt"
		if format == "html" {
			ext = "html"
		}
		summaryBody := FormatSummaryBody(report, trigger)
		payload := ReportPayload{
			Report:       report,
			Format:       format,
			Filename:     fmt.Sprintf("tarsier-precleanup-%s.%s", startedAt.Format("2006-01-02"), ext),
			BodyText:     summaryBody,
			Attachment:   reportBytes,
			SummaryTitle: fmt.Sprintf("Tarsier Pre-Cleanup Report (%s)", trigger),
			SummaryBody:  summaryBody,
		}

		delivery, dErr := m.deliverer.Deliver(ctx, payload, trigger == "emergency")
		result.Delivery = &delivery
		result.ReportStatus = delivery.OverallStatus
		if len(delivery.Outcomes) > 0 {
			var details []string
			for _, o := range delivery.Outcomes {
				if o.LastError != "" {
					details = append(details, fmt.Sprintf("%s: %s", o.Channel, o.LastError))
				}
			}
			result.ReportDetail = strings.Join(details, "; ")
		}

		if dErr != nil || !delivery.ShouldProceed {
			result.Status = "blocked"
			result.FinishedAt = time.Now().UTC()
			result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
			_ = m.persistRunRecord(ctx, &result)
			log.Printf("[cleanup] [%s] execution blocked by delivery policy (overall_status=%s, should_proceed=%v, details=%q)",
				trigger, result.ReportStatus, delivery.ShouldProceed, result.ReportDetail)
			return result, nil
		}
	}

	// 4. Dry-Run Exit
	if dryRun {
		result.Status = "completed"
		result.LogsDeleted = report.TotalLogsPurged
		result.BytesReclaimed = report.TotalBytesReclaim
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] [dry-run] simulated run complete: would reclaim %s across %d lines (duration=%dms)",
			trigger, FormatBytes(result.BytesReclaimed), result.LogsDeleted, result.DurationMs)
		return result, nil
	}

	// 5. Execute Deletions
	// A. O(1) chunk unlinks
	logsDeleted, bytesReclaimed, err := m.executor.DeleteExpiredChunks(plan.ExpiredChunks)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] chunk deletion failed: %v", trigger, err)
		return result, fmt.Errorf("delete expired chunks: %w", err)
	}
	result.LogsDeleted = logsDeleted
	result.BytesReclaimed = bytesReclaimed
	log.Printf("[cleanup] [%s] unlinked %d expired chunks: %d lines deleted, %s reclaimed",
		trigger, len(plan.ExpiredChunks), logsDeleted, FormatBytes(bytesReclaimed))

	// B. Emergency size-cap check
	if logsMaxSize, pErr := config.ParseBytes(m.cfg.Storage.LogsMaxSize); pErr == nil && logsMaxSize > 0 {
		todayUTC := startedAt.Format("2006-01-02")
		_, capBytes, capLines, _ := m.executor.EnforceLogsSizeCap(logsMaxSize, todayUTC)
		result.BytesReclaimed += capBytes
		result.LogsDeleted += capLines
		if capBytes > 0 || capLines > 0 {
			log.Printf("[cleanup] [%s] logs size cap enforced: reclaimed %s across %d lines",
				trigger, FormatBytes(capBytes), capLines)
		}
	}

	// C. Prune Metrics DB rows
	metricsPruned, err := m.executor.PruneMetrics(ctx, plan)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] metrics prune failed: %v", trigger, err)
		return result, fmt.Errorf("prune metrics: %w", err)
	}
	result.MetricsDeleted = metricsPruned

	// D. Prune Config DB rows
	eventsPruned, err := m.executor.PruneEvents(ctx, plan)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] events prune failed: %v", trigger, err)
		return result, fmt.Errorf("prune events: %w", err)
	}
	result.EventsDeleted = eventsPruned

	containersPruned, err := m.executor.PruneExitedContainers(ctx, plan.ContainersCutoffTS)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		_ = m.persistRunRecord(ctx, &result)
		log.Printf("[cleanup] [%s] exited containers prune failed: %v", trigger, err)
		return result, fmt.Errorf("prune exited containers: %w", err)
	}
	log.Printf("[cleanup] [%s] database rows pruned: %d metrics rows, %d container event rows, %d exited container rows",
		trigger, metricsPruned, eventsPruned, containersPruned)

	// 6. Post-Cleanup Database Maintenance
	if m.maintainer != nil {
		maintStart := time.Now()
		log.Printf("[cleanup] [%s] running database compaction & maintenance (vacuum, optimize, checkpoint)...", trigger)
		_, _ = m.maintainer.MaintainAll(ctx)
		log.Printf("[cleanup] [%s] database maintenance completed in %v", trigger, time.Since(maintStart))
	}

	// 7. Record completed run
	result.Status = "completed"
	result.FinishedAt = time.Now().UTC()
	result.DurationMs = result.FinishedAt.Sub(startedAt).Milliseconds()
	_ = m.persistRunRecord(ctx, &result)

	log.Printf("[cleanup] [%s] cycle finished in %dms (status=%s, logs_deleted=%d, metrics_deleted=%d, bytes_reclaimed=%s)",
		trigger, result.DurationMs, result.Status, result.LogsDeleted, result.MetricsDeleted, FormatBytes(result.BytesReclaimed))

	return result, nil
}

// ComputeRetentionPlan computes cutoffs and discovers expired chunks from config.
func (m *CleanupManager) ComputeRetentionPlan(now time.Time, dryRun bool) (RetentionPlan, error) {
	plan := RetentionPlan{
		GeneratedAt: now,
		DryRun:      dryRun,
	}

	// 1. Logs cutoff
	if m.cfg.Retention.Logs != "" {
		d, err := config.ParseDuration(m.cfg.Retention.Logs)
		if err == nil && d > 0 {
			plan.LogsCutoffTS = now.Add(-d).UnixNano()
		}
	}

	// 2. Metrics Raw cutoff
	if m.cfg.Retention.MetricsRaw != "" {
		d, err := config.ParseDuration(m.cfg.Retention.MetricsRaw)
		if err == nil && d > 0 {
			plan.RawCutoffTS = now.Add(-d).Unix()
		}
	}

	// 3. Rollup 1m cutoff
	if m.cfg.Retention.Metrics1m != "" {
		d, err := config.ParseDuration(m.cfg.Retention.Metrics1m)
		if err == nil && d > 0 {
			plan.Rollup1mCutoff = now.Add(-d).Unix()
		}
	}

	// 4. Rollup 5m cutoff
	if m.cfg.Retention.Metrics5m != "" {
		d, err := config.ParseDuration(m.cfg.Retention.Metrics5m)
		if err == nil && d > 0 {
			plan.Rollup5mCutoff = now.Add(-d).Unix()
		}
	}

	// 5. Events cutoff (defaults to 30d or Rollup5m cutoff)
	if plan.Rollup5mCutoff > 0 {
		plan.EventsCutoffTS = plan.Rollup5mCutoff
	} else {
		plan.EventsCutoffTS = now.Add(-30 * 24 * time.Hour).Unix()
	}

	// 6. Exited containers cutoff (defaults to 24h)
	plan.ContainersCutoffTS = m.containersCutoffTS(now)

	// Discover expired chunk files
	sourcesMap := make(map[int64]string)
	if m.store != nil && m.store.Config != nil {
		rows, err := m.store.Config.RO.Query("SELECT id, name FROM source;")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err == nil {
					sourcesMap[id] = name
				}
			}
		}
	}

	logsDir := filepath.Join(m.cfg.Storage.Path, "logs")
	chunks, err := DiscoverExpiredChunks(logsDir, plan.LogsCutoffTS, sourcesMap)
	if err == nil {
		plan.ExpiredChunks = chunks
	}

	return plan, nil
}

func (m *CleanupManager) persistRunRecord(ctx context.Context, res *CleanupRunResult) error {
	if m.store == nil || m.store.Config == nil {
		return nil
	}

	dryRunInt := 0
	if res.DryRun {
		dryRunInt = 1
	}

	return m.store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		query := `
			INSERT INTO cleanup_run (
				trigger, dry_run, started_at, finished_at,
				logs_deleted, metrics_deleted, events_deleted, bytes_reclaimed,
				report_status, report_detail, report_body, status
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
		`
		execRes, err := tx.ExecContext(txCtx, query,
			res.Trigger, dryRunInt, res.StartedAt.Unix(), res.FinishedAt.Unix(),
			res.LogsDeleted, res.MetricsDeleted, res.EventsDeleted, res.BytesReclaimed,
			res.ReportStatus, res.ReportDetail, res.ReportBody, res.Status,
		)
		if err != nil {
			return err
		}
		id, err := execRes.LastInsertId()
		if err == nil {
			res.ID = id
		}
		return nil
	})
}

// containersCutoffTS returns the Unix seconds cutoff for pruning exited containers.
func (m *CleanupManager) containersCutoffTS(now time.Time) int64 {
	d := 24 * time.Hour
	if m.cfg != nil && m.cfg.Retention.ContainersExited != "" {
		if parsed, err := config.ParseDuration(m.cfg.Retention.ContainersExited); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return now.Add(-d).Unix()
}

// Start launches the background size-cap watchdog and scheduled cleanup timers.
func (m *CleanupManager) Start(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		watchdogTicker := time.NewTicker(1 * time.Minute)
		defer watchdogTicker.Stop()

		for {
			select {
			case <-m.stopCh:
				return
			case <-ctx.Done():
				return
			case now := <-watchdogTicker.C:
				// 1. Check size cap watchdog
				if logsMaxSize, err := config.ParseBytes(m.cfg.Storage.LogsMaxSize); err == nil && logsMaxSize > 0 {
					logsDir := filepath.Join(m.cfg.Storage.Path, "logs")
					var totalBytes int64
					_ = filepath.Walk(logsDir, func(_ string, info os.FileInfo, _ error) error {
						if info != nil && !info.IsDir() {
							totalBytes += info.Size()
						}
						return nil
					})
					if totalBytes > logsMaxSize {
						log.Printf("[cleanup] watchdog: storage size (%s) exceeded logs_max_size (%s); triggering emergency cleanup",
							FormatBytes(totalBytes), FormatBytes(logsMaxSize))
						_, _ = m.Run(ctx, "emergency", false)
					}
				}

				// 2. Check daily schedule (e.g. 03:00 UTC)
				if now.UTC().Hour() == 3 && now.UTC().Minute() == 0 {
					log.Printf("[cleanup] scheduler: daily retention cleanup triggered (03:00 UTC)")
					_, _ = m.Run(ctx, "scheduled", false)
				}

				// 3. Hourly exited container prune (at minute 0 of each hour)
				if now.UTC().Minute() == 0 && m.executor != nil {
					cutoffTS := m.containersCutoffTS(now.UTC())
					if pruned, err := m.executor.PruneExitedContainers(ctx, cutoffTS); err != nil {
						log.Printf("[cleanup] hourly exited container prune failed: %v", err)
					} else if pruned > 0 {
						log.Printf("[cleanup] hourly prune: removed %d expired exited containers", pruned)
					}
				}
			}
		}
	}()
}

// Stop cleanly terminates background tickers.
func (m *CleanupManager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.wg.Wait()
}
