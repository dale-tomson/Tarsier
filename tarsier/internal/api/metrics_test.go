package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

type mockClock struct {
	now time.Time
}

func (m *mockClock) Now() time.Time {
	return m.now
}

func setupMetricsTestServer(t *testing.T, clock *mockClock) (*Server, *db.Store, *http.Cookie) {
	t.Helper()

	ctx := context.Background()
	tempDir := t.TempDir()
	store, err := db.Open(ctx, tempDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}

	password := "adminpass"
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	secret := bytes.Repeat([]byte("k"), 32)
	authenticator, err := auth.NewAuthenticator("admin", string(hash), secret)
	if err != nil {
		t.Fatalf("NewAuthenticator failed: %v", err)
	}

	sessions, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
		TTL:      24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSessionStore failed: %v", err)
	}

	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: 5,
		Lockout:     15 * time.Minute,
	})

	cfg := &config.Config{
		Server: config.ServerConfig{
			BaseURL: "http://localhost:8080",
		},
	}

	hotCache := metrics.NewHotCache(100, 180)
	var rollupClock model.Clock
	if clock != nil {
		rollupClock = clock
	}
	rollup := metrics.NewRollupEngine(store.Metrics, rollupClock)

	srv, err := NewServer(ServerConfig{
		Config:    cfg,
		ConfigDB:  store.Config,
		MetricsDB: store.Metrics,
		HotCache:  hotCache,
		Rollup:    rollup,
		Clock:     rollupClock,
		Auth:      authenticator,
		Sessions:  sessions,
		Limiter:   limiter,
		Version:   "test-1.0.0",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	cookie, err := sessions.CreateSession(ctx, false)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	return srv, store, cookie
}

func TestHostMetricsAPI_AutoTierSelection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := &mockClock{now: now}
	srv, store, cookie := setupMetricsTestServer(t, clock)
	defer store.Close()

	// Seed series
	reg := metrics.NewSeriesRegistry(store.Metrics, 100)
	sID, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil {
		t.Fatalf("create series failed: %v", err)
	}

	// 1. Seed HotCache (points from now-15m to now)
	for i := 0; i < 90; i++ {
		ts := now.Unix() - int64((90-i)*10)
		srv.hotCache.Record(model.MetricSample{
			Entity: "host",
			Metric: "cpu_pct",
			TS:     ts,
			Value:  float64(i),
		})
	}

	// 2. Seed sample_raw (points from now-3h to now)
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		for i := 0; i < 100; i++ {
			ts := now.Unix() - int64((100-i)*60)
			_, _ = tx.ExecContext(txCtx, "INSERT OR REPLACE INTO sample_raw VALUES (?, ?, ?);", sID, ts, 25.0)
		}
		// 3. Seed sample_1m (points from now-100m to now)
		for i := 0; i < 100; i++ {
			ts := now.Unix() - int64((100-i)*60)
			_, _ = tx.ExecContext(txCtx, "INSERT OR REPLACE INTO sample_1m VALUES (?, ?, ?, ?, ?);", sID, ts, 30.0, 10.0, 50.0)
		}
		// 4. Seed sample_5m (points from now-14d to now)
		for i := 0; i < 100; i++ {
			ts := now.Unix() - int64((100-i)*86400/2)
			_, _ = tx.ExecContext(txCtx, "INSERT OR REPLACE INTO sample_5m VALUES (?, ?, ?, ?, ?);", sID, ts, 40.0, 15.0, 75.0)
		}
		return nil
	})

	cases := []struct {
		name         string
		spanSec      int64
		stepParam    string
		expectedStep string
	}{
		{"HotCache (15m)", 15 * 60, "auto", "10s"},
		{"Raw (3h)", 3 * 3600, "auto", "10s"},
		{"1m Rollup (3d)", 3 * 86400, "auto", "1m"},
		{"5m Rollup (14d)", 14 * 86400, "auto", "5m"},
		{"Explicit 1m override on short span", 15 * 60, "1m", "1m"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := now.Unix() - tc.spanSec
			to := now.Unix()
			url := "/api/v1/host/metrics?metric=cpu_pct&entity=host&from=" +
				time.Unix(from, 0).UTC().Format(time.RFC3339) + "&to=" +
				time.Unix(to, 0).UTC().Format(time.RFC3339) + "&step=" + tc.stepParam

			req := httptest.NewRequest(http.MethodGet, url, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()

			srv.Router().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
			}

			var resp HostMetricsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response failed: %v", err)
			}

			if resp.Step != tc.expectedStep {
				t.Errorf("expected step %q, got %q", tc.expectedStep, resp.Step)
			}
			if resp.Entity != "host" || resp.Metric != "cpu_pct" {
				t.Errorf("unexpected entity/metric: %+v", resp)
			}
			if len(resp.Points) == 0 {
				t.Errorf("expected non-empty points array")
			}
		})
	}
}

