package alert

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
)

// roundTripFunc implements http.RoundTripper as a function.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDiscordNotifier_PayloadAndRetry(t *testing.T) {
	var attempts atomic.Int32
	var receivedPayloads []discordPayload
	var mu sync.Mutex

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		att := attempts.Add(1)

		bodyBytes, _ := io.ReadAll(req.Body)
		var p discordPayload
		if err := json.Unmarshal(bodyBytes, &p); err == nil {
			mu.Lock()
			receivedPayloads = append(receivedPayloads, p)
			mu.Unlock()
		}

		// First 2 attempts fail with 503, 3rd attempt succeeds with 204
		if att <= 2 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("upstream service unavailable")),
				Header:     make(http.Header),
			}, nil
		}

		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})

	httpClient := &http.Client{Transport: transport}
	cfg := config.DiscordNotifierConfig{
		Enabled:    true,
		WebhookURL: "https://discord.com/api/webhooks/123456/secret-token-xyz",
		Mention:    "<@&9999>",
	}

	dn := NewDiscordNotifier(cfg, WithDiscordHTTPClient(httpClient))

	disp, err := NewDispatcher(config.NotifiersConfig{}, nil,
		WithNotifier("discord", dn),
		WithRetryBackoffs([]time.Duration{2 * time.Millisecond, 5 * time.Millisecond}),
		WithAttemptTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}
	defer disp.Close(context.Background())

	notif := Notification{
		Rule:     "High Host CPU",
		Target:   "host",
		Severity: "critical",
		State:    "firing",
		Summary:  "CPU utilization exceeded 90% for 5m",
		Details: map[string]string{
			"cpu_pct": "94.2%",
			"load1":   "4.82",
		},
		At: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	}

	disp.Dispatch([]string{"discord"}, notif)

	// Wait for retry attempts to complete
	deadline := time.Now().Add(500 * time.Millisecond)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedPayloads) == 0 {
		t.Fatal("expected at least 1 received payload")
	}

	p := receivedPayloads[len(receivedPayloads)-1]
	if p.Content != "<@&9999>" {
		t.Errorf("expected mention '<@&9999>', got %q", p.Content)
	}
	if len(p.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(p.Embeds))
	}
	emb := p.Embeds[0]
	if emb.Title != "[FIRING] High Host CPU" {
		t.Errorf("unexpected embed title: %q", emb.Title)
	}
	if emb.Color != ColorCritical {
		t.Errorf("expected critical color %d, got %d", ColorCritical, emb.Color)
	}
	if emb.Description != notif.Summary {
		t.Errorf("expected summary %q, got %q", notif.Summary, emb.Description)
	}

	// Verify details are sorted
	var detailKeys []string
	for _, f := range emb.Fields {
		if f.Name != "Target" && f.Name != "Severity" {
			detailKeys = append(detailKeys, f.Name)
		}
	}
	if len(detailKeys) != 2 || detailKeys[0] != "cpu_pct" || detailKeys[1] != "load1" {
		t.Errorf("expected sorted detail keys [cpu_pct, load1], got %v", detailKeys)
	}

	// Test resolved notification embed color
	resolvedNotif := notif
	resolvedNotif.State = "resolved"
	resolvedPayload := dn.buildPayload(resolvedNotif)
	if resolvedPayload.Content != "" {
		t.Errorf("expected empty content on resolved, got %q", resolvedPayload.Content)
	}
	if resolvedPayload.Embeds[0].Color != ColorResolved {
		t.Errorf("expected resolved color %d, got %d", ColorResolved, resolvedPayload.Embeds[0].Color)
	}
	if resolvedPayload.Embeds[0].Title != "[RESOLVED] High Host CPU" {
		t.Errorf("unexpected resolved title: %q", resolvedPayload.Embeds[0].Title)
	}
}

