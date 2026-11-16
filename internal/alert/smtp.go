package alert

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

// SMTPOption allows customizing SMTPNotifier (e.g. for testing).
type SMTPOption func(*SMTPNotifier)

// WithSMTPDialer allows supplying a custom dialer function for testing.
func WithSMTPDialer(dialer func(ctx context.Context, network, addr string) (net.Conn, error)) SMTPOption {
	return func(s *SMTPNotifier) {
		s.customDialer = dialer
	}
}

// WithSMTPTLSConfig allows overriding the TLS configuration (e.g. InsecureSkipVerify for mock servers).
func WithSMTPTLSConfig(tlsConfig *tls.Config) SMTPOption {
	return func(s *SMTPNotifier) {
		s.customTLS = tlsConfig
	}
}

// SMTPNotifier sends alert notifications via SMTP email.
type SMTPNotifier struct {
	cfg          config.EmailNotifierConfig
	customDialer func(ctx context.Context, network, addr string) (net.Conn, error)
	customTLS    *tls.Config
}

// NewSMTPNotifier creates an SMTPNotifier from configuration.
func NewSMTPNotifier(cfg config.EmailNotifierConfig, opts ...SMTPOption) *SMTPNotifier {
	s := &SMTPNotifier{
		cfg: cfg,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name returns the identifier for this notifier.
func (s *SMTPNotifier) Name() string {
	return "email"
}

// Send builds and sends the RFC 5322 email message over SMTP.
func (s *SMTPNotifier) Send(ctx context.Context, n Notification) error {
	host := strings.TrimSpace(s.cfg.SMTPHost)
	if host == "" {
		return PermanentError{Err: errors.New("smtp_host is not configured")}
	}

	from := strings.TrimSpace(s.cfg.From)
	if from == "" {
		return PermanentError{Err: errors.New("email from address is not configured")}
	}

	var recipients []string
	seen := make(map[string]bool)
	for _, to := range s.cfg.To {
		trimmed := strings.TrimSpace(to)
		if trimmed == "" {
			continue
		}
		if addrs, err := mail.ParseAddressList(trimmed); err == nil && len(addrs) > 0 {
			for _, a := range addrs {
				clean := strings.TrimSpace(a.Address)
				if clean != "" && !seen[clean] {
					seen[clean] = true
					recipients = append(recipients, clean)
				}
			}
			continue
		}
		parts := strings.FieldsFunc(trimmed, func(r rune) bool {
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
	if len(recipients) == 0 {
		return PermanentError{Err: errors.New("email to addresses are not configured")}
	}

	port := s.cfg.SMTPPort
	secMode := strings.ToLower(strings.TrimSpace(s.cfg.Security))
	if secMode == "" {
		secMode = "starttls"
	}
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

	// Prepare TLS config
	tlsConfig := s.customTLS
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}
	}

	// Dial SMTP connection
	client, err := s.connect(ctx, addr, secMode, host, tlsConfig)
	if err != nil {
		return s.sanitizeError(err)
	}
	defer client.Close()

	// Authenticate if credentials provided
	username := strings.TrimSpace(s.cfg.Username)
	password := strings.TrimSpace(s.cfg.Password)
	if username != "" || password != "" {
		auth := smtp.PlainAuth("", username, password, host)
		if err := client.Auth(auth); err != nil {
			return PermanentError{Err: s.sanitizeError(fmt.Errorf("smtp authentication failed: %w", err))}
		}
	}

	// Sender and recipients
	if err := client.Mail(from); err != nil {
		return s.sanitizeError(fmt.Errorf("smtp MAIL FROM failed: %w", err))
	}

	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return s.sanitizeError(fmt.Errorf("smtp RCPT TO %q failed: %w", rcpt, err))
		}
	}

	// Assemble RFC 5322 message
	msg := s.buildRFC5322Message(from, recipients, n)

	w, err := client.Data()
	if err != nil {
		return s.sanitizeError(fmt.Errorf("smtp DATA command failed: %w", err))
	}

	if _, err := io.WriteString(w, msg); err != nil {
		_ = w.Close()
		return s.sanitizeError(fmt.Errorf("smtp write message failed: %w", err))
	}

	if err := w.Close(); err != nil {
		return s.sanitizeError(fmt.Errorf("smtp close data writer failed: %w", err))
	}

	_ = client.Quit()
	return nil
}

func (s *SMTPNotifier) connect(ctx context.Context, addr, secMode, host string, tlsCfg *tls.Config) (*smtp.Client, error) {
	dialer := s.customDialer
	if dialer == nil {
		stdDialer := &net.Dialer{Timeout: 10 * time.Second}
		dialer = stdDialer.DialContext
	}

	if secMode == "tls" {
		rawConn, err := dialer(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("tls dial %s: %w", addr, err)
		}
		deadline := time.Now().Add(25 * time.Second)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		_ = rawConn.SetDeadline(deadline)

		tlsConn := tls.Client(rawConn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("tls handshake %s: %w", addr, err)
		}
		return smtp.NewClient(tlsConn, host)
	}

	rawConn, err := dialer(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(25 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = rawConn.SetDeadline(deadline)

	client, err := smtp.NewClient(rawConn, host)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("smtp new client %s: %w", addr, err)
	}

	if secMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("starttls negotiation %s: %w", addr, err)
			}
		} else {
			_ = client.Close()
			return nil, fmt.Errorf("smtp server %s does not advertise STARTTLS extension", addr)
		}
	}

	return client, nil
}

