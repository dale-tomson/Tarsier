package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

func TestReportBuilder_EndToEndAggregation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	logsDir := filepath.Join(dataDir, "logs")

	// 1. Initialize temporary SQLite stores for metrics and config
	store, err := db.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Seed config.db source table
	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO source (id, kind, name, first_seen, last_seen) VALUES
			(1, 'docker', 'web-api', 1700000000, 1700001000),
			(2, 'docker', 'worker', 1700000000, 1700001000);
		`)
		return err
	})

	// Seed config.db container and container_event
	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status) VALUES
			('c1', 'api-container', 'img:v1', 1700000000, 1700001000, 'running');
			INSERT INTO container_event (id, container_id, ts, type, detail) VALUES
			(1, 'c1', 1000, 'restart', 'signal 9'),
			(2, 'c1', 1001, 'oom', 'cgroup kill'),
			(3, 'c1', 1002, 'die', 'exit 137');
			INSERT INTO alert_event (id, rule_id, rule_name, target, severity, state, started, resolved, summary, details_json, created_at) VALUES
			(1, 10, 'HighCPU', 'host', 'critical', 'firing', 1000, 0, 'CPU > 90%', '{}', 1005);
		`)
		return err
	})

	// Seed metrics.db series and sample_1m
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO series (id, entity, metric) VALUES
			(1, 'host', 'cpu_pct'),
			(2, 'host', 'mem_used_pct');
			INSERT INTO sample_1m (series_id, ts, avg, min, max) VALUES
			(1, 1000, 75.5, 50.0, 95.0),
			(1, 1060, 80.0, 60.0, 98.0),
			(2, 1000, 65.0, 60.0, 70.0);
		`)
		return err
	})

	// 2. Seed .chunk and .idx files
	idxMgr := logs.NewIndexManager(logsDir)
	defer idxMgr.Close()

	chunkCfg := logs.ChunkWriterConfig{
		BaseDir:          logsDir,
		BlockTargetBytes: 1024, // Small blocks to force flushes
		FlushInterval:    100 * time.Millisecond,
		ZstdLevel:        zstd.SpeedFastest,
		OnBlockFlushed: func(meta logs.BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}

	chunkMgr, err := logs.NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	// Day 1: Expired day (2026-11-10) for Source 1 (includes an INFO block and an ERROR block with UUIDs)
	expiredDay := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	// Pure INFO lines
	for i := 0; i < 50; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: expiredDay.Add(time.Duration(i) * time.Second),
			SourceID:  "1",
			Level:     model.LevelInfo,
			Stream:    model.StreamStdout,
			Message:   []byte(fmt.Sprintf("standard operational info line %d", i)),
		})
	}
	// Force block boundary
	_ = chunkMgr.Flush()

	// Repeated ERROR lines with UUIDs
	for i := 0; i < 30; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: expiredDay.Add(time.Duration(100+i) * time.Second),
			SourceID:  "1",
			Level:     model.LevelError,
			Stream:    model.StreamStderr,
			Message:   []byte(fmt.Sprintf("failed to load resource 550e8400-e29b-41d4-a716-%012d: timeout", i)),
		})
	}

	// Day 2: Active day (2026-11-15) for Source 1 (should be excluded)
	activeDay := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: activeDay.Add(time.Duration(i) * time.Second),
			SourceID:  "1",
			Level:     model.LevelError,
			Stream:    model.StreamStderr,
			Message:   []byte("active today error line"),
		})
	}

	if err := chunkMgr.Close(ctx); err != nil {
		t.Fatalf("chunkMgr.Close failed: %v", err)
	}
	if err := idxMgr.Sync(); err != nil {
		t.Fatalf("idxMgr.Sync failed: %v", err)
	}

	// 3. Discover Expired Chunks with cutoff = 2026-11-12 UTC
	cutoffTS := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).UnixNano()
	sources := map[int64]string{1: "web-api", 2: "worker"}

	expiredChunks, err := DiscoverExpiredChunks(logsDir, cutoffTS, sources)
	if err != nil {
		t.Fatalf("DiscoverExpiredChunks failed: %v", err)
	}

	if len(expiredChunks) != 1 {
		t.Fatalf("expected 1 expired chunk file (2026-11-10), got %d: %+v", len(expiredChunks), expiredChunks)
	}
	if expiredChunks[0].Date != "2026-11-10" {
		t.Errorf("expected date 2026-11-10, got %s", expiredChunks[0].Date)
	}

	// 4. Run ReportBuilder.Build
	builder, err := NewReportBuilder(ReportBuilderConfig{
		DataDir:   dataDir,
		MetricsRO: store.Metrics.RO,
		ConfigRO:  store.Config.RO,
	})
	if err != nil {
		t.Fatalf("NewReportBuilder failed: %v", err)
	}

	plan := RetentionPlan{
		GeneratedAt:    time.Now().UTC(),
		DryRun:         true,
		LogsCutoffTS:   cutoffTS,
		RawCutoffTS:    2000,
		Rollup1mCutoff: 2000,
		EventsCutoffTS: 2000,
		ExpiredChunks:  expiredChunks,
	}

	report, err := builder.Build(ctx, plan)
	if err != nil {
		t.Fatalf("ReportBuilder.Build failed: %v", err)
	}

	// Verify report totals
	if report.TotalLogsPurged != 80 {
		t.Errorf("expected 80 logs purged (50 info + 30 error), got %d", report.TotalLogsPurged)
	}
	if report.TotalErrors != 30 {
		t.Errorf("expected 30 total errors, got %d", report.TotalErrors)
	}
	if report.TotalBytesReclaim <= 0 {
		t.Errorf("expected positive reclaimed bytes, got %d", report.TotalBytesReclaim)
	}

	// Verify top error pattern
	if len(report.TopErrorPatterns) == 0 {
		t.Fatalf("expected top error patterns, got none")
	}
	if report.TopErrorPatterns[0].Pattern != "failed to load resource {uuid}: timeout" {
		t.Errorf("unexpected top pattern: %s", report.TopErrorPatterns[0].Pattern)
	}
	if report.TopErrorPatterns[0].Count != 30 {
		t.Errorf("expected count 30 for top pattern, got %d", report.TopErrorPatterns[0].Count)
	}

	// Verify container activity
	if len(report.ContainerActivity) != 1 {
		t.Fatalf("expected 1 container activity record, got %d", len(report.ContainerActivity))
	}
	ca := report.ContainerActivity[0]
	if ca.ContainerName != "api-container" || ca.Restarts != 1 || ca.OOMKills != 1 || ca.Dies != 1 {
		t.Errorf("unexpected container activity: %+v", ca)
	}

	// Verify alerts fired
	if report.AlertsFired != 1 {
		t.Errorf("expected 1 alert fired, got %d", report.AlertsFired)
	}

	// Verify metrics summary
	if len(report.MetricSummaries) != 2 {
		t.Errorf("expected 2 metric summaries, got %d", len(report.MetricSummaries))
	}

	// Verify zero files or rows were deleted
	if _, err := os.Stat(expiredChunks[0].ChunkPath); os.IsNotExist(err) {
		t.Errorf("expired chunk file was unexpectedly deleted during report build!")
	}
	var rowCount int
	_ = store.Metrics.RO.QueryRow("SELECT COUNT(*) FROM sample_1m;").Scan(&rowCount)
	if rowCount != 3 {
		t.Errorf("metrics rows were modified during report build! expected 3, got %d", rowCount)
	}
}

func TestRenderTemplates_GoldenAndXSS(t *testing.T) {
	report := PreCleanupReport{
		GeneratedAt:       time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC),
		DryRun:            true,
		WindowStart:       time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		WindowEnd:         time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
		TotalLogsPurged:   15000,
		TotalErrors:       42,
		TotalWarnings:     15,
		TotalBytesReclaim: 10485760, // 10 MiB
		AlertsFired:       2,
		Sources: []SourceLogDigest{
			{
				SourceID:     1,
				SourceName:   "web-api",
				FilesCount:   7,
				TotalLines:   10000,
				ErrorLines:   40,
				WarnLines:    10,
				BytesReclaim: 8000000,
			},
		},
		TopErrorPatterns: []PatternSummary{
			{
				Pattern:    "database connection refused at {ip}:{num}",
				Count:      40,
				SampleLine: `<script>alert("XSS")</script> failed connecting to 10.0.1.5:5432`,
				SourceName: "web-api",
				LastSeenTS: 1700000000,
			},
		},
		ContainerActivity: []ContainerLifecycleDigest{
			{
				ContainerName: "web-api",
				Restarts:      3,
				OOMKills:      1,
				Dies:          3,
			},
		},
		MetricSummaries: []MetricDigest{
			{
				Entity: "host",
				Metric: "cpu_pct",
				Min:    12.5,
				Avg:    45.8,
				Max:    99.2,
				Count:  1000,
			},
		},
	}

	engine, err := NewTemplateEngine("", "")
	if err != nil {
		t.Fatalf("NewTemplateEngine failed: %v", err)
	}

	// 1. Render Plain Text
	textBytes, truncated, err := engine.RenderText(report, 0)
	if err != nil {
		t.Fatalf("RenderText failed: %v", err)
	}
	if truncated {
		t.Errorf("unexpected truncation on maxBytes=0")
	}
	textStr := string(textBytes)

	if !strings.Contains(textStr, "Tarsier Pre-Cleanup Report") {
		t.Errorf("text report missing title: %s", textStr)
	}
	if !strings.Contains(textStr, "DRY RUN (Preview Only)") {
		t.Errorf("text report missing dry-run indicator")
	}
	if !strings.Contains(textStr, "database connection refused at {ip}:{num}") {
		t.Errorf("text report missing pattern")
	}
	if !strings.Contains(textStr, "SOURCE BREAKDOWN") || !strings.Contains(textStr, "CONTAINER ACTIVITY") || !strings.Contains(textStr, "METRIC SUMMARIES") {
		t.Errorf("text report missing expected section headers: %s", textStr)
	}
	if strings.Contains(textStr, "# ") || strings.Contains(textStr, "**") || strings.Contains(textStr, "|---") {
		t.Errorf("text report unexpectedly contains markdown syntax: %s", textStr)
	}

	// 2. Render HTML
	htmlBytes, truncated, err := engine.RenderHTML(report, 0)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}
	if truncated {
		t.Errorf("unexpected truncation on maxBytes=0")
	}
	htmlStr := string(htmlBytes)

	// Verify XSS protection: <script> must be escaped to &lt;script&gt;
	if strings.Contains(htmlStr, `<script>alert("XSS")</script>`) {
		t.Fatalf("CRITICAL: Raw XSS payload was not escaped in HTML template!")
	}
	if !strings.Contains(htmlStr, `&lt;script&gt;alert(&#34;XSS&#34;)&lt;/script&gt;`) {
		t.Errorf("expected HTML-escaped script tag, got: %s", htmlStr)
	}
}

