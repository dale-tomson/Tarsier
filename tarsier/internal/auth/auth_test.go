package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/db"
	"golang.org/x/crypto/bcrypt"
)

func setupTestStore(t *testing.T) *db.Store {
	t.Helper()
	dataDir := t.TempDir()
	store, err := db.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("failed to open test db store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func setupTestAuthenticator(t *testing.T, username, password string) (*auth.Authenticator, []byte) {
	t.Helper()
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to generate bcrypt hash: %v", err)
	}

	secret := []byte("01234567890123456789012345678901") // 32 bytes
	authenticator, err := auth.NewAuthenticator(username, string(hashBytes), secret)
	if err != nil {
		t.Fatalf("failed to create authenticator: %v", err)
	}

	return authenticator, secret
}

// 1. TestLogin_SuccessAndSessionLifecycle:
// - Valid username/password returns 200 OK and sets HttpOnly, SameSite=Lax cookie.
// - Subsequent request to GET /api/v1/auth/me with cookie succeeds (200 OK); without cookie returns 401.
// - POST /api/v1/auth/logout deletes row from config.db and invalidates the cookie.
func TestLogin_SuccessAndSessionLifecycle(t *testing.T) {
	store := setupTestStore(t)
	authenticator, secret := setupTestAuthenticator(t, "admin", "correct-horse-battery")

	sessionStore, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
		TTL:      1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create session store: %v", err)
	}

	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: 5,
		Lockout:     15 * time.Minute,
	})

	handlers := auth.NewHandlers(authenticator, sessionStore, limiter, "http://localhost:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", handlers.LoginHandler)
	mux.HandleFunc("POST /api/v1/auth/logout", handlers.LogoutHandler)
	mux.HandleFunc("GET /api/v1/auth/me", handlers.MeHandler)

	// Step 1: Login with invalid credentials -> 401
	badLoginPayload := `{"username":"admin","password":"wrong-password"}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(badLoginPayload))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad password, got %d", rec1.Code)
	}

	// Step 2: Login with valid credentials -> 200 and cookie
	goodLoginPayload := `{"username":"admin","password":"correct-horse-battery"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(goodLoginPayload))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for good login, got %d", rec2.Code)
	}

	var userResp auth.UserResponse
	if err := json.NewDecoder(rec2.Body).Decode(&userResp); err != nil {
		t.Fatalf("failed to decode user response: %v", err)
	}
	if userResp.Username != "admin" {
		t.Errorf("expected username 'admin', got %q", userResp.Username)
	}

	cookies := rec2.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie to be set in login response")
	}
	if !sessionCookie.HttpOnly {
		t.Error("expected session cookie to be HttpOnly")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected session cookie SameSite=Lax, got %v", sessionCookie.SameSite)
	}

	// Step 3: GET /me without cookie -> 401
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for /me without cookie, got %d", rec3.Code)
	}

	// Step 4: GET /me with valid session cookie -> 200 OK
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req4.AddCookie(sessionCookie)
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, req4)

	if rec4.Code != http.StatusOK {
		t.Fatalf("expected 200 for /me with cookie, got %d", rec4.Code)
	}
	var meResp auth.UserResponse
	if err := json.NewDecoder(rec4.Body).Decode(&meResp); err != nil {
		t.Fatalf("failed to decode /me response: %v", err)
	}
	if meResp.Username != "admin" {
		t.Errorf("expected username 'admin', got %q", meResp.Username)
	}

	// Step 5: POST /logout with session cookie -> 200 OK and expired cookie
	req5 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req5.AddCookie(sessionCookie)
	rec5 := httptest.NewRecorder()
	mux.ServeHTTP(rec5, req5)

	if rec5.Code != http.StatusOK {
		t.Errorf("expected 200 for logout, got %d", rec5.Code)
	}

	logoutCookies := rec5.Result().Cookies()
	var expiredCookieFound bool
	for _, c := range logoutCookies {
		if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
			expiredCookieFound = true
			break
		}
	}
	if !expiredCookieFound {
		t.Error("expected expired session cookie in logout response")
	}

	// Step 6: Subsequent GET /me with original session cookie -> 401 (deleted from DB)
	req6 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req6.AddCookie(sessionCookie)
	rec6 := httptest.NewRecorder()
	mux.ServeHTTP(rec6, req6)

	if rec6.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after logout, got %d", rec6.Code)
	}
}