func (s *SMTPNotifier) buildRFC5322Message(from string, recipients []string, n Notification) string {
	cleanRule := sanitizeHeader(n.Rule)
	cleanTarget := sanitizeHeader(n.Target)
	cleanSev := strings.ToUpper(sanitizeHeader(n.Severity))
	cleanState := strings.ToUpper(sanitizeHeader(n.State))

	subject := fmt.Sprintf("[Tarsier][%s][%s] %s (%s)", cleanSev, cleanState, cleanRule, cleanTarget)

	ts := n.At.UTC().Format(time.RFC1123Z)
	if n.At.IsZero() {
		ts = time.Now().UTC().Format(time.RFC1123Z)
	}

	var sb strings.Builder
	sb.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	sb.WriteString("To: " + strings.Join(recipients, ", ") + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	sb.WriteString("Date: " + ts + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")

	// Message body with CRLF
	sb.WriteString("Tarsier Alert Notification\r\n")
	sb.WriteString("--------------------------\r\n")
	sb.WriteString(fmt.Sprintf("Rule:     %s\r\n", n.Rule))
	sb.WriteString(fmt.Sprintf("State:    %s\r\n", cleanState))
	sb.WriteString(fmt.Sprintf("Severity: %s\r\n", cleanSev))
	sb.WriteString(fmt.Sprintf("Target:   %s\r\n", n.Target))
	sb.WriteString(fmt.Sprintf("Time:     %s\r\n", n.At.UTC().Format("2006-01-02 15:04:05 UTC")))
	sb.WriteString("\r\n")
	sb.WriteString("Summary:\r\n")
	sb.WriteString(n.Summary + "\r\n")

	if len(n.Details) > 0 {
		sb.WriteString("\r\nDetails:\r\n")
		keys := make([]string, 0, len(n.Details))
		for k := range n.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  %s: %s\r\n", k, n.Details[k]))
		}
	}

	return sb.String()
}

func sanitizeHeader(val string) string {
	r := strings.NewReplacer("\r", " ", "\n", " ")
	return strings.TrimSpace(r.Replace(val))
}

func (s *SMTPNotifier) sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if s.cfg.Password != "" {
		msg = strings.ReplaceAll(msg, s.cfg.Password, "[REDACTED_PASSWORD]")
	}
	return errors.New(msg)
}
