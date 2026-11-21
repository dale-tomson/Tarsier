package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultAdminUser is used when TARSIER_ADMIN_USER is unset.
	DefaultAdminUser = "admin"

	// MinSessionSecretBytes defines the minimum key length for HMAC-SHA256 session cookie signing.
	MinSessionSecretBytes = 32
)

// Authenticator provides single-admin authentication with constant-time checks
// and timing-attack mitigation.
type Authenticator struct {
	adminUser     string
	adminHash     []byte
	sessionSecret []byte
	dummyHash     []byte
}

// NewAuthenticator creates and validates an Authenticator with the provided credentials and session secret.
func NewAuthenticator(username, passwordHash string, sessionSecret []byte) (*Authenticator, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		username = DefaultAdminUser
	}

	hashBytes := []byte(strings.TrimSpace(passwordHash))
	if len(hashBytes) == 0 {
		return nil, errors.New("admin password hash is required (TARSIER_ADMIN_PASSWORD_HASH)")
	}

	cost, err := bcrypt.Cost(hashBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid bcrypt password hash format: %w", err)
	}

	// Session secret validation or generation
	secret := sessionSecret
	if len(secret) == 0 {
		generated := make([]byte, MinSessionSecretBytes)
		if _, err := rand.Read(generated); err != nil {
			return nil, fmt.Errorf("failed to generate random session secret: %w", err)
		}
		secret = generated
		log.Println("WARNING: TARSIER_SESSION_SECRET is unset; generated ephemeral 32-byte secret. Sessions will reset on restart.")
	} else if len(secret) < MinSessionSecretBytes {
		return nil, fmt.Errorf("TARSIER_SESSION_SECRET must be at least %d bytes, got %d", MinSessionSecretBytes, len(secret))
	}

	// Generate a dummy hash matching the target hash cost to eliminate timing differentials
	// between existing and non-existing username login attempts.
	dummyPassword := []byte("tarsier-timing-dummy-salt")
	dummyHash, err := bcrypt.GenerateFromPassword(dummyPassword, cost)
	if err != nil {
		// Fallback to min cost if cost was outside valid range
		dummyHash, _ = bcrypt.GenerateFromPassword(dummyPassword, bcrypt.DefaultCost)
	}

	return &Authenticator{
		adminUser:     username,
		adminHash:     hashBytes,
		sessionSecret: secret,
		dummyHash:     dummyHash,
	}, nil
}

// NewAuthenticatorFromEnv initializes Authenticator using TARSIER_ADMIN_USER,
// TARSIER_ADMIN_PASSWORD_HASH, and TARSIER_SESSION_SECRET.
func NewAuthenticatorFromEnv() (*Authenticator, error) {
	user := os.Getenv("TARSIER_ADMIN_USER")
	hash := os.Getenv("TARSIER_ADMIN_PASSWORD_HASH")
	secretStr := os.Getenv("TARSIER_SESSION_SECRET")

	var secret []byte
	if secretStr != "" {
		secret = []byte(secretStr)
	}

	return NewAuthenticator(user, hash, secret)
}

// Authenticate verifies the username and password in constant time without revealing username validity.
func (a *Authenticator) Authenticate(username, password string) bool {
	// Compare usernames in constant time
	userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(a.adminUser)) == 1

	pwdBytes := []byte(password)
	if userMatch {
		// Run bcrypt against the actual admin password hash
		err := bcrypt.CompareHashAndPassword(a.adminHash, pwdBytes)
		return err == nil
	}

	// Timing attack mitigation: run dummy bcrypt computation so execution time is indistinguishable
	_ = bcrypt.CompareHashAndPassword(a.dummyHash, pwdBytes)
	return false
}

// SessionSecret returns the verified HMAC session signing key.
func (a *Authenticator) SessionSecret() []byte {
	return a.sessionSecret
}

// AdminUser returns the configured admin username.
func (a *Authenticator) AdminUser() string {
	return a.adminUser
}