// 2. TestSession_TamperingAndExpiry:
// - Modifying signature or expiry timestamp is rejected with 401 without hitting DB.
// - Advancing fake clock past session_ttl rejects session.
// - PruneExpiredSessions cleans up expired DB rows.
func TestSession_TamperingAndExpiry(t *testing.T) {
	store := setupTestStore(t)
	_, secret := setupTestAuthenticator(t, "admin", "secret")

	fakeNow := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var currentClockMu sync.Mutex
	currentClock := fakeNow

	nowFn := func() time.Time {
		currentClockMu.Lock()
		defer currentClockMu.Unlock()
		return currentClock
	}

	sessionStore, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
		TTL:      30 * time.Minute,
		Clock:    nowFn,
	})
	if err != nil {
		t.Fatalf("failed to create session store: %v", err)
	}

	cookie, err := sessionStore.CreateSession(context.Background(), false)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Validate authentic cookie succeeds
	sid, err := sessionStore.ValidateCookie(context.Background(), cookie.Value)
	if err != nil || sid == "" {
		t.Fatalf("expected cookie to validate successfully, got err: %v", err)
	}

	// Case A: Tamper signature byte
	tamperedSig := cookie.Value[:len(cookie.Value)-1] + "x"
	_, err = sessionStore.ValidateCookie(context.Background(), tamperedSig)
	if err == nil {
		t.Error("expected error for tampered signature, got nil")
	}

	// Case B: Tamper expiry timestamp
	parts := strings.Split(cookie.Value, ".")
	tamperedExp := fmt.Sprintf("%s.%d.%s", parts[0], time.Now().Add(10*time.Hour).Unix(), parts[2])
	_, err = sessionStore.ValidateCookie(context.Background(), tamperedExp)
	if err == nil {
		t.Error("expected error for tampered expiry timestamp, got nil")
	}

	// Case C: Advance clock past 30-minute TTL -> rejected as expired
	currentClockMu.Lock()
	currentClock = currentClock.Add(31 * time.Minute)
	currentClockMu.Unlock()

	_, err = sessionStore.ValidateCookie(context.Background(), cookie.Value)
	if err == nil {
		t.Error("expected error after advancing past session TTL, got nil")
	}

	// Case D: PruneExpiredSessions clears the expired row from config.db
	if err := sessionStore.PruneExpiredSessions(context.Background()); err != nil {
		t.Fatalf("prune expired sessions failed: %v", err)
	}

	// Verify row no longer exists in DB
	var count int
	row := store.Config.QueryRO().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM session WHERE id = ?;", sid)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query session count failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 sessions after pruning, got %d", count)
	}
}

// 3. TestRateLimiter_LockoutAndReset:
// - 5 consecutive failed logins trigger HTTP 429 on the 6th attempt (even with valid credentials).
// - Advancing fake clock past auth.lockout (15m) allows login again and resets counter upon success.
func TestRateLimiter_LockoutAndReset(t *testing.T) {
	store := setupTestStore(t)
	authenticator, secret := setupTestAuthenticator(t, "admin", "correct-password")

	fakeNow := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentClock := fakeNow
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentClock
	}

	sessionStore, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
		TTL:      1 * time.Hour,
		Clock:    nowFn,
	})
	if err != nil {
		t.Fatalf("failed to create session store: %v", err)
	}

	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: 5,
		Lockout:     15 * time.Minute,
		Clock:       nowFn,
	})

	handlers := auth.NewHandlers(authenticator, sessionStore, limiter, "http://localhost:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", handlers.LoginHandler)

	badPayload := `{"username":"admin","password":"wrong-password"}`

	// 5 failed login attempts -> all 401
	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(badPayload))
		req.RemoteAddr = "192.168.1.100:12345"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}

	// 6th attempt with VALID credentials must return 429 Too Many Requests
	goodPayload := `{"username":"admin","password":"correct-password"}`
	req6 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(goodPayload))
	req6.RemoteAddr = "192.168.1.100:12345"
	req6.Header.Set("Content-Type", "application/json")
	rec6 := httptest.NewRecorder()
	mux.ServeHTTP(rec6, req6)

	if rec6.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 6th attempt, got %d", rec6.Code)
	}

	retryAfterHeader := rec6.Header().Get("Retry-After")
	if retryAfterHeader == "" {
		t.Error("expected Retry-After header on 429 response")
	}

	var errResp auth.ErrorResponse
	if err := json.NewDecoder(rec6.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != "rate_limited" {
		t.Errorf("expected error code 'rate_limited', got %q", errResp.Error.Code)
	}

	// Advance clock past 15 minutes lockout
	clockMu.Lock()
	currentClock = currentClock.Add(16 * time.Minute)
	clockMu.Unlock()

	// 7th attempt with valid credentials now succeeds -> 200 OK
	req7 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(goodPayload))
	req7.RemoteAddr = "192.168.1.100:12345"
	req7.Header.Set("Content-Type", "application/json")
	rec7 := httptest.NewRecorder()
	mux.ServeHTTP(rec7, req7)

	if rec7.Code != http.StatusOK {
		t.Errorf("expected 200 after lockout duration expired, got %d", rec7.Code)
	}

	// Ensure failure count was reset by checking that another failed attempt returns 401, not 429
	req8 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(badPayload))
	req8.RemoteAddr = "192.168.1.100:12345"
	req8.Header.Set("Content-Type", "application/json")
	rec8 := httptest.NewRecorder()
	mux.ServeHTTP(rec8, req8)

	if rec8.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 on single bad attempt after reset, got %d", rec8.Code)
	}
}

