package alert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

// HeartbeatStatus represents the live operational state and telemetry of the heartbeat worker.
type HeartbeatStatus struct {
	Enabled        bool   `json:"enabled"`
	TargetHost     string `json:"target_host"` // Hostname only (path/query secret redacted)
	IntervalSec    int    `json:"interval_sec"`
	LastAttemptAt  int64  `json:"last_attempt_at"`
	LastSuccessAt  int64  `json:"last_success_at"`
	LastStatusCode int    `json:"last_status_code"`
	LastError      string `json:"last_error,omitempty"` // Sanitized error without secret URL token
	ConsecutiveErr int    `json:"consecutive_errors"`
}

// HeartbeatOption allows customizing HeartbeatWorker behavior.
type HeartbeatOption func(*HeartbeatWorker)

// WithHealthCheck sets a health check gate function. If fn() returns false, pings are suppressed.
func WithHealthCheck(fn func() bool) HeartbeatOption {
	return func(w *HeartbeatWorker) {
		w.healthCheck = fn
	}
}

// WithHeartbeatHTTPClient overrides the HTTP client used for pinging.
func WithHeartbeatHTTPClient(client *http.Client) HeartbeatOption {
	return func(w *HeartbeatWorker) {
		if client != nil {
			w.client = client
		}
	}
}

// WithHeartbeatVersion sets the version string in the User-Agent header.
func WithHeartbeatVersion(version string) HeartbeatOption {
	return func(w *HeartbeatWorker) {
		if version != "" {
			w.version = version
		}
	}
}

// WithHeartbeatInterval overrides the ping interval directly (useful for tests).
func WithHeartbeatInterval(d time.Duration) HeartbeatOption {
	return func(w *HeartbeatWorker) {
		if d > 0 {
			w.interval = d
			w.status.IntervalSec = int(d.Seconds())
		}
	}
}

// HeartbeatWorker periodically sends outbound HTTP GET pings to an external dead-man's-switch monitor.
type HeartbeatWorker struct {
	cfg         config.HeartbeatConfig
	client      *http.Client
	healthCheck func() bool
	version     string
	mu          sync.RWMutex
	interval    time.Duration
	timeout     time.Duration
	targetHost  string
	status      HeartbeatStatus
	reloadChan  chan struct{}
}

// NewHeartbeatWorker creates a new HeartbeatWorker from configuration.
func NewHeartbeatWorker(cfg config.HeartbeatConfig, opts ...HeartbeatOption) (*HeartbeatWorker, error) {
	w := &HeartbeatWorker{
		cfg:        cfg,
		version:    "1.0",
		reloadChan: make(chan struct{}, 1),
	}

	if err := w.applyConfig(cfg); err != nil {
		return nil, err
	}

	for _, opt := range opts {
		opt(w)
	}

	if w.client == nil {
		w.client = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:    2,
				IdleConnTimeout: 90 * time.Second,
			},
		}
	}

	return w, nil
}

func (w *HeartbeatWorker) applyConfig(cfg config.HeartbeatConfig) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.cfg = cfg
	w.interval = 60 * time.Second
	if cfg.Interval != "" {
		if d, err := time.ParseDuration(cfg.Interval); err == nil && d > 0 {
			w.interval = d
		}
	}
	// Enforce minimum interval of 10s if enabled
	if cfg.Enabled && w.interval < 10*time.Second {
		w.interval = 10 * time.Second
	}

	w.timeout = 10 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			w.timeout = d
		}
	}
	if w.timeout > w.interval/2 {
		w.timeout = w.interval / 2
	}
	if w.timeout < 1*time.Second {
		w.timeout = 1 * time.Second
	}

	w.targetHost = ""
	trimmedURL := strings.TrimSpace(cfg.URL)
	if cfg.Enabled {
		if trimmedURL == "" {
			return errors.New("heartbeat URL is required when enabled")
		}
		parsed, err := url.Parse(trimmedURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("invalid heartbeat URL %q; must be http or https with valid host", trimmedURL)
		}
		w.targetHost = parsed.Host
	}

	w.status.Enabled = cfg.Enabled
	w.status.TargetHost = w.targetHost
	w.status.IntervalSec = int(w.interval.Seconds())
	return nil
}

