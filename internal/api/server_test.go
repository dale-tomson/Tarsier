package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/web"
)

func setupTestServer(t *testing.T) (*Server, *httptest.Server, func()) {
	t.Helper()

	tempDir := t.TempDir()
	store, err := db.Open(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}

	password := "supersecret123"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt generate failed: %v", err)
	}

	secret := bytes.Repeat([]byte("s"), 32)
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
			Listen:   "127.0.0.1:0",
			BaseURL:  "http://localhost:8080",
			Timezone: "UTC",
		},
		Storage: config.StorageConfig{
			Path:           tempDir,
			LogsMaxSize:    "10GiB",
			MetricsMaxSize: "5GiB",
		},
		Retention: config.RetentionConfig{
			Logs:       "14d",
			MetricsRaw: "7d",
			Metrics1d:  "90d",
		},
		Notifiers: config.NotifiersConfig{
			Email: config.EmailNotifierConfig{
				Password: "my-secret-email-password",
			},
			Discord: config.DiscordNotifierConfig{
				WebhookURL: "https://discord.com/api/webhooks/1234/my-secret-webhook",
			},
		},
	}

	srv, err := NewServer(ServerConfig{
		Config:    cfg,
		ConfigDB:  store.Config,
		MetricsDB: store.Metrics,
		Auth:      authenticator,
		Sessions:  sessions,
		Limiter:   limiter,
		Version:   "test-0.5.0",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(srv.Router())

	cleanup := func() {
		ts.Close()
		_ = store.Close()
	}

	return srv, ts, cleanup
}

func decompressGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer reader.Close()
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to decompress gzip data: %v", err)
	}
	return decompressed
}

func TestStaticAssets_ServedAndCompressed(t *testing.T) {
	_, ts, cleanup := setupTestServer(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	paths := []struct {
		urlPath          string
		expectedType     string
		checkVendorCache bool
	}{
		{"/", "text/html; charset=utf-8", false},
		{"/static/vendor/petite-vue.iife.js", "application/javascript; charset=utf-8", true},
		{"/static/vendor/pico.min.css", "text/css; charset=utf-8", true},
		{"/static/vendor/uPlot.iife.min.js", "application/javascript; charset=utf-8", true},
		{"/static/vendor/uPlot.min.css", "text/css; charset=utf-8", true},
		{"/static/js/app.js", "application/javascript; charset=utf-8", false},
		{"/static/css/app.css", "text/css; charset=utf-8", false},
	}

	for _, p := range paths {
		t.Run(p.urlPath, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+p.urlPath, nil)
			if err != nil {
				t.Fatalf("NewRequest failed: %v", err)
			}
			req.Header.Set("Accept-Encoding", "gzip")

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("client.Do failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d for %s", resp.StatusCode, p.urlPath)
			}

			// Gzip header verification
			encoding := resp.Header.Get("Content-Encoding")
			if encoding != "gzip" {
				t.Errorf("expected Content-Encoding: gzip, got %q", encoding)
			}

			// Content-Type verification
			ctype := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ctype, p.expectedType) {
				t.Errorf("expected Content-Type %q, got %q", p.expectedType, ctype)
			}

			// Cache-Control verification
			cacheCtrl := resp.Header.Get("Cache-Control")
			if p.checkVendorCache {
				if !strings.Contains(cacheCtrl, "max-age=31536000") || !strings.Contains(cacheCtrl, "immutable") {
					t.Errorf("expected vendor Cache-Control with max-age=31536000 and immutable, got %q", cacheCtrl)
				}
			} else if p.urlPath == "/" {
				if !strings.Contains(cacheCtrl, "no-cache") {
					t.Errorf("expected / Cache-Control: no-cache, got %q", cacheCtrl)
				}
			} else if strings.HasPrefix(p.urlPath, "/static/js/") || strings.HasPrefix(p.urlPath, "/static/css/") {
				if !strings.Contains(cacheCtrl, "max-age=300") {
					t.Errorf("expected /static Cache-Control: max-age=300, got %q", cacheCtrl)
				}
			}

			// Read and verify body is valid compressed data
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read response body: %v", err)
			}
			if len(bodyBytes) == 0 {
				t.Errorf("expected non-empty response body for %s", p.urlPath)
			}

			decompressed := decompressGzip(t, bodyBytes)
			if len(decompressed) == 0 {
				t.Errorf("decompressed body is empty for %s", p.urlPath)
			}
		})
	}
}

func TestETag_NotModified(t *testing.T) {
	_, ts, cleanup := setupTestServer(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}
	url := ts.URL + "/static/vendor/pico.min.css"

	// 1. Initial request to capture ETag
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("first GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("expected non-empty ETag header")
	}

	// 2. Follow-up request with If-None-Match
	req2, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest 2 failed: %v", err)
	}
	req2.Header.Set("If-None-Match", etag)

	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second GET failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("expected 304 Not Modified, got %d", resp2.StatusCode)
	}

	body, _ := io.ReadAll(resp2.Body)
	if len(body) != 0 {
		t.Errorf("expected empty body on 304, got %d bytes", len(body))
	}
}

func TestHealthz_Unauthenticated(t *testing.T) {
	_, ts, cleanup := setupTestServer(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var data map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode healthz JSON failed: %v", err)
	}

	if data["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", data["status"])
	}
	if data["version"] != "test-0.5.0" {
		t.Errorf("expected version 'test-0.5.0', got %q", data["version"])
	}
}