// 4. TestRateLimiter_MemoryCap:
// - Simulate 10,000 distinct failing IPs and verify the internal map length never exceeds 4096 entries.
func TestRateLimiter_MemoryCap(t *testing.T) {
	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: 5,
		Lockout:     15 * time.Minute,
		MaxTracked:  4096,
	})

	const totalIPs = 10000
	for i := 0; i < totalIPs; i++ {
		// Generate distinct IP: 10.x.y.z
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		limiter.RecordFailure(ip)

		// Check cap after each insertion
		if count := limiter.Len(); count > 4096 {
			t.Fatalf("rate limiter exceeded capacity %d at iteration %d: %d", 4096, i, count)
		}
	}

	finalCount := limiter.Len()
	if finalCount > 4096 {
		t.Errorf("expected final count <= 4096, got %d", finalCount)
	}
	if finalCount < 4000 {
		t.Errorf("expected near 4096 entries, got %d", finalCount)
	}
}

// 5. TestRequireAuth_Middleware:
// - Tests unauthenticated rejection and authenticated pass-through.
func TestRequireAuth_Middleware(t *testing.T) {
	store := setupTestStore(t)
	authenticator, secret := setupTestAuthenticator(t, "admin", "secret123")

	sessionStore, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
		TTL:      1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create session store: %v", err)
	}

	handlers := auth.NewHandlers(authenticator, sessionStore, auth.NewRateLimiter(auth.RateLimiterConfig{}), "")

	protectedHandler := handlers.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok || user != "admin" {
			http.Error(w, "missing context user", http.StatusInternalServerError)
			return
		}
		auth.WriteJSON(w, http.StatusOK, map[string]string{"protected": "ok"})
	}))

	// Request without cookie -> 401
	req1 := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rec1 := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without cookie, got %d", rec1.Code)
	}

	// Create valid cookie
	cookie, err := sessionStore.CreateSession(context.Background(), false)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Request with valid cookie -> 200
	req2 := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 with cookie, got %d", rec2.Code)
	}
}

// 6. TestClientIP_Extraction:
// - Tests IP parsing from RemoteAddr, X-Forwarded-For, and X-Real-IP.
func TestClientIP_Extraction(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		expected   string
	}{
		{
			name:       "remote addr with port",
			remoteAddr: "192.168.1.50:54321",
			headers:    nil,
			expected:   "192.168.1.50",
		},
		{
			name:       "x-forwarded-for single",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.195"},
			expected:   "203.0.113.195",
		},
		{
			name:       "x-forwarded-for multiple takes leftmost",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.195, 70.41.3.18, 150.172.238.178"},
			expected:   "203.0.113.195",
		},
		{
			name:       "x-real-ip fallback",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Real-IP": "198.51.100.1"},
			expected:   "198.51.100.1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			ip := auth.ExtractClientIP(req)
			if ip != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, ip)
			}
		})
	}
}

// 7. TestAuthenticator_StartupValidation:
// - Verifies cost checking and secret validation.
func TestAuthenticator_StartupValidation(t *testing.T) {
	// Missing hash
	_, err := auth.NewAuthenticator("admin", "", []byte("12345678901234567890123456789012"))
	if err == nil {
		t.Error("expected error for empty hash, got nil")
	}

	// Invalid bcrypt format
	_, err = auth.NewAuthenticator("admin", "not-a-bcrypt-hash", []byte("12345678901234567890123456789012"))
	if err == nil {
		t.Error("expected error for invalid bcrypt hash format, got nil")
	}

	// Valid hash with short secret
	validHash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	_, err = auth.NewAuthenticator("admin", string(validHash), []byte("too-short"))
	if err == nil {
		t.Error("expected error for short secret, got nil")
	}

	// Valid hash with nil secret generates random 32-byte secret
	authGen, err := auth.NewAuthenticator("admin", string(validHash), nil)
	if err != nil {
		t.Fatalf("expected nil secret to generate random secret, got: %v", err)
	}
	if len(authGen.SessionSecret()) < 32 {
		t.Errorf("expected generated secret >= 32 bytes, got %d", len(authGen.SessionSecret()))
	}
}

// 8. TestLoginHandler_MaxBytesReader:
// - Verifies that a payload > 4 KB is rejected with 400 Bad Request.
func TestLoginHandler_MaxBytesReader(t *testing.T) {
	store := setupTestStore(t)
	authenticator, secret := setupTestAuthenticator(t, "admin", "pwd")
	sessionStore, _ := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   secret,
	})
	handlers := auth.NewHandlers(authenticator, sessionStore, auth.NewRateLimiter(auth.RateLimiterConfig{}), "")

	// 5 KB payload
	largeJSON := fmt.Sprintf(`{"username":"admin","password":"%s"}`, strings.Repeat("A", 5000))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(largeJSON)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.LoginHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for payload > 4KB, got %d", rec.Code)
	}
}