func TestHostMetricsAPI_DynamicHotCacheBoundary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := &mockClock{now: now}
	srv, store, _ := setupMetricsTestServer(t, clock)
	defer store.Close()

	// Seed hotCache with 45 minutes of data (270 points at 10s interval) to verify spans > 30m still hit cache
	srv.hotCache = metrics.NewHotCache(100, 360)
	for i := 0; i < 270; i++ {
		ts := now.Unix() - int64((270-i)*10)
		srv.hotCache.Record(model.MetricSample{
			Entity: "host",
			Metric: "cpu_pct",
			TS:     ts,
			Value:  99.0,
		})
	}

	// Also put different values in sample_raw so we can distinguish cache vs DB
	reg := metrics.NewSeriesRegistry(store.Metrics, 100)
	sID, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil {
		t.Fatalf("create series failed: %v", err)
	}
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		for i := 0; i < 360; i++ {
			ts := now.Unix() - int64((360-i)*10)
			_, _ = tx.ExecContext(txCtx, "INSERT OR REPLACE INTO sample_raw VALUES (?, ?, ?);", sID, ts, 11.0)
		}
		return nil
	})

	// 40-minute query (within 45m hotCache): should hit hotCache (value == 99.0) even though span > 30m
	respCache, err := srv.queryHostMetrics(ctx, "host", "cpu_pct", now.Unix()-40*60, now.Unix(), "auto")
	if err != nil {
		t.Fatalf("queryHostMetrics (cache) failed: %v", err)
	}
	if len(respCache.Points) == 0 || respCache.Points[0].V != 99.0 {
		t.Errorf("expected hotCache hit (V=99.0) for 40m span within OldestTS, got %+v", respCache.Points[:min(1, len(respCache.Points))])
	}

	// 55-minute query (older than 45m OldestTS): should fall back to sample_raw (value == 11.0)
	respRaw, err := srv.queryHostMetrics(ctx, "host", "cpu_pct", now.Unix()-55*60, now.Unix(), "auto")
	if err != nil {
		t.Fatalf("queryHostMetrics (raw) failed: %v", err)
	}
	if len(respRaw.Points) == 0 || respRaw.Points[0].V != 11.0 {
		t.Errorf("expected sample_raw fallback (V=11.0) for 55m span beyond OldestTS, got %+v", respRaw.Points[:min(1, len(respRaw.Points))])
	}
}

func TestHostMetricsAPI_ValidationAndClamping(t *testing.T) {
	srv, store, cookie := setupMetricsTestServer(t, nil)
	defer store.Close()

	// 1. Missing metric parameter -> 400 Bad Request
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/host/metrics", nil)
	req1.AddCookie(cookie)
	rec1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on missing metric, got %d", rec1.Code)
	}

	var errResp map[string]map[string]string
	_ = json.Unmarshal(rec1.Body.Bytes(), &errResp)
	if errResp["error"]["code"] != "invalid_param" {
		t.Errorf("expected error code invalid_param, got %q", errResp["error"]["code"])
	}

	// 2. from >= to -> 400 Bad Request
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/host/metrics?metric=cpu_pct&from=1700000000&to=1600000000", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on from >= to, got %d", rec2.Code)
	}

	// 3. Unauthenticated request -> 401 Unauthorized
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/host/metrics?metric=cpu_pct", nil)
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec3.Code)
	}
}

