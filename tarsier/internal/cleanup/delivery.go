package cleanup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

var (
	// ErrPermanentConfig indicates a permanent configuration error where retry would be futile.
	ErrPermanentConfig = errors.New("permanent notifier configuration error")
)

// ReportPayload encapsulates a rendered report and its metadata for notification dispatch.
type ReportPayload struct {
	Report       PreCleanupReport
	Format       string // "markdown" | "html"
	Filename     string // e.g., "tarsier-precleanup-2026-11-15.md" or ".html"
	BodyText     string // Rendered Markdown or HTML content
	Attachment   []byte // Rendered report bytes (already capped)
	SummaryTitle string // Compact summary for Discord embed / Email subject
	SummaryBody  string // Compact 4-6 line key stats summary
}

// ChannelDeliveryOutcome tracks the delivery result for a single channel.
type ChannelDeliveryOutcome struct {
	Channel   string `json:"channel"`
	Attempts  int    `json:"attempts"`
	Succeeded bool   `json:"succeeded"`
	LastError string `json:"last_error,omitempty"`
}

// DeliverySummary summarizes delivery attempts across all configured channels.
type DeliverySummary struct {
	OverallStatus string                   `json:"overall_status"` // "sent" | "partial" | "failed" | "skipped"
	Outcomes      []ChannelDeliveryOutcome `json:"outcomes"`
	ShouldProceed bool                     `json:"should_proceed"`
	WarningActive bool                     `json:"warning_active"`
}

// ReportSender defines an interface for delivering reports with attachments.
type ReportSender interface {
	Name() string
	SendReport(ctx context.Context, payload ReportPayload) error
}

// DelivererOption configures ReportDeliverer.
type DelivererOption func(*ReportDeliverer)

// WithDelivererSleeper injects a custom sleeper function for test backoff acceleration.
func WithDelivererSleeper(sleeper func(ctx context.Context, d time.Duration) error) DelivererOption {
	return func(d *ReportDeliverer) {
		d.sleeper = sleeper
	}
}

// ReportDeliverer orchestrates report delivery, retries, and proceed/block decisions.
type ReportDeliverer struct {
	cfg           config.CleanupReportConfig
	senders       map[string]ReportSender
	warningActive atomic.Bool
	sleeper       func(ctx context.Context, d time.Duration) error
}