func TestRenderTemplates_MaxBytesTruncation(t *testing.T) {
	report := PreCleanupReport{
		GeneratedAt:       time.Now().UTC(),
		TotalLogsPurged:   1000000,
		TotalBytesReclaim: 50000000,
		TopErrorPatterns: []PatternSummary{
			{
				Pattern:    "pattern " + strings.Repeat("A", 200),
				Count:      500,
				SampleLine: "sample line " + strings.Repeat("B", 300),
				SourceName: "source-1",
			},
		},
	}

	engine, err := NewTemplateEngine("", "")
	if err != nil {
		t.Fatalf("NewTemplateEngine failed: %v", err)
	}

	// Max 500 bytes limit
	maxBytes := 500
	textBytes, truncated, err := engine.RenderText(report, maxBytes)
	if err != nil {
		t.Fatalf("RenderText failed: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if len(textBytes) > maxBytes {
		t.Fatalf("rendered text length %d exceeds maxBytes %d", len(textBytes), maxBytes)
	}
	if !strings.Contains(string(textBytes), "[Report truncated at 500 bytes") {
		t.Errorf("missing truncation notice in text report: %s", string(textBytes))
	}

	// HTML truncation
	htmlBytes, truncated, err := engine.RenderHTML(report, maxBytes)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true for HTML")
	}
	if len(htmlBytes) > maxBytes {
		t.Fatalf("rendered HTML length %d exceeds maxBytes %d", len(htmlBytes), maxBytes)
	}
}

