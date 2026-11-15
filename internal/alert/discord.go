package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

// Discord embed colors in decimal representation
const (
	ColorCritical = 15158332 // Red
	ColorWarning  = 15105570 // Amber
	ColorInfo     = 3447003  // Blue
	ColorResolved = 3066993  // Green
)

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Timestamp   string              `json:"timestamp"`
}

type discordPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds"`
}

// DiscordNotifier implements Notifier for Discord incoming webhooks.
type DiscordNotifier struct {
	cfg    config.DiscordNotifierConfig
	client *http.Client
}

// DiscordOption allows customizing DiscordNotifier (e.g. for testing).
type DiscordOption func(*DiscordNotifier)

// WithDiscordHTTPClient overrides the HTTP client used for Discord webhook calls.
func WithDiscordHTTPClient(client *http.Client) DiscordOption {
	return func(d *DiscordNotifier) {
		if client != nil {
			d.client = client
		}
	}
}

// NewDiscordNotifier creates a Discord webhook notifier from configuration.
func NewDiscordNotifier(cfg config.DiscordNotifierConfig, opts ...DiscordOption) *DiscordNotifier {
	d := &DiscordNotifier{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Name returns the identifier for this notifier.
func (d *DiscordNotifier) Name() string {
	return "discord"
}

// Send formats and dispatches a notification payload to the Discord webhook.
func (d *DiscordNotifier) Send(ctx context.Context, n Notification) error {
	webhookURL := strings.TrimSpace(d.cfg.WebhookURL)
	if webhookURL == "" {
		return PermanentError{Err: errors.New("discord webhook URL is not configured")}
	}

	payload := d.buildPayload(n)
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return d.sanitizeError(fmt.Errorf("create discord request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Tarsier-Notifier/1.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return d.sanitizeError(fmt.Errorf("send discord webhook: %w", err))
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	// Success cases: HTTP 200 - 204
	if resp.StatusCode >= 200 && resp.StatusCode <= 204 {
		return nil
	}

	// Rate limited: HTTP 429
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfterSec := 5 * time.Second
		if val := resp.Header.Get("Retry-After"); val != "" {
			if sec, parseErr := strconv.ParseFloat(val, 64); parseErr == nil && sec > 0 {
				retryAfterSec = time.Duration(sec * float64(time.Second))
			}
		}
		if retryAfterSec > 30*time.Second {
			retryAfterSec = 30 * time.Second
		}
		return RateLimitError{
			RetryAfter: retryAfterSec,
			Err:        errors.New(fmt.Sprintf("discord rate limited (HTTP 429): %s", strings.TrimSpace(string(respBody)))),
		}
	}

	// Permanent configuration errors: 401, 403, 404
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return PermanentError{
			Err: d.sanitizeError(fmt.Errorf("discord permanent error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))),
		}
	}

	// Transient server error: 5xx, etc.
	return d.sanitizeError(fmt.Errorf("discord server error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
}

func (d *DiscordNotifier) buildPayload(n Notification) discordPayload {
	var content string
	isFiring := strings.EqualFold(n.State, "firing")
	isCritical := strings.EqualFold(n.Severity, "critical")

	if isFiring && isCritical && strings.TrimSpace(d.cfg.Mention) != "" {
		content = strings.TrimSpace(d.cfg.Mention)
	}

	// Determine embed color
	var color int
	if !isFiring {
		color = ColorResolved
	} else {
		switch strings.ToLower(n.Severity) {
		case "critical":
			color = ColorCritical
		case "warning":
			color = ColorWarning
		default:
			color = ColorInfo
		}
	}

	// Determine embed title
	titleState := "FIRING"
	if !isFiring {
		titleState = "RESOLVED"
	}
	title := fmt.Sprintf("[%s] %s", titleState, n.Rule)

	// Build fields: Target, Severity, then sorted details
	fields := []discordEmbedField{
		{Name: "Target", Value: n.Target, Inline: true},
		{Name: "Severity", Value: strings.ToUpper(n.Severity), Inline: true},
	}

	if len(n.Details) > 0 {
		keys := make([]string, 0, len(n.Details))
		for k := range n.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fields = append(fields, discordEmbedField{
				Name:   k,
				Value:  n.Details[k],
				Inline: true,
			})
		}
	}

	ts := n.At.UTC().Format(time.RFC3339)
	if n.At.IsZero() {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	return discordPayload{
		Content: content,
		Embeds: []discordEmbed{
			{
				Title:       title,
				Description: n.Summary,
				Color:       color,
				Fields:      fields,
				Timestamp:   ts,
			},
		},
	}
}

// sanitizeError removes raw webhook URLs and secret query tokens from error messages.
func (d *DiscordNotifier) sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	rawMsg := err.Error()

	target := strings.TrimSpace(d.cfg.WebhookURL)
	if target != "" {
		rawMsg = strings.ReplaceAll(rawMsg, target, "[REDACTED_WEBHOOK_URL]")
		if parsed, parseErr := url.Parse(target); parseErr == nil && parsed.Path != "" {
			parts := strings.Split(parsed.Path, "/")
			if len(parts) >= 2 {
				// Redact token segment if present in message
				token := parts[len(parts)-1]
				if len(token) > 5 {
					rawMsg = strings.ReplaceAll(rawMsg, token, "[REDACTED]")
				}
			}
		}
	}

	return errors.New(rawMsg)
}
