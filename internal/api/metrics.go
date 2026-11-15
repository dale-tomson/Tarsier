package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/auth"
)

// TimeSeriesPoint represents a single data point in the host metrics response.
type TimeSeriesPoint struct {
	TS  int64    `json:"ts"`
	V   float64  `json:"v"`
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// HostMetricsResponse encapsulates queried time series.
type HostMetricsResponse struct {
	Entity string            `json:"entity"`
	Metric string            `json:"metric"`
	Step   string            `json:"step"`
	From   string            `json:"from"`
	To     string            `json:"to"`
	Points []TimeSeriesPoint `json:"points"`
}

// DailyMetricStat holds average and peak numbers for a single metric.
type DailyMetricStat struct {
	AvgPct    *float64 `json:"avg_pct,omitempty"`
	PeakPct   *float64 `json:"peak_pct,omitempty"`
	AvgBytes  *int64   `json:"avg_bytes,omitempty"`
	PeakBytes *int64   `json:"peak_bytes,omitempty"`
}

// DailyHostSummary encapsulates today's live host metrics.
type DailyHostSummary struct {
	CPU struct {
		AvgPct  float64 `json:"avg_pct"`
		PeakPct float64 `json:"peak_pct"`
	} `json:"cpu"`
	RAM struct {
		AvgBytes   int64 `json:"avg_bytes"`
		PeakBytes  int64 `json:"peak_bytes"`
		TotalBytes int64 `json:"total_bytes"`
	} `json:"ram"`
}

// DailyContainerSummary encapsulates today's live container metrics.
type DailyContainerSummary struct {
	Name string `json:"name"`
	CPU  struct {
		AvgPct  float64 `json:"avg_pct"`
		PeakPct float64 `json:"peak_pct"`
	} `json:"cpu"`
	RAM struct {
		AvgBytes   int64 `json:"avg_bytes"`
		PeakBytes  int64 `json:"peak_bytes"`
		LimitBytes int64 `json:"limit_bytes"`
	} `json:"ram"`
}

// TodaySummaryResponse is the top-level payload for /api/v1/metrics/daily/summary.
type TodaySummaryResponse struct {
	Date       string                  `json:"date"`
	Host       DailyHostSummary        `json:"host"`
	Containers []DailyContainerSummary `json:"containers"`
}

// DailyHistoryEntry represents a single completed day's CPU and RAM stats.
type DailyHistoryEntry struct {
	Date string `json:"date"`
	CPU  struct {
		AvgPct  float64 `json:"avg_pct"`
		PeakPct float64 `json:"peak_pct"`
	} `json:"cpu"`
	RAM struct {
		AvgBytes  int64 `json:"avg_bytes"`
		PeakBytes int64 `json:"peak_bytes"`
	} `json:"ram"`
}

// DailyHistoryResponse is the top-level payload for /api/v1/metrics/daily.
type DailyHistoryResponse struct {
	Entity  string              `json:"entity"`
	Days    int                 `json:"days"`
	History []DailyHistoryEntry `json:"history"`
}

func parseTimeParam(val string, defaultVal int64) (int64, error) {
	if val == "" {
		return defaultVal, nil
	}
	// Try parsing unix seconds integer
	if sec, err := strconv.ParseInt(val, 10, 64); err == nil {
		return sec, nil
	}
	// Try parsing RFC3339 timestamp
	if t, err := time.Parse(time.RFC3339, val); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid timestamp %q: expected epoch seconds or RFC3339", val)
}

// handleHostMetrics serves GET /api/v1/host/metrics.
func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := strings.TrimSpace(q.Get("metric"))
	if metric == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", "metric parameter is required")
		return
	}

	entity := strings.TrimSpace(q.Get("entity"))
	if entity == "" {
		entity = "host"
	}

	nowUnix := s.clock.Now().Unix()
	to, err := parseTimeParam(q.Get("to"), nowUnix)
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", err.Error())
		return
	}

	from, err := parseTimeParam(q.Get("from"), to-3600)
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", err.Error())
		return
	}

	// Clamp 'to' to now + 60s
	if to > nowUnix+60 {
		to = nowUnix + 60
	}

	if from >= to {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", "from timestamp must be strictly less than to timestamp")
		return
	}

	stepParam := strings.ToLower(strings.TrimSpace(q.Get("step")))
	if stepParam == "" {
		stepParam = "auto"
	}

	cacheKey := r.Method + "?" + r.URL.RawQuery
	data, err := s.queryCache.GetOrCompute(cacheKey, func() ([]byte, error) {
		resp, queryErr := s.queryHostMetrics(r.Context(), entity, metric, from, to, stepParam)
		if queryErr != nil {
			return nil, queryErr
		}
		return json.Marshal(resp)
	})

	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) queryHostMetrics(ctx context.Context, entity, metric string, from, to int64, stepParam string) (*HostMetricsResponse, error) {
	span := to - from
	selectedStep := stepParam

	// Auto tier selection
	if stepParam == "auto" {
		if span <= 30*60 && s.hotCache != nil && s.hotCache.OldestTS(entity, metric) > 0 && from >= s.hotCache.OldestTS(entity, metric) {
			selectedStep = "cache"
		} else if span <= 6*3600 {
			selectedStep = "raw"
		} else if span <= 7*86400 {
			selectedStep = "1m"
		} else {
			selectedStep = "5m"
		}
	}

	resp := &HostMetricsResponse{
		Entity: entity,
		Metric: metric,
		From:   time.Unix(from, 0).UTC().Format(time.RFC3339),
		To:     time.Unix(to, 0).UTC().Format(time.RFC3339),
		Points: make([]TimeSeriesPoint, 0),
	}

	if selectedStep == "cache" && s.hotCache != nil {
		resp.Step = "10s"
		pts := s.hotCache.QueryRange(entity, metric, from, to, nil)
		for _, p := range pts {
			resp.Points = append(resp.Points, TimeSeriesPoint{
				TS: p.TS,
				V:  p.Value,
			})
		}
		return resp, nil
	}

	if s.metricsDB == nil {
		resp.Step = "10s"
		return resp, nil
	}

	// Lookup series ID
	var seriesID int64
	err := s.metricsDB.RO.QueryRowContext(ctx, "SELECT id FROM series WHERE entity = ? AND metric = ?;", entity, metric).Scan(&seriesID)
	if err == sql.ErrNoRows {
		// No data collected yet
		resp.Step = selectedStep
		if resp.Step == "raw" {
			resp.Step = "10s"
		}
		return resp, nil
	} else if err != nil {
		return nil, fmt.Errorf("lookup series: %w", err)
	}

	switch selectedStep {
	case "raw", "10s":
		resp.Step = "10s"
		rows, err := s.metricsDB.RO.QueryContext(ctx,
			"SELECT ts, v FROM sample_raw WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC;",
			seriesID, from, to)
		if err != nil {
			return nil, fmt.Errorf("query sample_raw: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var pt TimeSeriesPoint
			if err := rows.Scan(&pt.TS, &pt.V); err != nil {
				return nil, err
			}
			resp.Points = append(resp.Points, pt)
		}
		return resp, rows.Err()

	case "1m":
		resp.Step = "1m"
		rows, err := s.metricsDB.RO.QueryContext(ctx,
			"SELECT ts, avg, min, max FROM sample_1m WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC;",
			seriesID, from, to)
		if err != nil {
			return nil, fmt.Errorf("query sample_1m: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var pt TimeSeriesPoint
			var minVal, maxVal float64
			if err := rows.Scan(&pt.TS, &pt.V, &minVal, &maxVal); err != nil {
				return nil, err
			}
			pt.Min = &minVal
			pt.Max = &maxVal
			resp.Points = append(resp.Points, pt)
		}
		return resp, rows.Err()

	case "5m":
		resp.Step = "5m"
		rows, err := s.metricsDB.RO.QueryContext(ctx,
			"SELECT ts, avg, min, max FROM sample_5m WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC;",
			seriesID, from, to)
		if err != nil {
			return nil, fmt.Errorf("query sample_5m: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var pt TimeSeriesPoint
			var minVal, maxVal float64
			if err := rows.Scan(&pt.TS, &pt.V, &minVal, &maxVal); err != nil {
				return nil, err
			}
			pt.Min = &minVal
			pt.Max = &maxVal
			resp.Points = append(resp.Points, pt)
		}
		return resp, rows.Err()

	default:
		return nil, fmt.Errorf("unsupported step %q", selectedStep)
	}
}

// handleDailySummary serves GET /api/v1/metrics/daily/summary.
func (s *Server) handleDailySummary(w http.ResponseWriter, r *http.Request) {
	cacheKey := r.Method + "?" + r.URL.RawQuery
	data, err := s.queryCache.GetOrCompute(cacheKey, func() ([]byte, error) {
		resp, queryErr := s.computeDailySummary(r.Context())
		if queryErr != nil {
			return nil, queryErr
		}
		return json.Marshal(resp)
	})

	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "summary_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) computeDailySummary(ctx context.Context) (*TodaySummaryResponse, error) {
	now := s.clock.Now().UTC()
	todayStr := now.Format("2006-01-02")

	resp := &TodaySummaryResponse{
		Date:       todayStr,
		Containers: make([]DailyContainerSummary, 0),
	}

	if s.rollup == nil {
		return resp, nil
	}

	stats, err := s.rollup.QueryTodaySummary(ctx, "")
	if err != nil {
		return nil, err
	}

	// Latest total RAM for host
	var hostRAMTotal int64
	if s.hotCache != nil {
		if pt, ok := s.hotCache.GetLatest("host", "mem_total"); ok {
			hostRAMTotal = int64(pt.Value)
		}
	}
	if hostRAMTotal == 0 && s.metricsDB != nil {
		var ramFloat float64
		if err := s.metricsDB.RO.QueryRowContext(ctx,
			"SELECT r.v FROM sample_raw r JOIN series s ON r.series_id = s.id WHERE s.entity = 'host' AND s.metric = 'mem_total' ORDER BY r.ts DESC LIMIT 1;",
		).Scan(&ramFloat); err == nil && ramFloat > 0 {
			hostRAMTotal = int64(ramFloat)
		}
	}
	resp.Host.RAM.TotalBytes = hostRAMTotal

	containerMap := make(map[string]*DailyContainerSummary)

	for _, stat := range stats {
		if stat.Entity == "host" {
			switch stat.Metric {
			case "cpu_pct":
				resp.Host.CPU.AvgPct = stat.Avg
				resp.Host.CPU.PeakPct = stat.Max
			case "mem_used":
				resp.Host.RAM.AvgBytes = int64(stat.Avg)
				resp.Host.RAM.PeakBytes = int64(stat.Max)
			}
		} else if strings.HasPrefix(stat.Entity, "container:") {
			cName := strings.TrimPrefix(stat.Entity, "container:")
			cSummary, exists := containerMap[cName]
			if !exists {
				cSummary = &DailyContainerSummary{Name: cName}
				containerMap[cName] = cSummary
			}
			switch stat.Metric {
			case "cpu_pct":
				cSummary.CPU.AvgPct = stat.Avg
				cSummary.CPU.PeakPct = stat.Max
			case "mem_used":
				cSummary.RAM.AvgBytes = int64(stat.Avg)
				cSummary.RAM.PeakBytes = int64(stat.Max)
			}
		}
	}

	for _, cs := range containerMap {
		resp.Containers = append(resp.Containers, *cs)
	}

	return resp, nil
}

// handleDailyHistory serves GET /api/v1/metrics/daily.
func (s *Server) handleDailyHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("entity"))
	if entity == "" {
		entity = "host"
	}

	days := 14
	if daysParam := q.Get("days"); daysParam != "" {
		if d, err := strconv.Atoi(daysParam); err == nil && d > 0 {
			days = d
		}
	}
	if days > 90 {
		days = 90
	}

	includeToday := false
	if incParam := strings.ToLower(q.Get("include_today")); incParam == "true" || incParam == "1" {
		includeToday = true
	}

	cacheKey := r.Method + "?" + r.URL.RawQuery
	data, err := s.queryCache.GetOrCompute(cacheKey, func() ([]byte, error) {
		resp, queryErr := s.queryDailyHistory(r.Context(), entity, days, includeToday)
		if queryErr != nil {
			return nil, queryErr
		}
		return json.Marshal(resp)
	})

	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "history_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) queryDailyHistory(ctx context.Context, entity string, days int, includeToday bool) (*DailyHistoryResponse, error) {
	resp := &DailyHistoryResponse{
		Entity:  entity,
		Days:    days,
		History: make([]DailyHistoryEntry, 0),
	}

	now := s.clock.Now().UTC()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	sinceTS := todayMidnight - int64(days*86400)

	// Prepend today's live stat if requested
	if includeToday && s.rollup != nil {
		todayStats, err := s.rollup.QueryTodaySummary(ctx, entity)
		if err == nil && len(todayStats) > 0 {
			var todayEntry DailyHistoryEntry
			todayEntry.Date = now.Format("2006-01-02")
			for _, st := range todayStats {
				switch st.Metric {
				case "cpu_pct":
					todayEntry.CPU.AvgPct = st.Avg
					todayEntry.CPU.PeakPct = st.Max
				case "mem_used":
					todayEntry.RAM.AvgBytes = int64(st.Avg)
					todayEntry.RAM.PeakBytes = int64(st.Max)
				}
			}
			resp.History = append(resp.History, todayEntry)
		}
	}

	if s.metricsDB == nil {
		return resp, nil
	}

	query := `SELECT
    (d.ts / 86400) * 86400 AS day_ts,
    s.metric,
    d.avg,
    d.max
FROM sample_1d d
JOIN series s ON d.series_id = s.id
WHERE s.entity = ? AND d.ts >= ? AND d.ts < ?
ORDER BY d.ts DESC;`

	rows, err := s.metricsDB.RO.QueryContext(ctx, query, entity, sinceTS, todayMidnight)
	if err != nil {
		return nil, fmt.Errorf("query sample_1d: %w", err)
	}
	defer rows.Close()

	dayMap := make(map[int64]*DailyHistoryEntry)
	var dayOrder []int64

	for rows.Next() {
		var dayTS int64
		var metric string
		var avgVal, maxVal float64
		if err := rows.Scan(&dayTS, &metric, &avgVal, &maxVal); err != nil {
			return nil, err
		}

		entry, exists := dayMap[dayTS]
		if !exists {
			entry = &DailyHistoryEntry{
				Date: time.Unix(dayTS, 0).UTC().Format("2006-01-02"),
			}
			dayMap[dayTS] = entry
			dayOrder = append(dayOrder, dayTS)
		}

		switch metric {
		case "cpu_pct":
			entry.CPU.AvgPct = avgVal
			entry.CPU.PeakPct = maxVal
		case "mem_used":
			entry.RAM.AvgBytes = int64(avgVal)
			entry.RAM.PeakBytes = int64(maxVal)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, dTS := range dayOrder {
		resp.History = append(resp.History, *dayMap[dTS])
	}

	return resp, nil
}