func TestProtectedAPI_RequiresAuth(t *testing.T) {
	_, ts, cleanup := setupTestServer(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. GET /api/v1/system/config without cookie -> 401 Unauthorized
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system/config", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized without session cookie, got %d", resp.StatusCode)
	}

	// 2. Authenticate via POST /api/v1/auth/login
	loginPayload := `{"username":"admin","password":"supersecret123"}`
	loginResp, err := client.Post(ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(loginPayload))
	if err != nil {
		t.Fatalf("POST /login failed: %v", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on login, got %d", loginResp.StatusCode)
	}

	cookies := loginResp.Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("session cookie %q not set in login response", auth.SessionCookieName)
	}

	// 3. GET /api/v1/system/config with cookie -> 200 OK with masked secrets
	reqWithAuth, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system/config", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	reqWithAuth.AddCookie(sessionCookie)

	cfgResp, err := client.Do(reqWithAuth)
	if err != nil {
		t.Fatalf("authenticated GET failed: %v", err)
	}
	defer cfgResp.Body.Close()

	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK with session cookie, got %d", cfgResp.StatusCode)
	}

	var cfgData config.Config
	if err := json.NewDecoder(cfgResp.Body).Decode(&cfgData); err != nil {
		t.Fatalf("decode config JSON failed: %v", err)
	}

	// Verify secrets are masked
	if cfgData.Notifiers.Email.Password != "********" {
		t.Errorf("expected email password to be masked, got %q", cfgData.Notifiers.Email.Password)
	}
	if cfgData.Notifiers.Discord.WebhookURL != "********" {
		t.Errorf("expected discord webhook to be masked, got %q", cfgData.Notifiers.Discord.WebhookURL)
	}

	// 4. GET /api/v1/system/health with cookie -> 200 OK with uptime, memory
	reqHealth, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system/health", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	reqHealth.AddCookie(sessionCookie)

	healthResp, err := client.Do(reqHealth)
	if err != nil {
		t.Fatalf("authenticated health GET failed: %v", err)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /system/health, got %d", healthResp.StatusCode)
	}

	var healthData SystemHealthResponse
	if err := json.NewDecoder(healthResp.Body).Decode(&healthData); err != nil {
		t.Fatalf("decode health JSON failed: %v", err)
	}

	if healthData.Status != "ok" {
		t.Errorf("expected health status 'ok', got %q", healthData.Status)
	}
	if healthData.Memory.SysBytes == 0 {
		t.Errorf("expected non-zero SysBytes")
	}
}

func TestGracefulShutdown(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: "127.0.0.1:0",
		},
	}

	srv, err := NewServer(ServerConfig{
		Config:  cfg,
		Version: "test-shutdown",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.StartListener(ln)
	}()

	// Ensure server is accepting requests
	client := &http.Client{Timeout: 1 * time.Second}
	var ready bool
	for range 10 {
		resp, err := client.Get("http://" + srv.Addr() + "/healthz")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server failed to become ready")
	}

	// Trigger graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer shutdownCancel()

	start := time.Now()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("shutdown took too long: %v (expected < 1s)", elapsed)
	}

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("unexpected server exit error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
	_ = os.RemoveAll(tempDir)
}

func TestGzipPayloadBudget(t *testing.T) {
	// Assert total gzipped payload of index.html + petite-vue + Pico.css + uPlot + app.js < 65 KB
	paths := []string{
		"index.html",
		"static/vendor/petite-vue.iife.js",
		"static/vendor/pico.min.css",
		"static/vendor/uPlot.iife.min.js",
		"static/vendor/uPlot.min.css",
		"static/css/app.css",
		"static/js/components.js",
		"static/js/app.js",
	}

	var combined bytes.Buffer
	for _, p := range paths {
		data, err := web.Assets.ReadFile(p)
		if err != nil {
			t.Fatalf("failed to read embedded %s: %v", p, err)
		}
		combined.Write(data)
	}

	var gzBuf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&gzBuf, 5)
	if err != nil {
		t.Fatalf("NewWriterLevel failed: %v", err)
	}
	if _, err := gw.Write(combined.Bytes()); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}

	totalGzipBytes := gzBuf.Len()
	const maxAllowedBytes = 65 * 1024 // 65 KB

	t.Logf("Total gzipped bundle size: %d bytes (%.2f KB) / limit %d bytes",
		totalGzipBytes, float64(totalGzipBytes)/1024, maxAllowedBytes)

	if totalGzipBytes > maxAllowedBytes {
		t.Errorf("gzipped payload exceeded budget: got %d bytes, max %d bytes", totalGzipBytes, maxAllowedBytes)
	}
}

func TestNoExternalCDNLinks(t *testing.T) {
	indexHTML, err := web.Assets.ReadFile("index.html")
	if err != nil {
		t.Fatalf("failed to read index.html: %v", err)
	}

	content := string(indexHTML)
	suspicious := []string{"http://", "https://", "cdn.", "unpkg.com", "jsdelivr.net", "cdnjs.cloudflare.com"}
	for _, term := range suspicious {
		if strings.Contains(content, term) {
			t.Errorf("found disallowed external link/CDN term %q in index.html for air-gapped compliance", term)
		}
	}
}

func TestSPAFallback(t *testing.T) {
	_, ts, cleanup := setupTestServer(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	// Any non-static, non-api route should return 200 OK with index.html
	spaRoutes := []string{"/overview", "/host", "/containers", "/logs", "/alerts", "/settings"}
	for _, route := range spaRoutes {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s failed: %v", route, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for SPA fallback %s, got %d", route, resp.StatusCode)
		}
		if !strings.Contains(string(body), "<title>Tarsier</title>") {
			t.Errorf("expected index.html body for SPA route %s", route)
		}
	}

	// Non-existent static asset should return 404
	resp404, err := client.Get(ts.URL + "/static/nonexistent.js")
	if err != nil {
		t.Fatalf("GET /static/nonexistent.js failed: %v", err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for non-existent static asset, got %d", resp404.StatusCode)
	}
}
