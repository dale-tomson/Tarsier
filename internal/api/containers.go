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

	"github.com/go-chi/chi/v5"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/metrics"
)

// ContainerListItem represents a single container in the listing endpoint.
type ContainerListItem struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Image        string                  `json:"image"`
	State        string                  `json:"state"`
	Health       string                  `json:"health"` // "green", "amber", "red"
	UptimeS      int64                   `json:"uptime_s"`
	RestartCount int                     `json:"restart_count"`
	FirstSeen    string                  `json:"first_seen"` // RFC 3339
	LastSeen     string                  `json:"last_seen"`  // RFC 3339
	Metrics      ContainerMetricsSummary `json:"metrics"`
	Today        ContainerTodaySummary   `json:"today"`
}

// ContainerMetricsSummary contains live telemetry snapshot.
type ContainerMetricsSummary struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemUsed  int64   `json:"mem_used"`
	MemLimit int64   `json:"mem_limit"`
	NetRxBps float64 `json:"net_rx_bps"`
	NetTxBps float64 `json:"net_tx_bps"`
}

// ContainerTodaySummary contains today's running average and peak metrics.
type ContainerTodaySummary struct {
	CPU struct {
		AvgPct  float64 `json:"avg_pct"`
		PeakPct float64 `json:"peak_pct"`
	} `json:"cpu"`
	RAM struct {
		AvgBytes  int64 `json:"avg_bytes"`
		PeakBytes int64 `json:"peak_bytes"`
	} `json:"ram"`
}

// ContainersListResponse is the payload returned by GET /api/v1/containers.
type ContainersListResponse struct {
	DockerConnected bool                `json:"docker_connected"`
	Containers      []ContainerListItem `json:"containers"`
}

// ContainerDetailResponse is the payload returned by GET /api/v1/containers/{name}.
type ContainerDetailResponse struct {
	Container    ContainerListItem       `json:"container"`
	DailyHistory []ContainerDailyHistory `json:"daily_history"`
	RecentEvents []ContainerRecentEvent  `json:"recent_events"`
}

// ContainerDailyHistory represents daily rollup aggregates for a container.
type ContainerDailyHistory struct {
	Date string `json:"date"` // YYYY-MM-DD
	CPU  struct {
		AvgPct  float64 `json:"avg_pct"`
		PeakPct float64 `json:"peak_pct"`
	} `json:"cpu"`
	RAM struct {
		AvgBytes  int64 `json:"avg_bytes"`
		PeakBytes int64 `json:"peak_bytes"`
	} `json:"ram"`
}

// ContainerRecentEvent represents a single container lifecycle transition event.
type ContainerRecentEvent struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"` // RFC 3339
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

func computeContainerHealth(state string, restartCount int) string {
	st := strings.ToLower(strings.TrimSpace(state))
	switch st {
	case "running":
		if restartCount > 0 {
			return "amber"
		}
		return "green"
	case "restarting", "paused":
		return "amber"
	case "exited", "dead", "destroyed":
		return "red"
	default:
		return "red"
	}
}

func (s *Server) isDockerConnected() bool {
	if s.dockerConnectedFunc != nil {
		return s.dockerConnectedFunc()
	}
	return s.cfg.Docker.Enabled == nil || *s.cfg.Docker.Enabled
}