// NewReportDeliverer creates an initialized ReportDeliverer.
func NewReportDeliverer(cfg config.CleanupReportConfig, senders []ReportSender, opts ...DelivererOption) *ReportDeliverer {
	sendersMap := make(map[string]ReportSender, len(senders))
	for _, s := range senders {
		sendersMap[s.Name()] = s
	}

	d := &ReportDeliverer{
		cfg:     cfg,
		senders: sendersMap,
		sleeper: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// IsWarningActive returns whether the delivery warning banner should be active.
func (d *ReportDeliverer) IsWarningActive() bool {
	return d.warningActive.Load()
}

// SetWarningActive sets the active state of the delivery warning banner.
func (d *ReportDeliverer) SetWarningActive(active bool) {
	d.warningActive.Store(active)
}

// Deliver sends the report payload to all configured channels with retries and policy enforcement.
func (d *ReportDeliverer) Deliver(ctx context.Context, payload ReportPayload, isEmergency bool) (DeliverySummary, error) {
	enabled := true
	if d.cfg.Enabled != nil {
		enabled = *d.cfg.Enabled
	}

	if !enabled || len(d.cfg.Channels) == 0 {
		log.Printf("[cleanup] pre-cleanup report delivery skipped (enabled=%v, channels=%d)", enabled, len(d.cfg.Channels))
		return DeliverySummary{
			OverallStatus: "skipped",
			ShouldProceed: true,
			WarningActive: d.warningActive.Load(),
		}, nil
	}

	log.Printf("[cleanup] delivering pre-cleanup report to %d channel(s): %v (emergency=%v)", len(d.cfg.Channels), d.cfg.Channels, isEmergency)

	minAttempts := d.cfg.MinAttempts
	if minAttempts < 2 {
		minAttempts = 2
	}

	retryDelay := 10 * time.Second
	if d.cfg.RetryDelay != "" {
		if parsed, err := time.ParseDuration(d.cfg.RetryDelay); err == nil && parsed > 0 {
			retryDelay = parsed
		}
	}

	summary := DeliverySummary{}
	succeededCount := 0

	for _, ch := range d.cfg.Channels {
		outcome := ChannelDeliveryOutcome{
			Channel: ch,
		}

		sender, exists := d.senders[ch]
		if !exists {
			outcome.Attempts = 1
			outcome.Succeeded = false
			outcome.LastError = fmt.Sprintf("notifier channel %q not configured", ch)
			log.Printf("[cleanup] channel %q has no sender configured", ch)
			summary.Outcomes = append(summary.Outcomes, outcome)
			continue
		}

		for attempt := 1; attempt <= minAttempts; attempt++ {
			if ctx.Err() != nil {
				outcome.LastError = ctx.Err().Error()
				log.Printf("[cleanup] [%s] context cancelled: %v", ch, ctx.Err())
				break
			}

			outcome.Attempts = attempt
			log.Printf("[cleanup] [%s] dispatching pre-cleanup report (attempt %d/%d)...", ch, attempt, minAttempts)

			attemptCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			err := sender.SendReport(attemptCtx, payload)
			cancel()

			if err == nil {
				outcome.Succeeded = true
				outcome.LastError = ""
				succeededCount++
				log.Printf("[cleanup] [%s] report delivered successfully on attempt %d", ch, attempt)
				break
			}

			outcome.LastError = RedactSecrets(err.Error())
			log.Printf("[cleanup] [%s] attempt %d/%d failed: %s", ch, attempt, minAttempts, outcome.LastError)

			if errors.Is(err, ErrPermanentConfig) {
				log.Printf("[cleanup] [%s] permanent configuration error encountered; skipping retries", ch)
				break
			}

			if attempt < minAttempts {
				log.Printf("[cleanup] [%s] sleeping %v before retry...", ch, retryDelay)
				if sErr := d.sleeper(ctx, retryDelay); sErr != nil {
					outcome.LastError = sErr.Error()
					break
				}
			}
		}

		summary.Outcomes = append(summary.Outcomes, outcome)
	}

	totalChannels := len(d.cfg.Channels)
	if succeededCount == totalChannels {
		summary.OverallStatus = "sent"
		summary.WarningActive = false
		summary.ShouldProceed = true
		d.warningActive.Store(false)
		log.Printf("[cleanup] pre-cleanup report successfully delivered to all %d channel(s)", totalChannels)
		return summary, nil
	}

	if succeededCount > 0 {
		summary.OverallStatus = "partial"
	} else {
		summary.OverallStatus = "failed"
	}

	summary.WarningActive = true
	d.warningActive.Store(true)

	if isEmergency {
		summary.ShouldProceed = true
	} else if strings.EqualFold(d.cfg.OnFailure, "block") {
		summary.ShouldProceed = false
	} else {
		// Default "proceed"
		summary.ShouldProceed = true
	}

	log.Printf("[cleanup] report delivery finished: overall_status=%s, succeeded=%d/%d, should_proceed=%v",
		summary.OverallStatus, succeededCount, totalChannels, summary.ShouldProceed)

	return summary, nil
}

// RedactSecrets masks sensitive tokens from error and log strings.
func RedactSecrets(msg string) string {
	// Redact Discord webhooks: https://discord.com/api/webhooks/<id>/<token>
	if idx := strings.Index(msg, "api/webhooks/"); idx != -1 {
		parts := strings.Split(msg[idx:], "/")
		if len(parts) >= 4 {
			secretToken := strings.Fields(parts[3])[0]
			msg = strings.ReplaceAll(msg, secretToken, "***")
		} else if len(parts) >= 3 {
			secretToken := strings.Fields(parts[2])[0]
			msg = strings.ReplaceAll(msg, secretToken, "***")
		}
	}
	// Redact embedded user:password@
	if atIdx := strings.Index(msg, "@"); atIdx != -1 {
		colonIdx := strings.LastIndex(msg[:atIdx], ":")
		slashIdx := strings.LastIndex(msg[:atIdx], "//")
		if colonIdx != -1 && colonIdx > slashIdx {
			secret := msg[colonIdx+1 : atIdx]
			if len(secret) > 0 {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
		}
	}
	return msg
}

func redactUsername(u string) string {
	u = strings.TrimSpace(u)
	if len(u) <= 2 {
		return "***"
	}
	return u[:2] + "***"
}

// --- Discord Report Sender ---

// DiscordReportSender sends rich pre-cleanup report embeds and attachments to Discord.
type DiscordReportSender struct {
	cfg    config.DiscordNotifierConfig
	client *http.Client
}

// NewDiscordReportSender creates an initialized DiscordReportSender.
func NewDiscordReportSender(cfg config.DiscordNotifierConfig, client *http.Client) *DiscordReportSender {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &DiscordReportSender{
		cfg:    cfg,
		client: client,
	}
}

func (s *DiscordReportSender) Name() string {
	return "discord"
}

func (s *DiscordReportSender) SendReport(ctx context.Context, payload ReportPayload) error {
	rawURL := strings.TrimSpace(s.cfg.WebhookURL)
	if rawURL == "" {
		return fmt.Errorf("%w: discord webhook_url is empty", ErrPermanentConfig)
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("%w: invalid discord webhook_url: %v", ErrPermanentConfig, err)
	}

	start := time.Now()
	log.Printf("[cleanup] [discord] dispatching pre-cleanup report (payload=%d bytes)", len(payload.Attachment))

	// Build multipart request body
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// 1. Embed JSON payload
	title := payload.SummaryTitle
	if title == "" {
		title = "Tarsier Pre-Cleanup Report"
	}

	embed := map[string]any{
		"title":       title,
		"description": payload.SummaryBody,
		"color":       3447003, // Blue
		"fields": []map[string]any{
			{"name": "Logs to Purge", "value": fmt.Sprintf("%d lines", payload.Report.TotalLogsPurged), "inline": true},
			{"name": "Reclaimed Space", "value": FormatBytes(payload.Report.TotalBytesReclaim), "inline": true},
			{"name": "Errors / Warnings", "value": fmt.Sprintf("%d / %d", payload.Report.TotalErrors, payload.Report.TotalWarnings), "inline": true},
			{"name": "Alerts Fired", "value": fmt.Sprintf("%d", payload.Report.AlertsFired), "inline": true},
		},
		"timestamp": payload.Report.GeneratedAt.Format(time.RFC3339),
	}

	content := ""
	if s.cfg.Mention != "" {
		content = s.cfg.Mention + " "
	}

	payloadJSON, err := json.Marshal(map[string]any{
		"content": content,
		"embeds":  []any{embed},
	})
	if err != nil {
		return err
	}

	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return err
	}

	// 2. Report file attachment
	filename := payload.Filename
	if filename == "" {
		if payload.Format == "html" {
			filename = "tarsier-precleanup.html"
		} else {
			filename = "tarsier-precleanup.md"
		}
	}

	contentType := "text/markdown"
	if payload.Format == "html" {
		contentType = "text/html"
	}

	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[0]"; filename="%s"`, filename))
	partHeader.Set("Content-Type", contentType)

	filePart, err := writer.CreatePart(partHeader)
	if err != nil {
		return err
	}

	attachmentData := payload.Attachment
	if len(attachmentData) == 0 && payload.BodyText != "" {
		attachmentData = []byte(payload.BodyText)
	}

	if _, err := filePart.Write(attachmentData); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		err = fmt.Errorf("discord post error: %w", err)
		log.Printf("[cleanup] [discord] failed: %s", RedactSecrets(err.Error()))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		err = fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
		log.Printf("[cleanup] [discord] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		err = fmt.Errorf("discord webhook returned HTTP %d: %s", resp.StatusCode, string(respBytes))
		log.Printf("[cleanup] [discord] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	log.Printf("[cleanup] [discord] pre-cleanup report successfully delivered in %v", time.Since(start))
	return nil
}

// --- SMTP Report Sender ---

// SMTPOption allows customizing SMTPReportSender for testing.
type SMTPOption func(*SMTPReportSender)

// WithCustomDialer provides an in-memory/mock TCP dialer for SMTP tests.
func WithCustomDialer(dialer func(ctx context.Context, network, addr string) (net.Conn, error)) SMTPOption {
	return func(s *SMTPReportSender) {
		s.customDialer = dialer
	}
}

// WithCustomTLS provides custom TLS configuration for SMTP tests.
func WithCustomTLS(tlsConfig *tls.Config) SMTPOption {
	return func(s *SMTPReportSender) {
		s.customTLS = tlsConfig
	}
}

// SMTPReportSender sends rich multipart/mixed emails with report attachments.
type SMTPReportSender struct {
	cfg          config.EmailNotifierConfig
	customDialer func(ctx context.Context, network, addr string) (net.Conn, error)
	customTLS    *tls.Config
}

// NewSMTPReportSender creates an initialized SMTPReportSender.
func NewSMTPReportSender(cfg config.EmailNotifierConfig, opts ...SMTPOption) *SMTPReportSender {
	s := &SMTPReportSender{
		cfg: cfg,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *SMTPReportSender) Name() string {
	return "email"
}

// ParseRecipients normalizes and splits email recipient entries, supporting
// comma-separated, semicolon-separated, angle-bracketed, and display-name addresses.
func ParseRecipients(to []string) []string {
	var recipients []string
	seen := make(map[string]bool)

	for _, entry := range to {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Try parsing as standard mail address list (handles commas, display names, angle brackets)
		if addrs, err := mail.ParseAddressList(entry); err == nil && len(addrs) > 0 {
			for _, a := range addrs {
				clean := strings.TrimSpace(a.Address)
				if clean != "" && !seen[clean] {
					seen[clean] = true
					recipients = append(recipients, clean)
				}
			}
			continue
		}

		// Fallback: split by commas and semicolons
		parts := strings.FieldsFunc(entry, func(r rune) bool {
			return r == ',' || r == ';'
		})
		for _, p := range parts {
			clean := strings.Trim(strings.TrimSpace(p), "<>\"' ")
			if clean != "" && !seen[clean] {
				seen[clean] = true
				recipients = append(recipients, clean)
			}
		}
	}
	return recipients
}

func (s *SMTPReportSender) SendReport(ctx context.Context, payload ReportPayload) error {
	host := strings.TrimSpace(s.cfg.SMTPHost)
	if host == "" {
		return fmt.Errorf("%w: smtp_host is empty", ErrPermanentConfig)
	}

	recipients := ParseRecipients(s.cfg.To)
	if len(recipients) == 0 {
		return fmt.Errorf("%w: email to recipients list is empty", ErrPermanentConfig)
	}

	secMode := strings.ToLower(strings.TrimSpace(s.cfg.Security))
	if secMode == "" {
		secMode = "starttls"
	}

	port := s.cfg.SMTPPort
	if port <= 0 {
		switch secMode {
		case "tls":
			port = 465
		case "none":
			port = 25
		default:
			port = 587
		}
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	start := time.Now()
	log.Printf("[cleanup] [email] connecting to SMTP server %s (security=%s, recipients=%d)", addr, secMode, len(recipients))

	// Build multipart/mixed message
	var msg bytes.Buffer
	boundary := fmt.Sprintf("tarsier_rel_%d", time.Now().UnixNano())

	subject := payload.SummaryTitle
	if subject == "" {
		subject = "Tarsier Pre-Cleanup Report"
	}

	from := s.cfg.From
	if from == "" {
		from = "tarsier@localhost"
	}

	msg.WriteString(fmt.Sprintf("From: %s\r\n", from))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(recipients, ", ")))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", boundary))

	// Part 1: Body
	bodyContentType := "text/plain; charset=utf-8"
	if payload.Format == "html" {
		bodyContentType = "text/html; charset=utf-8"
	}

	msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	msg.WriteString(fmt.Sprintf("Content-Type: %s\r\n", bodyContentType))
	msg.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(payload.BodyText)
	msg.WriteString("\r\n\r\n")

	// Part 2: Attachment
	filename := payload.Filename
	if filename == "" {
		if payload.Format == "html" {
			filename = "tarsier-precleanup.html"
		} else {
			filename = "tarsier-precleanup.md"
		}
	}

	attachmentData := payload.Attachment
	if len(attachmentData) == 0 && payload.BodyText != "" {
		attachmentData = []byte(payload.BodyText)
	}

	msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	msg.WriteString(fmt.Sprintf("Content-Type: text/plain; charset=utf-8; name=\"%s\"\r\n", filename))
	msg.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n", filename))
	msg.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

	// Base64 encode wrapped in 76-character lines
	encoded := base64.StdEncoding.EncodeToString(attachmentData)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		msg.WriteString(encoded[i:end] + "\r\n")
	}
	msg.WriteString(fmt.Sprintf("\r\n--%s--\r\n", boundary))

	// Dial SMTP
	var conn net.Conn
	var err error
	if s.customDialer != nil {
		conn, err = s.customDialer(ctx, "tcp", addr)
	} else {
		dialer := net.Dialer{Timeout: 10 * time.Second}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		err = fmt.Errorf("dial smtp %s: %w", addr, err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}
	defer conn.Close()

	// Enforce socket deadline to protect against hanging servers
	deadline := time.Now().Add(25 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	tlsConf := s.customTLS
	if tlsConf == nil {
		tlsConf = &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}
	}

	if secMode == "tls" {
		tlsConn := tls.Client(conn, tlsConf)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			err = fmt.Errorf("tls handshake %s: %w", addr, err)
			log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
			return err
		}
		conn = tlsConn
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		err = fmt.Errorf("smtp client error %s: %w", addr, err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}
	defer c.Close()

	if secMode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				err = fmt.Errorf("starttls error %s: %w", addr, err)
				log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
				return err
			}
		} else {
			err = fmt.Errorf("smtp server %s does not advertise STARTTLS extension", addr)
			log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
			return err
		}
	}

	if s.cfg.Username != "" || s.cfg.Password != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, host)
		if err := c.Auth(auth); err != nil {
			err = fmt.Errorf("smtp auth failed: %w", err)
			log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
			return err
		}
		log.Printf("[cleanup] [email] connected to %s, authenticated as %s", addr, redactUsername(s.cfg.Username))
	} else {
		log.Printf("[cleanup] [email] connected to %s (anonymous)", addr)
	}

	if err := c.Mail(from); err != nil {
		err = fmt.Errorf("smtp mail from error: %w", err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	for _, recipient := range recipients {
		if err := c.Rcpt(recipient); err != nil {
			err = fmt.Errorf("smtp rcpt to %s error: %w", recipient, err)
			log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
			return err
		}
	}

	w, err := c.Data()
	if err != nil {
		err = fmt.Errorf("smtp data command error: %w", err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	if _, err := w.Write(msg.Bytes()); err != nil {
		_ = w.Close()
		err = fmt.Errorf("smtp write error: %w", err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	if err := w.Close(); err != nil {
		err = fmt.Errorf("smtp close error: %w", err)
		log.Printf("[cleanup] [email] failed: %s", RedactSecrets(err.Error()))
		return err
	}

	_ = c.Quit()

	log.Printf("[cleanup] [email] pre-cleanup report successfully delivered to %d recipient(s) in %v", len(recipients), time.Since(start))
	return nil
}