// Reload dynamically updates worker configuration without restarting.
func (w *HeartbeatWorker) Reload(newCfg config.HeartbeatConfig) error {
	if err := w.applyConfig(newCfg); err != nil {
		return err
	}

	select {
	case w.reloadChan <- struct{}{}:
	default:
	}
	return nil
}

// Status returns a thread-safe snapshot of the current heartbeat status.
func (w *HeartbeatWorker) Status() HeartbeatStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

// Run executes the heartbeat loop, performing an immediate startup ping followed by periodic ticks.
func (w *HeartbeatWorker) Run(ctx context.Context) {
	for {
		w.mu.RLock()
		enabled := w.cfg.Enabled
		interval := w.interval
		w.mu.RUnlock()

		if !enabled {
			select {
			case <-ctx.Done():
				return
			case <-w.reloadChan:
				continue
			}
		}

		// Initial immediate ping upon enabling / starting
		_ = w.PingOnce(ctx)

		ticker := time.NewTicker(interval)

	tickerLoop:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-w.reloadChan:
				ticker.Stop()
				break tickerLoop
			case <-ticker.C:
				_ = w.PingOnce(ctx)
			}
		}
	}
}

// PingOnce executes a single outbound HTTP GET ping.
func (w *HeartbeatWorker) PingOnce(ctx context.Context) error {
	w.mu.RLock()
	enabled := w.cfg.Enabled
	targetURL := strings.TrimSpace(w.cfg.URL)
	timeout := w.timeout
	targetHost := w.targetHost
	version := w.version
	healthCheck := w.healthCheck
	w.mu.RUnlock()

	if !enabled || targetURL == "" {
		return nil
	}

	nowSec := time.Now().UTC().Unix()

	// Check health gate if configured
	if healthCheck != nil && !healthCheck() {
		w.mu.Lock()
		w.status.LastAttemptAt = nowSec
		w.status.ConsecutiveErr++
		w.status.LastError = "health check failed; ping suppressed"
		w.status.LastStatusCode = 0
		w.mu.Unlock()
		return errors.New("health check failed; ping suppressed")
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		sanitized := w.sanitizeError(err, targetURL, targetHost)
		w.recordFailure(nowSec, 0, sanitized)
		return sanitized
	}

	req.Header.Set("User-Agent", fmt.Sprintf("Tarsier/%s", version))

	resp, err := w.client.Do(req)
	if err != nil {
		sanitized := w.sanitizeError(err, targetURL, targetHost)
		w.recordFailure(nowSec, 0, sanitized)
		return sanitized
	}
	defer resp.Body.Close()

	// Drain up to 4 KiB of response body
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.mu.Lock()
		w.status.LastAttemptAt = nowSec
		w.status.LastSuccessAt = nowSec
		w.status.LastStatusCode = resp.StatusCode
		w.status.LastError = ""
		w.status.ConsecutiveErr = 0
		w.mu.Unlock()
		return nil
	}

	failErr := fmt.Sprintf("non-2xx response from heartbeat monitor (HTTP %d)", resp.StatusCode)
	w.recordFailure(nowSec, resp.StatusCode, errors.New(failErr))
	return errors.New(failErr)
}

func (w *HeartbeatWorker) recordFailure(nowSec int64, statusCode int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.LastAttemptAt = nowSec
	w.status.LastStatusCode = statusCode
	w.status.ConsecutiveErr++
	if err != nil {
		w.status.LastError = err.Error()
	}
}

func (w *HeartbeatWorker) sanitizeError(err error, rawURL, host string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, host)
	}
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil && parsed.Path != "" {
		parts := strings.Split(parsed.Path, "/")
		for _, part := range parts {
			if len(part) >= 6 {
				msg = strings.ReplaceAll(msg, part, "[REDACTED]")
			}
		}
	}
	return errors.New(msg)
}