func TestDailyMetricsAPI_SummaryAndHistory(t *testing.T) {
	ctx := context.Background()
	today := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	clock := &mockClock{now: today}
	srv, store, cookie := setupMetricsTestServer(t, clock)
	defer store.Close()

	reg := metrics.NewSeriesRegistry(store.Metrics, 100)
	hostCPU, _ := reg.GetOrCreate(ctx, "host", "cpu_pct")
	hostRAM, _ := reg.GetOrCreate(ctx, "host", "mem_used")
	hostRAMTot, _ := reg.GetOrCreate(ctx, "host", "mem_total")
	apiCPU, _ := reg.GetOrCreate(ctx, "container:api-service", "cpu_pct")
	apiRAM, _ := reg.GetOrCreate(ctx, "container:api-service", "mem_used")

	// Seed today's sample_1m
	todayMidnight := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC).Unix()
	_ = store.Metrics.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", hostCPU, todayMidnight+3600, 14.2, 5.0, 68.5)
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", hostRAM, todayMidnight+3600, 1450280000.0, 1000000000.0, 2100500000.0)
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", apiCPU, todayMidnight+3600, 4.1, 1.0, 28.3)
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1m VALUES (?, ?, ?, ?, ?);", apiRAM, todayMidnight+3600, 74500000.0, 50000000.0, 118200000.0)

		// Seed mem_total in sample_raw
		_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_raw VALUES (?, ?, ?);", hostRAMTot, todayMidnight+3600, 4294967296.0)

		// Seed 7 days of historical sample_1d
		for d := 1; d <= 7; d++ {
			dayTS := todayMidnight - int64(d*86400)
			_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1d VALUES (?, ?, ?, ?, ?);", hostCPU, dayTS, 12.0+float64(d), 5.0, 50.0+float64(d))
			_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1d VALUES (?, ?, ?, ?, ?);", hostRAM, dayTS, 1390000000.0, 1000000000.0, 1980000000.0)
			_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1d VALUES (?, ?, ?, ?, ?);", apiCPU, dayTS, 5.0+float64(d), 1.0, 30.0)
			_, _ = tx.ExecContext(txCtx, "INSERT INTO sample_1d VALUES (?, ?, ?, ?, ?);", apiRAM, dayTS, 70000000.0, 50000000.0, 110000000.0)
		}
		return nil
	})

	// 1. GET /api/v1/metrics/daily/summary
	reqSummary := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/daily/summary", nil)
	reqSummary.AddCookie(cookie)
	recSummary := httptest.NewRecorder()
	srv.Router().ServeHTTP(recSummary, reqSummary)

	if recSummary.Code != http.StatusOK {
		t.Fatalf("summary request failed with %d: %s", recSummary.Code, recSummary.Body.String())
	}

	var summary TodaySummaryResponse
	if err := json.Unmarshal(recSummary.Body.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal summary failed: %v", err)
	}

	if summary.Date != "2026-11-13" {
		t.Errorf("expected date 2026-11-13, got %s", summary.Date)
	}
	if summary.Host.CPU.AvgPct != 14.2 || summary.Host.CPU.PeakPct != 68.5 {
		t.Errorf("unexpected host CPU stats: %+v", summary.Host.CPU)
	}
	if summary.Host.CPU.Cores <= 0 {
		t.Errorf("expected host CPU cores > 0, got %d", summary.Host.CPU.Cores)
	}
	if summary.Host.RAM.AvgBytes != 1450280000 || summary.Host.RAM.PeakBytes != 2100500000 || summary.Host.RAM.TotalBytes != 4294967296 {
		t.Errorf("unexpected host RAM stats: %+v", summary.Host.RAM)
	}
	if len(summary.Containers) != 1 || summary.Containers[0].Name != "api-service" {
		t.Fatalf("expected 1 container 'api-service', got %+v", summary.Containers)
	}
	if summary.Containers[0].CPU.AvgPct != 4.1 || summary.Containers[0].CPU.PeakPct != 28.3 {
		t.Errorf("unexpected container CPU stats: %+v", summary.Containers[0].CPU)
	}

	// 2. GET /api/v1/metrics/daily?entity=host&days=7
	reqHist := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/daily?entity=host&days=7", nil)
	reqHist.AddCookie(cookie)
	recHist := httptest.NewRecorder()
	srv.Router().ServeHTTP(recHist, reqHist)

	if recHist.Code != http.StatusOK {
		t.Fatalf("history request failed with %d: %s", recHist.Code, recHist.Body.String())
	}

	var hist DailyHistoryResponse
	if err := json.Unmarshal(recHist.Body.Bytes(), &hist); err != nil {
		t.Fatalf("unmarshal history failed: %v", err)
	}

	if hist.Days != 7 || len(hist.History) != 7 {
		t.Fatalf("expected 7 history days, got days=%d, len=%d", hist.Days, len(hist.History))
	}
	if hist.History[0].Date != "2026-11-12" {
		t.Errorf("expected latest historical day 2026-11-12, got %s", hist.History[0].Date)
	}
}

func TestQueryCache_TTLExpiryAndCap(t *testing.T) {
	cache := NewQueryCache(3, 50*time.Millisecond)

	computeCount := 0
	computeFn := func() ([]byte, error) {
		computeCount++
		return []byte("payload"), nil
	}

	// 1. First call computes
	data, err := cache.GetOrCompute("key1", computeFn)
	if err != nil || string(data) != "payload" || computeCount != 1 {
		t.Fatalf("initial compute failed: %v, count=%d", err, computeCount)
	}

	// 2. Immediate second call serves from cache (computeCount remains 1)
	data, err = cache.GetOrCompute("key1", computeFn)
	if err != nil || computeCount != 1 {
		t.Fatalf("expected cache hit, count=%d", computeCount)
	}

	// 3. Wait for TTL to expire
	time.Sleep(60 * time.Millisecond)
	data, err = cache.GetOrCompute("key1", computeFn)
	if err != nil || computeCount != 2 {
		t.Fatalf("expected cache expiry and recompute, count=%d", computeCount)
	}

	// 4. Test max capacity eviction (cap = 3)
	for i := 2; i <= 6; i++ {
		_, _ = cache.GetOrCompute(string(rune('a'+i)), computeFn)
	}
	if cache.Len() > 3 {
		t.Fatalf("cache size %d exceeded capacity 3", cache.Len())
	}

	// 5. Invalidate flushes
	cache.Invalidate()
	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after Invalidate, got %d", cache.Len())
	}
}
