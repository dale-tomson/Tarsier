package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

// RetentionPlan describes the parameters and targets for a pre-cleanup or cleanup run.
type RetentionPlan struct {
	GeneratedAt    time.Time
	DryRun         bool
	LogsCutoffTS   int64 // UTC nanoseconds; chunks strictly older than this date/cutoff are slated for purge
	RawCutoffTS    int64 // Unix seconds for metrics.db sample_raw
	Rollup1mCutoff int64 // Unix seconds for metrics.db sample_1m
	Rollup5mCutoff int64 // Unix seconds for metrics.db sample_5m
	EventsCutoffTS int64 // Unix seconds for config.db container_event / alert_event
	ExpiredChunks  []ExpiredChunkFile
}

// ExpiredChunkFile references a single candidate .chunk and companion .idx on disk.
type ExpiredChunkFile struct {
	SourceID   int64
	SourceName string
	Date       string // "YYYY-MM-DD"
	ChunkPath  string
	IdxPath    string
	ChunkBytes int64
	IdxBytes   int64
}

// SourceLogDigest summarizes purged log volumes for one source.
type SourceLogDigest struct {
	SourceID     int64  `json:"source_id"`
	SourceName   string `json:"source_name"`
	FilesCount   int    `json:"files_count"`
	TotalLines   int64  `json:"total_lines"`
	ErrorLines   int64  `json:"error_lines"`
	WarnLines    int64  `json:"warn_lines"`
	BytesReclaim int64  `json:"bytes_reclaim"`
}

// MetricDigest summarizes expiring metric values for an entity/metric pair.
type MetricDigest struct {
	Entity string  `json:"entity"`
	Metric string  `json:"metric"`
	Min    float64 `json:"min"`
	Avg    float64 `json:"avg"`
	Max    float64 `json:"max"`
	Count  int64   `json:"count"`
}

// ContainerLifecycleDigest summarizes expiring container events.
type ContainerLifecycleDigest struct {
	ContainerName string `json:"container_name"`
	Restarts      int64  `json:"restarts"`
	OOMKills      int64  `json:"oom_kills"`
	Dies          int64  `json:"dies"`
}

// PreCleanupReport compiles all statistics and digests slated for pruning.
type PreCleanupReport struct {
	GeneratedAt       time.Time                  `json:"generated_at"`
	DryRun            bool                       `json:"dry_run"`
	WindowStart       time.Time                  `json:"window_start"`
	WindowEnd         time.Time                  `json:"window_end"`
	TotalLogsPurged   int64                      `json:"total_logs_purged"`
	TotalErrors       int64                      `json:"total_errors"`
	TotalWarnings     int64                      `json:"total_warnings"`
	TotalBytesReclaim int64                      `json:"total_bytes_reclaim"`
	Sources           []SourceLogDigest          `json:"sources"`
	TopErrorPatterns  []PatternSummary           `json:"top_error_patterns"`
	ContainerActivity []ContainerLifecycleDigest `json:"container_activity"`
	MetricSummaries   []MetricDigest             `json:"metric_summaries"`
	AlertsFired       int64                      `json:"alerts_fired"`
	Truncated         bool                       `json:"truncated"`
	TruncationNote    string                     `json:"truncation_note"`
}

// ReportBuilderConfig provides dependencies and tunables for ReportBuilder.
type ReportBuilderConfig struct {
	DataDir         string
	MetricsRO       *sql.DB
	ConfigRO        *sql.DB
	TopPatterns     int  // default 10
	IncludeWarnings bool // default false
}

// ReportBuilder inspects expired log chunks and SQLite database windows to compile PreCleanupReport.
type ReportBuilder struct {
	dataDir         string
	metricsRO       *sql.DB
	configRO        *sql.DB
	topPatterns     int
	includeWarnings bool
	dec             *zstd.Decoder
	rawPool         *sync.Pool
}