func TestDiscordNotifier_RedactsSecretsOn401(t *testing.T) {
	secretURL := "https://discord.com/api/webhooks/987654321/super-secret-token"
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader("Invalid Webhook Token for " + secretURL)),
			Header:     make(http.Header),
		}, nil
	})

	httpClient := &http.Client{Transport: transport}
	cfg := config.DiscordNotifierConfig{
		Enabled:    true,
		WebhookURL: secretURL,
	}

	dn := NewDiscordNotifier(cfg, WithDiscordHTTPClient(httpClient))
	err := dn.Send(context.Background(), Notification{
		Rule:     "Test",
		Severity: "info",
		State:    "firing",
	})

	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}

	var permErr PermanentError
	if !errors.As(err, &permErr) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secretURL) {
		t.Errorf("secret URL leaked in error message: %s", errMsg)
	}
	if strings.Contains(errMsg, "super-secret-token") {
		t.Errorf("secret token leaked in error message: %s", errMsg)
	}
}

// generateSelfSignedCert creates an in-memory TLS certificate for mock SMTP testing.
func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Tarsier Test"},
			CommonName:   "127.0.0.1",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost", "smtp.test.local"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}
}

// runMockSMTPServer handles an in-memory or TCP mock SMTP session.
func runMockSMTPServer(conn net.Conn, cert *tls.Certificate, startTLS bool, authFailMsg string, captured *bytes.Buffer, errChan chan error) {
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	var activeReader = r
	var activeWriter = w
	isTLS := false

	send := func(code int, msg string) error {
		_, err := fmt.Fprintf(activeWriter, "%d %s\r\n", code, msg)
		if err == nil {
			err = activeWriter.Flush()
		}
		return err
	}

	if err := send(220, "smtp.test.local ESMTP Mock"); err != nil {
		errChan <- err
		return
	}

	for {
		line, err := activeReader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				errChan <- nil
				return
			}
			errChan <- err
			return
		}

		line = strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToUpper(parts[0])

		switch cmd {
		case "EHLO", "HELO":
			if startTLS && !isTLS {
				_, _ = fmt.Fprintf(activeWriter, "250-localhost\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
				_ = activeWriter.Flush()
			} else {
				_, _ = fmt.Fprintf(activeWriter, "250-localhost\r\n250 AUTH PLAIN\r\n")
				_ = activeWriter.Flush()
			}
		case "STARTTLS":
			_ = send(220, "Ready to start TLS")
			tlsServer := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{*cert},
			})
			if err := tlsServer.Handshake(); err != nil {
				errChan <- fmt.Errorf("server tls handshake: %w", err)
				return
			}
			isTLS = true
			activeReader = bufio.NewReader(tlsServer)
			activeWriter = bufio.NewWriter(tlsServer)
		case "AUTH":
			if authFailMsg != "" {
				_ = send(535, authFailMsg)
				errChan <- nil
				return
			}
			_ = send(235, "Authentication successful")
		case "MAIL":
			_ = send(250, "Sender OK")
		case "RCPT":
			_ = send(250, "Recipient OK")
		case "DATA":
			_ = send(354, "Start mail input; end with <CRLF>.<CRLF>")
			for {
				dataLine, err := activeReader.ReadString('\n')
				if err != nil {
					errChan <- err
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
				captured.WriteString(dataLine)
			}
			_ = send(250, "Message accepted for delivery")
		case "QUIT":
			_ = send(221, "Bye")
			errChan <- nil
			return
		default:
			_ = send(500, "Command not recognized")
		}
	}
}