func TestFormatSummaryBody(t *testing.T) {
	report := PreCleanupReport{
		GeneratedAt:       time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC),
		WindowStart:       time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		WindowEnd:         time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
		TotalLogsPurged:   15000,
		TotalErrors:       42,
		TotalWarnings:     15,
		TotalBytesReclaim: 524288, // 512 KiB
		AlertsFired:       2,
	}

	manualBody := FormatSummaryBody(report, "manual")
	expectedManual := "Generated : 2026-11-15 14:00:00 UTC\n" +
		"Mode : Manual\n" +
		"Cleanup Window: From 2026-11-08 00:00:00 To 2026-11-15 00:00:00 UTC\n\n" +
		"Summary:\n" +
		"Logs Lines Purged: 15000\n" +
		"    Errors: 42\n" +
		"    Warnings: 15\n\n" +
		"Disk Space Freed: 512.0 KiB\n" +
		"Alerts Fired: 2"
	if manualBody != expectedManual {
		t.Errorf("FormatSummaryBody(manual) mismatch:\ngot:\n%s\nwant:\n%s", manualBody, expectedManual)
	}

	autoBody := FormatSummaryBody(report, "scheduled")
	if !strings.Contains(autoBody, "Mode : Auto\n") {
		t.Errorf("FormatSummaryBody(scheduled) expected Mode : Auto, got:\n%s", autoBody)
	}
}
