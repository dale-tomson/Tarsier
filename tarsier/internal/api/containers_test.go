package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestContainersAPI_ListMergesLiveAndDailyStats(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	clock := &mockClock{now: now}

	srv, store, cookie := setupMetricsTestServer(t, clock)
	defer store.Close()

	// 1. Seed container records in config.db
	err := store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
			VALUES
				('id-green', 'service-green', 'alpine:3.18', 1700000000, 1700001000, 'running', 0),
				('id-amber', 'service-amber', 'redis:7', 1700000000, 1700001000, 'running', 2),
				('id-red', 'service-red', 'postgres:16', 1700000000, 1700001000, 'exited', 0);
		`)
		return err
	})
	if err != nil {
		t.Fatalf("seed containers failed: %v", err)
	}

	// 2. Push live metrics into HotCache for service-green
	srv.hotCache.Record(
		model.MetricSample{Entity: "container:service-green", Metric: "cpu_pct", TS: now.Unix(), Value: 12.5},
		model.MetricSample{Entity: "container:service-green", Metric: "mem_used", TS: now.Unix(), Value: 52428800},
		model.MetricSample{Entity: "container:service-green", Metric: "mem_limit", TS: now.Unix(), Value: 268435456},
		model.MetricSample{Entity: "container:service-green", Metric: "net_rx_bps", TS: now.Unix(), Value: 1024.0},
		model.MetricSample{Entity: "container:service-green", Metric: "net_tx_bps", TS: now.Unix(), Value: 2048.0},
	)

	// 3. Seed today's sample_1m aggregates in metrics.db
	todayStart := (now.Unix() / 86400) * 86400
	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `INSERT INTO series (entity, metric) VALUES ('container:service-green', 'cpu_pct');`)
		if err != nil {
			return err
		}
		cpuSeriesID, _ := res.LastInsertId()

		res, err = tx.ExecContext(txCtx, `INSERT INTO series (entity, metric) VALUES ('container:service-green', 'mem_used');`)
		if err != nil {
			return err
		}
		memSeriesID, _ := res.LastInsertId()

		// Insert 1m rollup sample
		_, err = tx.ExecContext(txCtx, `
			INSERT INTO sample_1m (series_id, ts, avg, min, max)
			VALUES
				(?, ?, 10.0, 5.0, 30.0),
				(?, ?, 50000000.0, 40000000.0, 60000000.0);
		`, cpuSeriesID, todayStart+60, memSeriesID, todayStart+60)
		return err
	})
	if err != nil {
		t.Fatalf("seed metrics failed: %v", err)
	}

	// 4. Query GET /api/v1/containers
	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ContainersListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if !resp.DockerConnected {
		t.Errorf("expected DockerConnected to be true")
	}

	if len(resp.Containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(resp.Containers))
	}

	// Verify health mappings
	containerMap := make(map[string]ContainerListItem)
	for _, c := range resp.Containers {
		containerMap[c.Name] = c
	}

	green := containerMap["service-green"]
	if green.Health != "green" {
		t.Errorf("expected service-green health 'green', got %q", green.Health)
	}
	if green.Metrics.CPUPct != 12.5 {
		t.Errorf("expected live CPU 12.5, got %.2f", green.Metrics.CPUPct)
	}
	if green.Metrics.MemUsed != 52428800 {
		t.Errorf("expected live MemUsed 52428800, got %d", green.Metrics.MemUsed)
	}
	if green.Today.CPU.AvgPct != 10.0 || green.Today.CPU.PeakPct != 30.0 {
		t.Errorf("expected today CPU avg 10.0 / peak 30.0, got avg %.2f / peak %.2f", green.Today.CPU.AvgPct, green.Today.CPU.PeakPct)
	}
	if green.Today.RAM.AvgBytes != 50000000 || green.Today.RAM.PeakBytes != 60000000 {
		t.Errorf("expected today RAM avg 50000000 / peak 60000000, got avg %d / peak %d", green.Today.RAM.AvgBytes, green.Today.RAM.PeakBytes)
	}

	amber := containerMap["service-amber"]
	if amber.Health != "amber" {
		t.Errorf("expected service-amber health 'amber', got %q", amber.Health)
	}

	red := containerMap["service-red"]
	if red.Health != "red" {
		t.Errorf("expected service-red health 'red', got %q", red.Health)
	}
}

func TestContainersAPI_DetailAndNotFound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	clock := &mockClock{now: now}

	srv, store, cookie := setupMetricsTestServer(t, clock)
	defer store.Close()

	// Seed container and events
	err := store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
			VALUES ('cid-100', 'api-service', 'api:v2', 1700000000, 1700005000, 'running', 1);

			INSERT INTO container_event (container_id, ts, type, detail)
			VALUES
				('cid-100', 1700001000, 'start', ''),
				('cid-100', 1700002000, 'oom', 'oom=true'),
				('cid-100', 1700003000, 'die', 'exit_code=137 oom=true');
		`)
		return err
	})
	if err != nil {
		t.Fatalf("seed config failed: %v", err)
	}

	// Seed sample_1d historical daily metrics
	day1 := (now.Unix() / 86400) * 86400 - 86400
	day2 := day1 - 86400
	err = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `INSERT INTO series (entity, metric) VALUES ('container:api-service', 'cpu_pct');`)
		if err != nil {
			return err
		}
		sID, _ := res.LastInsertId()

		_, err = tx.ExecContext(txCtx, `
			INSERT INTO sample_1d (series_id, ts, avg, min, max)
			VALUES
				(?, ?, 5.5, 1.0, 22.0),
				(?, ?, 7.0, 2.0, 35.0);
		`, sID, day2, sID, day1)
		return err
	})
	if err != nil {
		t.Fatalf("seed sample_1d failed: %v", err)
	}

	// 1. Successful container detail
	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers/api-service?days=7", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var detail ContainerDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail response failed: %v", err)
	}

	if detail.Container.Name != "api-service" {
		t.Errorf("expected container name 'api-service', got %q", detail.Container.Name)
	}
	if len(detail.RecentEvents) != 3 {
		t.Errorf("expected 3 recent events, got %d", len(detail.RecentEvents))
	}
	if len(detail.DailyHistory) != 2 {
		t.Errorf("expected 2 daily history entries, got %d", len(detail.DailyHistory))
	}

	// 2. 404 Not Found for non-existing container
	req404 := httptest.NewRequest(http.MethodGet, "/api/v1/containers/nonexistent", nil)
	req404.AddCookie(cookie)
	rec404 := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec404, req404)

	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", rec404.Code)
	}

	var errResp map[string]map[string]string
	_ = json.Unmarshal(rec404.Body.Bytes(), &errResp)
	if errResp["error"]["code"] != "not_found" {
		t.Errorf("expected error code 'not_found', got %v", errResp)
	}
}