func TestSMTPNotifier_PlainAndStartTLS(t *testing.T) {
	cert := generateSelfSignedCert(t)
	tlsClientConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	testCases := []struct {
		name     string
		security string
		startTLS bool
	}{
		{name: "None_Security", security: "none", startTLS: false},
		{name: "StartTLS_Security", security: "starttls", startTLS: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			var captured bytes.Buffer
			serverErr := make(chan error, 1)

			go runMockSMTPServer(serverConn, &cert, tc.startTLS, "", &captured, serverErr)

			smtpCfg := config.EmailNotifierConfig{
				Enabled:  true,
				SMTPHost: "localhost",
				SMTPPort: 2525,
				Security: tc.security,
				Username: "tarsier_user",
				Password: "tarsier_password",
				From:     "alerts@tarsier.local",
				To:       []string{"admin@tarsier.local", "ops@tarsier.local"},
			}

			notifier := NewSMTPNotifier(smtpCfg,
				WithSMTPDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
					return clientConn, nil
				}),
				WithSMTPTLSConfig(tlsClientConfig),
			)

			notif := Notification{
				Rule:     "High Host Memory",
				Target:   "host",
				Severity: "critical",
				State:    "firing",
				Summary:  "Memory utilization exceeded 92%",
				Details: map[string]string{
					"mem_used_pct": "93.4%",
				},
				At: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
			}

			err := notifier.Send(context.Background(), notif)
			if err != nil {
				t.Fatalf("notifier.Send failed: %v", err)
			}

			select {
			case sErr := <-serverErr:
				if sErr != nil {
					t.Fatalf("mock server error: %v", sErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for mock SMTP server")
			}

			msg := captured.String()

			// Assert RFC 5322 Headers
			if !strings.Contains(msg, "From: alerts@tarsier.local\r\n") {
				t.Errorf("missing or invalid From header: %s", msg)
			}
			if !strings.Contains(msg, "To: admin@tarsier.local, ops@tarsier.local\r\n") {
				t.Errorf("missing or invalid To header: %s", msg)
			}
			expectedSubject := "Subject: [Tarsier][CRITICAL][FIRING] High Host Memory (host)\r\n"
			if !strings.Contains(msg, expectedSubject) {
				t.Errorf("expected subject %q, got: %s", expectedSubject, msg)
			}
			if !strings.Contains(msg, "Content-Type: text/plain; charset=utf-8\r\n") {
				t.Errorf("missing Content-Type header: %s", msg)
			}

			// Assert Body
			if !strings.Contains(msg, "Rule:     High Host Memory\r\n") {
				t.Errorf("missing Rule in body: %s", msg)
			}
			if !strings.Contains(msg, "Summary:\r\nMemory utilization exceeded 92%\r\n") {
				t.Errorf("missing Summary in body: %s", msg)
			}
			if !strings.Contains(msg, "mem_used_pct: 93.4%\r\n") {
				t.Errorf("missing details in body: %s", msg)
			}
		})
	}
}

func TestSMTPNotifier_RedactsPasswordOnAuthFailure(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	var captured bytes.Buffer
	serverErr := make(chan error, 1)

	secretPass := "super_secret_pass"
	failMsg := "Authentication credentials invalid for " + secretPass

	go runMockSMTPServer(serverConn, nil, false, failMsg, &captured, serverErr)

	smtpCfg := config.EmailNotifierConfig{
		Enabled:  true,
		SMTPHost: "localhost",
		SMTPPort: 25,
		Security: "none",
		Username: "admin",
		Password: secretPass,
		From:     "alerts@tarsier.local",
		To:       []string{"ops@tarsier.local"},
	}

	notifier := NewSMTPNotifier(smtpCfg,
		WithSMTPDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return clientConn, nil
		}),
	)

	err := notifier.Send(context.Background(), Notification{
		Rule:     "Test",
		Severity: "info",
		State:    "firing",
	})

	if err == nil {
		t.Fatal("expected auth failure, got nil")
	}

	var permErr PermanentError
	if !errors.As(err, &permErr) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secretPass) {
		t.Errorf("password leaked in error message: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED_PASSWORD]") {
		t.Errorf("expected [REDACTED_PASSWORD] in error message: %s", errMsg)
	}
}

