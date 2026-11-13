package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
)

type contextKey string

const (
	// UserContextKey is the context key storing the authenticated username.
	UserContextKey contextKey = "tarsier.user"

	// SessionIDContextKey is the context key storing the validated session ID.
	SessionIDContextKey contextKey = "tarsier.session_id"
)

// ErrorResponse defines the standard JSON error envelope required across all Tarsier APIs.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail defines specific error codes and messages.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UserResponse defines the standard response for /me and /login endpoints.
type UserResponse struct {
	Username string `json:"username"`
}

// StatusResponse defines standard operational status responses.
type StatusResponse struct {
	Status string `json:"status"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// WriteJSON writes a JSON response with status code.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// WriteJSONError writes a standard {"error": {"code": "...", "message": "..."}} JSON response.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// ExtractClientIP extracts the client IP address from r.RemoteAddr or trusted proxy headers.
func ExtractClientIP(r *http.Request) string {
	// Check X-Forwarded-For if present (first/leftmost IP represents original client)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if parsed := net.ParseIP(ip); parsed != nil {
				return ip
			}
		}
	}

	// Check X-Real-IP
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if parsed := net.ParseIP(xri); parsed != nil {
			return xri
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}

	return r.RemoteAddr
}

// IsSecureRequest checks if the request is served over HTTPS or behind a TLS-terminating proxy.
func IsSecureRequest(r *http.Request, baseURL string) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(baseURL)), "https://") {
		return true
	}
	return false
}

// UserFromContext retrieves the authenticated username from ctx, if present.
func UserFromContext(ctx context.Context) (string, bool) {
	user, ok := ctx.Value(UserContextKey).(string)
	return user, ok
}

// SessionIDFromContext retrieves the current session ID from ctx, if present.
func SessionIDFromContext(ctx context.Context) (string, bool) {
	sid, ok := ctx.Value(SessionIDContextKey).(string)
	return sid, ok
}

// Handlers bundles the HTTP handlers and middleware for auth.
type Handlers struct {
	auth     *Authenticator
	sessions *SessionStore
	limiter  *RateLimiter
	baseURL  string
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(auth *Authenticator, sessions *SessionStore, limiter *RateLimiter, baseURL string) *Handlers {
	return &Handlers{
		auth:     auth,
		sessions: sessions,
		limiter:  limiter,
		baseURL:  baseURL,
	}
}

// LoginHandler handles POST /api/v1/auth/login.
func (h *Handlers) LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	clientIP := ExtractClientIP(r)

	// 1. IP Rate limit check
	if allowed, retryAfter := h.limiter.Check(clientIP); !allowed {
		secs := int(math.Ceil(retryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		WriteJSONError(w, http.StatusTooManyRequests, "rate_limited", "too many failed login attempts, please try again later")
		return
	}

	// 2. Enforce 4 KB max body size
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed request payload")
		return
	}

	// 3. Verify credentials
	if !h.auth.Authenticate(req.Username, req.Password) {
		h.limiter.RecordFailure(clientIP)
		WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid username or password")
		return
	}

	// 4. Success: clear rate limiter state for IP
	h.limiter.RecordSuccess(clientIP)

	// 5. Create session in DB and set cookie
	isSecure := IsSecureRequest(r, h.baseURL)
	cookie, err := h.sessions.CreateSession(r.Context(), isSecure)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create session")
		return
	}

	http.SetCookie(w, cookie)
	WriteJSON(w, http.StatusOK, UserResponse{Username: h.auth.AdminUser()})
}

// LogoutHandler handles POST /api/v1/auth/logout.
func (h *Handlers) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	cookie, err := r.Cookie(SessionCookieName)
	if err == nil && cookie != nil && cookie.Value != "" {
		// Attempt to extract session ID and delete row
		parts := strings.Split(cookie.Value, ".")
		if len(parts) >= 1 && parts[0] != "" {
			_ = h.sessions.DeleteSession(r.Context(), parts[0])
		}
	}

	isSecure := IsSecureRequest(r, h.baseURL)
	http.SetCookie(w, ExpiredCookie(isSecure))
	WriteJSON(w, http.StatusOK, StatusResponse{Status: "ok"})
}

// MeHandler handles GET /api/v1/auth/me.
func (h *Handlers) MeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie == nil || cookie.Value == "" {
		WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "missing session cookie")
		return
	}

	_, valErr := h.sessions.ValidateCookie(r.Context(), cookie.Value)
	if valErr != nil {
		WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
		return
	}

	WriteJSON(w, http.StatusOK, UserResponse{Username: h.auth.AdminUser()})
}

// RequireAuth middleware protects routes by requiring a valid session cookie.
func (h *Handlers) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil || cookie == nil || cookie.Value == "" {
			WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		sid, valErr := h.sessions.ValidateCookie(r.Context(), cookie.Value)
		if valErr != nil {
			if errors.Is(valErr, ErrSessionExpired) {
				WriteJSONError(w, http.StatusUnauthorized, "session_expired", "session expired")
				return
			}
			WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid session")
			return
		}

		// Inject user and session ID into context
		ctx := context.WithValue(r.Context(), UserContextKey, h.auth.AdminUser())
		ctx = context.WithValue(ctx, SessionIDContextKey, sid)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