func TestContainersAPI_RequiresAuth(t *testing.T) {
	srv, store, _ := setupMetricsTestServer(t, nil)
	defer store.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
	}
}

func TestContainersAPI_DeduplicatesMultipleIDsPerName(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	clock := &mockClock{now: now}

	srv, store, cookie := setupMetricsTestServer(t, clock)
	defer store.Close()

	// Seed multiple rows for same container name across restarts/recreations
	err := store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
			VALUES
				('old-id', 'recreated-app', 'nginx:alpine', 1700000000, 1700001000, 'running', 0),
				('new-id', 'recreated-app', 'nginx:alpine', 1700001000, 1700002000, 'running', 1),
				('exited-id', 'other-app', 'redis:7', 1700000500, 1700000900, 'exited', 0);
		`)
		return err
	})
	if err != nil {
		t.Fatalf("seed containers failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var resp ContainersListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list response failed: %v", err)
	}

	// Must contain exactly 2 containers: 1 for recreated-app and 1 for other-app
	if len(resp.Containers) != 2 {
		t.Fatalf("expected 2 deduplicated containers, got %d: %+v", len(resp.Containers), resp.Containers)
	}

	for _, c := range resp.Containers {
		if c.Name == "recreated-app" {
			if c.ID != "new-id" {
				t.Errorf("expected active container ID 'new-id', got %q", c.ID)
			}
			if c.RestartCount != 1 {
				t.Errorf("expected restart count 1, got %d", c.RestartCount)
			}
		}
	}
}