func TestDispatcher_ChannelIsolationAndDBStatus(t *testing.T) {
	// 1. Initialize temporary SQLite database store
	tmpDir := t.TempDir()
	dbStore, err := db.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer dbStore.Close()

	// Insert test alert_event row
	var eventID int64
	err = dbStore.Config.ExecWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, `
			INSERT INTO alert_event (rule_id, rule_name, target, severity, state, started, summary, details_json, notify_status, created_at)
			VALUES (1, 'Disk Full', 'disk:/', 'critical', 'firing', 1700000000, 'disk is full', '{}', 'pending', 1700000000)
		`)
		if execErr != nil {
			return execErr
		}
		eventID, execErr = res.LastInsertId()
		return execErr
	})
	if err != nil {
		t.Fatalf("insert alert_event: %v", err)
	}

	// 2. Mock Fast Discord (204 No Content)
	discordSent := make(chan struct{}, 1)
	fastDiscord := &mockNotifier{
		name: "discord",
		sendFunc: func(ctx context.Context, n Notification) error {
			select {
			case discordSent <- struct{}{}:
			default:
			}
			return nil
		},
	}

	// 3. Mock Slow / Stalled SMTP (returns timeout error after attempts)
	smtpFailed := make(chan struct{}, 1)
	failingSMTP := &mockNotifier{
		name: "email",
		sendFunc: func(ctx context.Context, n Notification) error {
			select {
			case smtpFailed <- struct{}{}:
			default:
			}
			return errors.New("connection timed out")
		},
	}

	disp, err := NewDispatcher(config.NotifiersConfig{}, dbStore,
		WithNotifier("discord", fastDiscord),
		WithNotifier("email", failingSMTP),
		WithRetryBackoffs([]time.Duration{2 * time.Millisecond, 5 * time.Millisecond}),
		WithAttemptTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer disp.Close(context.Background())

	notif := Notification{
		EventID:  eventID,
		Rule:     "Disk Full",
		Target:   "disk:/",
		Severity: "critical",
		State:    "firing",
		Summary:  "disk is full",
		At:       time.Now().UTC(),
	}

	start := time.Now()
	disp.Dispatch([]string{"discord", "email"}, notif)

	// Discord must deliver almost immediately
	select {
	case <-discordSent:
		if time.Since(start) > 200*time.Millisecond {
			t.Errorf("discord took too long, was it blocked by smtp? took %v", time.Since(start))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("discord did not deliver in time")
	}

	// Wait for delivery tracker to update alert_event.notify_status in DB
	var notifyStatus string
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		row := dbStore.Config.QueryRO().QueryRow("SELECT notify_status FROM alert_event WHERE id = ?", eventID)
		if scanErr := row.Scan(&notifyStatus); scanErr == nil && notifyStatus != "pending" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if !strings.Contains(notifyStatus, "discord") || !strings.Contains(notifyStatus, "email") {
		t.Errorf("expected combined status containing discord and email, got %q", notifyStatus)
	}
	if !strings.Contains(notifyStatus, "connection timed out") {
		t.Errorf("expected status to mention failure reason, got %q", notifyStatus)
	}
}

func TestDispatcher_QueueFullDrop(t *testing.T) {
	hangChan := make(chan struct{})
	hangingNotifier := &mockNotifier{
		name: "discord",
		sendFunc: func(ctx context.Context, n Notification) error {
			<-hangChan
			return nil
		},
	}

	disp, err := NewDispatcher(config.NotifiersConfig{}, nil,
		WithNotifier("discord", hangingNotifier),
	)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer func() {
		close(hangChan)
		disp.Close(context.Background())
	}()

	// Fill queue (128 capacity + 1 in-flight)
	for i := 0; i < 135; i++ {
		disp.Dispatch([]string{"discord"}, Notification{Rule: fmt.Sprintf("Rule %d", i)})
	}

	dropped := disp.DroppedCount("discord")
	if dropped == 0 {
		t.Errorf("expected dropped notifications when queue full, got 0")
	}
}

func TestDispatcher_SendTest(t *testing.T) {
	called := false
	testNotifier := &mockNotifier{
		name: "discord",
		sendFunc: func(ctx context.Context, n Notification) error {
			called = true
			if n.Rule != "Test Notification" {
				return fmt.Errorf("unexpected rule: %s", n.Rule)
			}
			return nil
		},
	}

	disp, err := NewDispatcher(config.NotifiersConfig{}, nil,
		WithNotifier("discord", testNotifier),
	)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer disp.Close(context.Background())

	if err := disp.SendTest(context.Background(), "discord"); err != nil {
		t.Fatalf("SendTest failed: %v", err)
	}
	if !called {
		t.Fatal("SendTest did not call notifier")
	}

	// Unknown channel returns error
	if err := disp.SendTest(context.Background(), "unknown_channel"); err == nil {
		t.Fatal("expected error for unknown channel, got nil")
	}
}

type mockNotifier struct {
	name     string
	sendFunc func(ctx context.Context, n Notification) error
}

func (m *mockNotifier) Name() string {
	return m.name
}

func (m *mockNotifier) Send(ctx context.Context, n Notification) error {
	if m.sendFunc != nil {
		return m.sendFunc(ctx, n)
	}
	return nil
}

// Unused imports check
var (
	_ = httptest.NewServer
	_ = filepath.Join
)