// NewReportBuilder creates an initialized ReportBuilder.
func NewReportBuilder(cfg ReportBuilderConfig) (*ReportBuilder, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}
	if cfg.TopPatterns <= 0 {
		cfg.TopPatterns = 10
	}

	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}

	return &ReportBuilder{
		dataDir:         cfg.DataDir,
		metricsRO:       cfg.MetricsRO,
		configRO:        cfg.ConfigRO,
		topPatterns:     cfg.TopPatterns,
		includeWarnings: cfg.IncludeWarnings,
		dec:             dec,
		rawPool: &sync.Pool{
			New: func() any {
				b := make([]byte, 68*1024)
				return &b
			},
		},
	}, nil
}

// DiscoverExpiredChunks scans <dataDir>/logs/<source_id>/ for chunk/idx pairs strictly older than cutoffTS.
func DiscoverExpiredChunks(logsDir string, cutoffTS int64, sources map[int64]string) ([]ExpiredChunkFile, error) {
	var result []ExpiredChunkFile

	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		sourceID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}

		sourceName := ""
		if sources != nil {
			sourceName = sources[sourceID]
		}
		if sourceName == "" {
			sourceName = fmt.Sprintf("source-%d", sourceID)
		}

		sourceDir := filepath.Join(logsDir, entry.Name())
		files, err := os.ReadDir(sourceDir)
		if err != nil {
			continue
		}

		// Group by date
		dateFiles := make(map[string]bool)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if filepath.Ext(name) == ".chunk" {
				date := name[:len(name)-6]
				dateFiles[date] = true
			}
		}

		for date := range dateFiles {
			// Calculate day end in UTC: YYYY-MM-DD 23:59:59.999999999 UTC
			t, err := time.Parse("2006-01-02", date)
			if err != nil {
				continue
			}
			dayEndNano := t.Add(24 * time.Hour).UnixNano()

			if dayEndNano < cutoffTS {
				chunkPath := filepath.Join(sourceDir, date+".chunk")
				idxPath := filepath.Join(sourceDir, date+".idx")

				var chunkBytes int64
				if cInfo, err := os.Stat(chunkPath); err == nil {
					chunkBytes = cInfo.Size()
				}
				var idxBytes int64
				if iInfo, err := os.Stat(idxPath); err == nil {
					idxBytes = iInfo.Size()
				}

				result = append(result, ExpiredChunkFile{
					SourceID:   sourceID,
					SourceName: sourceName,
					Date:       date,
					ChunkPath:  chunkPath,
					IdxPath:    idxPath,
					ChunkBytes: chunkBytes,
					IdxBytes:   idxBytes,
				})
			}
		}
	}

	// Sort chronologically by Date, then SourceID
	sort.Slice(result, func(i, j int) bool {
		if result[i].Date != result[j].Date {
			return result[i].Date < result[j].Date
		}
		return result[i].SourceID < result[j].SourceID
	})

	return result, nil
}