func (s *Server) handleContainersList(w http.ResponseWriter, r *http.Request) {
	cacheKey := "GET:/api/v1/containers"
	data, err := s.queryCache.GetOrCompute(cacheKey, func() ([]byte, error) {
		resp, queryErr := s.queryContainersList(r.Context())
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

func (s *Server) queryContainersList(ctx context.Context) (*ContainersListResponse, error) {
	resp := &ContainersListResponse{
		DockerConnected: s.isDockerConnected(),
		Containers:      make([]ContainerListItem, 0),
	}

	if s.configDB == nil {
		return resp, nil
	}

	// 1. Fetch discovered containers from config.db
	rows, err := s.configDB.RO.QueryContext(ctx, `
		SELECT id, name, image, first_seen, last_seen, last_status, restart_count
		FROM container
		ORDER BY name ASC;
	`)
	if err != nil {
		return nil, fmt.Errorf("query containers: %w", err)
	}
	defer rows.Close()

	type rawContainer struct {
		id           string
		name         string
		image        string
		firstSeen    int64
		lastSeen     int64
		lastStatus   string
		restartCount int
	}

	var rawContainers []rawContainer
	for rows.Next() {
		var c rawContainer
		if err := rows.Scan(&c.id, &c.name, &c.image, &c.firstSeen, &c.lastSeen, &c.lastStatus, &c.restartCount); err != nil {
			return nil, fmt.Errorf("scan container: %w", err)
		}
		rawContainers = append(rawContainers, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. Fetch today's summary stats across all entities
	todayMap := make(map[string]map[string]metrics.DailyStat)
	if s.rollup != nil {
		summary, err := s.rollup.QueryTodaySummary(ctx, "")
		if err == nil {
			for _, st := range summary {
				if _, ok := todayMap[st.Entity]; !ok {
					todayMap[st.Entity] = make(map[string]metrics.DailyStat)
				}
				todayMap[st.Entity][st.Metric] = st
			}
		}
	}

	nowUnix := s.clock.Now().Unix()

	for _, c := range rawContainers {
		entity := "container:" + c.name
		item := ContainerListItem{
			ID:           c.id,
			Name:         c.name,
			Image:        c.image,
			State:        c.lastStatus,
			Health:       computeContainerHealth(c.lastStatus, c.restartCount),
			RestartCount: c.restartCount,
			FirstSeen:    time.Unix(c.firstSeen, 0).UTC().Format(time.RFC3339),
			LastSeen:     time.Unix(c.lastSeen, 0).UTC().Format(time.RFC3339),
		}

		if c.lastStatus == "running" && c.firstSeen > 0 && nowUnix >= c.firstSeen {
			item.UptimeS = nowUnix - c.firstSeen
		}

		// Live metrics from HotCache
		if s.hotCache != nil {
			latest := s.hotCache.GetEntityLatest(entity)
			if latest != nil {
				if p, ok := latest["cpu_pct"]; ok {
					item.Metrics.CPUPct = p.Value
				}
				if p, ok := latest["mem_used"]; ok {
					item.Metrics.MemUsed = int64(p.Value)
				}
				if p, ok := latest["mem_limit"]; ok {
					item.Metrics.MemLimit = int64(p.Value)
				}
				if p, ok := latest["net_rx_bps"]; ok {
					item.Metrics.NetRxBps = p.Value
				}
				if p, ok := latest["net_tx_bps"]; ok {
					item.Metrics.NetTxBps = p.Value
				}
			}
		}

		// Today's avg and peak metrics
		if metricsForEntity, ok := todayMap[entity]; ok {
			if cpuStat, ok := metricsForEntity["cpu_pct"]; ok {
				item.Today.CPU.AvgPct = cpuStat.Avg
				item.Today.CPU.PeakPct = cpuStat.Max
			}
			if memStat, ok := metricsForEntity["mem_used"]; ok {
				item.Today.RAM.AvgBytes = int64(memStat.Avg)
				item.Today.RAM.PeakBytes = int64(memStat.Max)
			}
		}

		resp.Containers = append(resp.Containers, item)
	}

	return resp, nil
}

func (s *Server) handleContainerDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", "container name is required")
		return
	}

	days := 7
	if dStr := r.URL.Query().Get("days"); dStr != "" {
		if d, err := strconv.Atoi(dStr); err == nil && d >= 1 && d <= 30 {
			days = d
		}
	}

	cacheKey := fmt.Sprintf("GET:/api/v1/containers/%s?days=%d", name, days)
	data, err := s.queryCache.GetOrCompute(cacheKey, func() ([]byte, error) {
		resp, queryErr := s.queryContainerDetail(r.Context(), name, days)
		if queryErr != nil {
			return nil, queryErr
		}
		return json.Marshal(resp)
	})

	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "container not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) queryContainerDetail(ctx context.Context, name string, days int) (*ContainerDetailResponse, error) {
	if s.configDB == nil {
		return nil, fmt.Errorf("container not found")
	}

	var (
		id           string
		cName        string
		image        string
		firstSeen    int64
		lastSeen     int64
		lastStatus   string
		restartCount int
	)

	err := s.configDB.RO.QueryRowContext(ctx, `
		SELECT id, name, image, first_seen, last_seen, last_status, restart_count
		FROM container
		WHERE name = ?
		ORDER BY last_seen DESC
		LIMIT 1;
	`, name).Scan(&id, &cName, &image, &firstSeen, &lastSeen, &lastStatus, &restartCount)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("container not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query container detail: %w", err)
	}

	nowUnix := s.clock.Now().Unix()
	item := ContainerListItem{
		ID:           id,
		Name:         cName,
		Image:        image,
		State:        lastStatus,
		Health:       computeContainerHealth(lastStatus, restartCount),
		RestartCount: restartCount,
		FirstSeen:    time.Unix(firstSeen, 0).UTC().Format(time.RFC3339),
		LastSeen:     time.Unix(lastSeen, 0).UTC().Format(time.RFC3339),
	}

	if lastStatus == "running" && firstSeen > 0 && nowUnix >= firstSeen {
		item.UptimeS = nowUnix - firstSeen
	}

	entity := "container:" + name
	if s.hotCache != nil {
		latest := s.hotCache.GetEntityLatest(entity)
		if latest != nil {
			if p, ok := latest["cpu_pct"]; ok {
				item.Metrics.CPUPct = p.Value
			}
			if p, ok := latest["mem_used"]; ok {
				item.Metrics.MemUsed = int64(p.Value)
			}
			if p, ok := latest["mem_limit"]; ok {
				item.Metrics.MemLimit = int64(p.Value)
			}
			if p, ok := latest["net_rx_bps"]; ok {
				item.Metrics.NetRxBps = p.Value
			}
			if p, ok := latest["net_tx_bps"]; ok {
				item.Metrics.NetTxBps = p.Value
			}
		}
	}

	// Today's summary
	if s.rollup != nil {
		summary, err := s.rollup.QueryTodaySummary(ctx, entity)
		if err == nil {
			for _, st := range summary {
				if st.Metric == "cpu_pct" {
					item.Today.CPU.AvgPct = st.Avg
					item.Today.CPU.PeakPct = st.Max
				} else if st.Metric == "mem_used" {
					item.Today.RAM.AvgBytes = int64(st.Avg)
					item.Today.RAM.PeakBytes = int64(st.Max)
				}
			}
		}
	}

	resp := &ContainerDetailResponse{
		Container:    item,
		DailyHistory: make([]ContainerDailyHistory, 0),
		RecentEvents: make([]ContainerRecentEvent, 0),
	}

	// Recent events from config.db
	evRows, err := s.configDB.RO.QueryContext(ctx, `
		SELECT id, ts, type, detail
		FROM container_event
		WHERE container_id = ?
		ORDER BY ts DESC
		LIMIT 50;
	`, id)
	if err == nil {
		defer evRows.Close()
		for evRows.Next() {
			var ev ContainerRecentEvent
			var ts int64
			if scanErr := evRows.Scan(&ev.ID, &ts, &ev.Type, &ev.Detail); scanErr == nil {
				ev.TS = time.Unix(ts, 0).UTC().Format(time.RFC3339)
				resp.RecentEvents = append(resp.RecentEvents, ev)
			}
		}
	}

	// Daily history from sample_1d in metrics.db
	if s.metricsDB != nil {
		minTS := nowUnix - int64(days*86400)
		histRows, err := s.metricsDB.RO.QueryContext(ctx, `
			SELECT (m.ts / 86400) * 86400 AS day_ts, s.metric, m.avg, m.max
			FROM sample_1d m
			JOIN series s ON m.series_id = s.id
			WHERE s.entity = ? AND m.ts >= ?
			ORDER BY day_ts ASC;
		`, entity, minTS)

		if err == nil {
			defer histRows.Close()

			dayMap := make(map[int64]*ContainerDailyHistory)
			dayOrder := make([]int64, 0)

			for histRows.Next() {
				var dayTS int64
				var metric string
				var avg, max float64
				if scanErr := histRows.Scan(&dayTS, &metric, &avg, &max); scanErr == nil {
					entry, exists := dayMap[dayTS]
					if !exists {
						entry = &ContainerDailyHistory{
							Date: time.Unix(dayTS, 0).UTC().Format("2006-01-02"),
						}
						dayMap[dayTS] = entry
						dayOrder = append(dayOrder, dayTS)
					}

					if metric == "cpu_pct" {
						entry.CPU.AvgPct = avg
						entry.CPU.PeakPct = max
					} else if metric == "mem_used" {
						entry.RAM.AvgBytes = int64(avg)
						entry.RAM.PeakBytes = int64(max)
					}
				}
			}

			for _, dTS := range dayOrder {
				resp.DailyHistory = append(resp.DailyHistory, *dayMap[dTS])
			}
		}
	}

	return resp, nil
}
