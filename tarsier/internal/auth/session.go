package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

const (
	// SessionCookieName is the standard name for the session cookie.
	SessionCookieName = "tarsier_session"

	// SessionIDEntropyBytes defines the raw entropy size for session IDs.
	SessionIDEntropyBytes = 24
)

var (
	// ErrInvalidCookie indicates that the cookie format, signature, or expiry is invalid.
	ErrInvalidCookie = errors.New("invalid or tampered session cookie")

	// ErrSessionExpired indicates that the session expiry time has passed.
	ErrSessionExpired = errors.New("session has expired")

	// ErrSessionNotFound indicates that the session is not found in config.db.
	ErrSessionNotFound = errors.New("session not found in store")
)

// SessionStore manages HMAC-signed session cookies backed by config.db's session table.
type SessionStore struct {
	configDB *db.Handle
	secret   []byte
	ttl      time.Duration
	clock    func() time.Time
}

// SessionConfig configures SessionStore.
type SessionConfig struct {
	ConfigDB *db.Handle
	Secret   []byte
	TTL      time.Duration
	Clock    func() time.Time
}

// NewSessionStore initializes a new SessionStore.
func NewSessionStore(cfg SessionConfig) (*SessionStore, error) {
	if cfg.ConfigDB == nil {
		return nil, errors.New("config.db handle is required")
	}
	if len(cfg.Secret) < MinSessionSecretBytes {
		return nil, fmt.Errorf("session secret must be at least %d bytes, got %d", MinSessionSecretBytes, len(cfg.Secret))
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	return &SessionStore{
		configDB: cfg.ConfigDB,
		secret:   cfg.Secret,
		ttl:      cfg.TTL,
		clock:    clock,
	}, nil
}

// SetClock sets a custom clock function (useful in tests).
func (s *SessionStore) SetClock(clock func() time.Time) {
	s.clock = clock
}

// CreateSession generates a new random session ID, records it in config.db,
// and returns a signed http.Cookie.
func (s *SessionStore) CreateSession(ctx context.Context, secure bool) (*http.Cookie, error) {
	// 24 random bytes -> base64url string
	rawID := make([]byte, SessionIDEntropyBytes)
	if _, err := rand.Read(rawID); err != nil {
		return nil, fmt.Errorf("generate session ID entropy: %w", err)
	}
	sid := base64.RawURLEncoding.EncodeToString(rawID)

	now := s.clock()
	expiresAt := now.Add(s.ttl)
	expUnix := expiresAt.Unix()

	// Insert into config.db session table synchronously
	err := s.configDB.ExecWrite(ctx, func(c context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(c, "INSERT INTO session (id, expires) VALUES (?, ?);", sid, expUnix)
		return execErr
	})
	if err != nil {
		return nil, fmt.Errorf("persist session in config.db: %w", err)
	}

	cookieVal := s.SignCookie(sid, expUnix)

	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    cookieVal,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(s.ttl.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}

	return cookie, nil
}

// SignCookie produces <sid>.<expiresUnix>.<hmacSignatureHex>
func (s *SessionStore) SignCookie(sid string, expUnix int64) string {
	payload := fmt.Sprintf("%s.%d", sid, expUnix)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", payload, sig)
}

// ValidateCookie parses and verifies the session cookie.
// 1. Validates HMAC signature and expiry locally without touching the database.
// 2. Checks existence and validity in config.db.
func (s *SessionStore) ValidateCookie(ctx context.Context, cookieValue string) (string, error) {
	parts := strings.Split(cookieValue, ".")
	if len(parts) != 3 {
		return "", ErrInvalidCookie
	}

	sid := parts[0]
	expStr := parts[1]
	providedSig := parts[2]

	if sid == "" || expStr == "" || providedSig == "" {
		return "", ErrInvalidCookie
	}

	// 1. Verify HMAC signature in constant time
	payload := sid + "." + expStr
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(providedSig), []byte(expectedSig)) != 1 {
		return "", ErrInvalidCookie
	}

	// Parse expiry
	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", ErrInvalidCookie
	}

	nowUnix := s.clock().Unix()
	if nowUnix >= expUnix {
		return "", ErrSessionExpired
	}

	// 2. Signature and expiry are valid; check existence in config.db read-only pool
	var dbExpires int64
	queryErr := s.configDB.QueryRO().QueryRowContext(ctx, "SELECT expires FROM session WHERE id = ?;", sid).Scan(&dbExpires)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if queryErr != nil {
		return "", fmt.Errorf("query session from config.db: %w", queryErr)
	}

	if nowUnix >= dbExpires {
		return "", ErrSessionExpired
	}

	return sid, nil
}

// DeleteSession purges the session row from config.db.
func (s *SessionStore) DeleteSession(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	return s.configDB.ExecWrite(ctx, func(c context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(c, "DELETE FROM session WHERE id = ?;", sid)
		return err
	})
}

// PruneExpiredSessions purges all expired sessions from config.db.
func (s *SessionStore) PruneExpiredSessions(ctx context.Context) error {
	nowUnix := s.clock().Unix()
	return s.configDB.ExecWrite(ctx, func(c context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(c, "DELETE FROM session WHERE expires <= ?;", nowUnix)
		return err
	})
}

// ExpiredCookie creates a response cookie that clears the session cookie in the browser.
func ExpiredCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}