// Build compiles the PreCleanupReport for plan without deleting any files or rows.
func (b *ReportBuilder) Build(ctx context.Context, plan RetentionPlan) (PreCleanupReport, error) {
	report := PreCleanupReport{
		GeneratedAt: plan.GeneratedAt,
		DryRun:      plan.DryRun,
	}
	if report.GeneratedAt.IsZero() {
		report.GeneratedAt = time.Now().UTC()
	}

	// Determine time window
	var windowStart time.Time
	var windowEnd time.Time
	if plan.LogsCutoffTS > 0 {
		windowEnd = time.Unix(0, plan.LogsCutoffTS).UTC()
	} else {
		windowEnd = report.GeneratedAt
	}

	// 1. Process expired log chunks
	patternCounter := NewPatternCounter(4096)
	sourceMap := make(map[int64]*SourceLogDigest)

	rawPtr := b.rawPool.Get().(*[]byte)
	rawBuf := *rawPtr
	defer func() {
		*rawPtr = rawBuf
		b.rawPool.Put(rawPtr)
	}()

	var frameBuf []byte

	for _, chunk := range plan.ExpiredChunks {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}

		digest, exists := sourceMap[chunk.SourceID]
		if !exists {
			digest = &SourceLogDigest{
				SourceID:   chunk.SourceID,
				SourceName: chunk.SourceName,
			}
			sourceMap[chunk.SourceID] = digest
		}
		digest.FilesCount++
		digest.BytesReclaim += (chunk.ChunkBytes + chunk.IdxBytes)

		b.processExpiredChunk(chunk, digest, &report, patternCounter, &windowStart, &rawBuf, &frameBuf)
	}

	// Compile sources summary
	report.Sources = make([]SourceLogDigest, 0, len(sourceMap))
	for _, d := range sourceMap {
		report.Sources = append(report.Sources, *d)
		report.TotalLogsPurged += d.TotalLines
		report.TotalBytesReclaim += d.BytesReclaim
	}
	sort.Slice(report.Sources, func(i, j int) bool {
		return report.Sources[i].SourceID < report.Sources[j].SourceID
	})

	report.TopErrorPatterns = patternCounter.TopN(b.topPatterns)

	// 2. Query Metrics DB window summaries
	if b.metricsRO != nil {
		metricSummaries, err := b.queryMetricSummaries(ctx, plan)
		if err == nil {
			report.MetricSummaries = metricSummaries
		}
	}

	// 3. Query Config DB window summaries (container events, alert events)
	if b.configRO != nil {
		containerEvents, err := b.queryContainerEvents(ctx, plan.EventsCutoffTS)
		if err == nil {
			report.ContainerActivity = containerEvents
		}

		alertsFired, err := b.queryAlertsFired(ctx, plan.EventsCutoffTS)
		if err == nil {
			report.AlertsFired = alertsFired
		}
	}

	if windowStart.IsZero() {
		windowStart = windowEnd
	}
	report.WindowStart = windowStart
	report.WindowEnd = windowEnd

	return report, nil
}

func (b *ReportBuilder) processExpiredChunk(
	chunk ExpiredChunkFile,
	digest *SourceLogDigest,
	report *PreCleanupReport,
	patternCounter *PatternCounter,
	windowStart *time.Time,
	rawBuf *[]byte,
	frameBuf *[]byte,
) {
	// Read and parse .idx file
	idxBytes, err := os.ReadFile(chunk.IdxPath)
	if err != nil {
		return
	}

	numBlocks := len(idxBytes) / logs.IndexRecordSize
	if numBlocks == 0 {
		return
	}

	var fChunk *os.File
	defer func() {
		if fChunk != nil {
			_ = fChunk.Close()
		}
	}()

	sourceIDStr := strconv.FormatInt(chunk.SourceID, 10)

	for i := 0; i < numBlocks; i++ {
		recBytes := idxBytes[i*logs.IndexRecordSize : (i+1)*logs.IndexRecordSize]
		meta, err := logs.UnmarshalIndexRecord(recBytes)
		if err != nil {
			continue
		}

		digest.TotalLines += int64(meta.RecordCount)

		// Update window start with earliest encountered timestamp
		blockStart := time.Unix(0, meta.MinTS).UTC()
		if windowStart.IsZero() || blockStart.Before(*windowStart) {
			*windowStart = blockStart
		}

		// Zero-Decompression Skip: skip decompressing if block contains only levels < LevelWarn
		if meta.MaxLevel < model.LevelWarn {
			continue
		}

		// Open chunk file lazily when an error/warning block is found
		if fChunk == nil {
			var oErr error
			fChunk, oErr = os.Open(chunk.ChunkPath)
			if oErr != nil {
				return
			}
		}

		// Read compressed frame using reusable buffer
		compLen := int(meta.CompressedLen)
		if cap(*frameBuf) < compLen {
			*frameBuf = make([]byte, compLen)
		}
		frameBytes := (*frameBuf)[:compLen]
		if _, rErr := fChunk.ReadAt(frameBytes, meta.FileOffset); rErr != nil {
			continue
		}

		// Decompress block using pooled raw buffer
		rawBytes, dErr := b.dec.DecodeAll(frameBytes, (*rawBuf)[:0])
		if dErr != nil {
			continue
		}
		if cap(rawBytes) > cap(*rawBuf) && cap(rawBytes) <= 256*1024 {
			*rawBuf = rawBytes
		}

		entries, decErr := logs.DecodeBlock(rawBytes, sourceIDStr)
		if decErr != nil {
			continue
		}

		for _, ent := range entries {
			if ent.Level >= model.LevelError {
				digest.ErrorLines++
				report.TotalErrors++
				patternCounter.Add(chunk.SourceName, ent.Timestamp.UnixNano(), string(ent.Message))
			} else if ent.Level == model.LevelWarn {
				digest.WarnLines++
				report.TotalWarnings++
				if b.includeWarnings {
					patternCounter.Add(chunk.SourceName, ent.Timestamp.UnixNano(), string(ent.Message))
				}
			}
		}
	}
}

func (b *ReportBuilder) queryMetricSummaries(ctx context.Context, plan RetentionPlan) ([]MetricDigest, error) {
	var results []MetricDigest

	// Query sample_1m
	if plan.Rollup1mCutoff > 0 {
		query := `
			SELECT s.entity, s.metric, MIN(m.min), AVG(m.avg), MAX(m.max), COUNT(*)
			FROM sample_1m m
			JOIN series s ON s.id = m.series_id
			WHERE m.ts < ?
			GROUP BY s.entity, s.metric
			ORDER BY s.entity, s.metric;
		`
		rows, err := b.metricsRO.QueryContext(ctx, query, plan.Rollup1mCutoff)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d MetricDigest
				if err := rows.Scan(&d.Entity, &d.Metric, &d.Min, &d.Avg, &d.Max, &d.Count); err == nil {
					results = append(results, d)
				}
			}
		}
	}

	// If sample_1m produced nothing, query sample_raw
	if len(results) == 0 && plan.RawCutoffTS > 0 {
		query := `
			SELECT s.entity, s.metric, MIN(r.v), AVG(r.v), MAX(r.v), COUNT(*)
			FROM sample_raw r
			JOIN series s ON s.id = r.series_id
			WHERE r.ts < ?
			GROUP BY s.entity, s.metric
			ORDER BY s.entity, s.metric;
		`
		rows, err := b.metricsRO.QueryContext(ctx, query, plan.RawCutoffTS)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d MetricDigest
				if err := rows.Scan(&d.Entity, &d.Metric, &d.Min, &d.Avg, &d.Max, &d.Count); err == nil {
					results = append(results, d)
				}
			}
		}
	}

	return results, nil
}

func (b *ReportBuilder) queryContainerEvents(ctx context.Context, cutoffTS int64) ([]ContainerLifecycleDigest, error) {
	if cutoffTS <= 0 {
		return nil, nil
	}

	query := `
		SELECT COALESCE(c.name, e.container_id) as container_name,
		       SUM(CASE WHEN e.type = 'restart' THEN 1 ELSE 0 END) as restarts,
		       SUM(CASE WHEN e.type = 'oom' THEN 1 ELSE 0 END) as oom_kills,
		       SUM(CASE WHEN e.type = 'die' THEN 1 ELSE 0 END) as dies
		FROM container_event e
		LEFT JOIN container c ON c.id = e.container_id
		WHERE e.ts < ?
		GROUP BY container_name
		ORDER BY container_name;
	`
	rows, err := b.configRO.QueryContext(ctx, query, cutoffTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ContainerLifecycleDigest
	for rows.Next() {
		var d ContainerLifecycleDigest
		if err := rows.Scan(&d.ContainerName, &d.Restarts, &d.OOMKills, &d.Dies); err == nil {
			results = append(results, d)
		}
	}
	return results, nil
}

func (b *ReportBuilder) queryAlertsFired(ctx context.Context, cutoffTS int64) (int64, error) {
	if cutoffTS <= 0 {
		return 0, nil
	}

	query := `SELECT COUNT(*) FROM alert_event WHERE state = 'firing' AND created_at < ?;`
	var count int64
	err := b.configRO.QueryRowContext(ctx, query, cutoffTS).Scan(&count)
	return count, err
}
