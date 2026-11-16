package cleanup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

type mockSender struct {
	name      string
	attempts  atomic.Int32
	failCount int32
	permError bool
}

func (m *mockSender) Name() string {
	return m.name
}

func (m *mockSender) SendReport(ctx context.Context, payload ReportPayload) error {
	att := m.attempts.Add(1)
	if m.permError {
		return ErrPermanentConfig
	}
	if att <= m.failCount {
		return fmt.Errorf("transient network timeout on attempt %d", att)
	}
	return nil
}

func TestReportDeliverer_RetryAndBestEffortProceed(t *testing.T) {
	ctx := context.Background()

	// Discord succeeds on attempt 2 (fails 1)
	discord := &mockSender{name: "discord", failCount: 1}
	// Email fails on both attempts
	email := &mockSender{name: "email", failCount: 10}

	enabled := true
	cfg := config.CleanupReportConfig{
		Enabled:     &enabled,
		Channels:    []string{"discord", "email"},
		MinAttempts: 2,
		RetryDelay:  "1ms", // Fast retry for tests
		OnFailure:   "proceed",
	}

	deliverer := NewReportDeliverer(cfg, []ReportSender{discord, email},
		WithDelivererSleeper(func(ctx context.Context, d time.Duration) error {
			// Instant sleeper for tests
			return nil
		}),
	)

	payload := ReportPayload{
		Format:       "markdown",
		Filename:     "test-report.md",
		BodyText:     "# Test Report",
		SummaryTitle: "Pre-Cleanup Summary",
		SummaryBody:  "Purged 100 lines",
	}

	summary, err := deliverer.Deliver(ctx, payload, false)
	if err != nil {
		t.Fatalf("Deliver returned unexpected error: %v", err)
	}

	if discord.attempts.Load() != 2 {
		t.Errorf("expected discord 2 attempts, got %d", discord.attempts.Load())
	}
	if email.attempts.Load() != 2 {
		t.Errorf("expected email 2 attempts, got %d", email.attempts.Load())
	}

	if summary.OverallStatus != "partial" {
		t.Errorf("expected OverallStatus 'partial', got %q", summary.OverallStatus)
	}
	if !summary.WarningActive {
		t.Errorf("expected WarningActive = true")
	}
	if !summary.ShouldProceed {
		t.Errorf("expected ShouldProceed = true under on_failure: proceed")
	}
	if !deliverer.IsWarningActive() {
		t.Errorf("expected deliverer.IsWarningActive() = true")
	}
}

func TestReportDeliverer_StrictModeBlockVsEmergencyBypass(t *testing.T) {
	ctx := context.Background()

	email := &mockSender{name: "email", failCount: 10}
	enabled := true
	cfg := config.CleanupReportConfig{
		Enabled:     &enabled,
		Channels:    []string{"email"},
		MinAttempts: 2,
		RetryDelay:  "1ms",
		OnFailure:   "block",
	}

	deliverer := NewReportDeliverer(cfg, []ReportSender{email},
		WithDelivererSleeper(func(ctx context.Context, d time.Duration) error {
			return nil
		}),
	)

	payload := ReportPayload{BodyText: "test"}

	// 1. Scheduled / normal run with failure -> Must BLOCK
	summaryNormal, err := deliverer.Deliver(ctx, payload, false)
	if err != nil {
		t.Fatalf("Deliver normal failed: %v", err)
	}
	if summaryNormal.ShouldProceed {
		t.Fatalf("expected ShouldProceed = false on normal run under OnFailure: block")
	}
	if summaryNormal.OverallStatus != "failed" {
		t.Errorf("expected OverallStatus = 'failed', got %q", summaryNormal.OverallStatus)
	}

	// 2. Emergency size-cap run -> Must BYPASS and PROCEED
	summaryEmergency, err := deliverer.Deliver(ctx, payload, true)
	if err != nil {
		t.Fatalf("Deliver emergency failed: %v", err)
	}
	if !summaryEmergency.ShouldProceed {
		t.Fatalf("expected ShouldProceed = true on emergency run even under OnFailure: block")
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDiscordReportSender_MultipartEmbedAndFile(t *testing.T) {
	var receivedPayloadJSON string
	var receivedFileContent string
	var receivedFilename string

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			err := req.ParseMultipartForm(4 << 20)
			if err != nil {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(err.Error())),
					Header:     make(http.Header),
				}, nil
			}

			receivedPayloadJSON = req.FormValue("payload_json")

			file, header, fErr := req.FormFile("files[0]")
			if fErr == nil {
				defer file.Close()
				receivedFilename = header.Filename
				contentBytes, _ := io.ReadAll(file)
				receivedFileContent = string(contentBytes)
			}

			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	sender := NewDiscordReportSender(config.DiscordNotifierConfig{
		Enabled:    true,
		WebhookURL: "https://discord.com/api/webhooks/12345/super-secret-token",
		Mention:    "@admin",
	}, client)

	payload := ReportPayload{
		Format:       "markdown",
		Filename:     "report-test.md",
		BodyText:     "# Full Markdown Report Content",
		Attachment:   []byte("# Full Markdown Report Content"),
		SummaryTitle: "Pre-Cleanup Report Ready",
		SummaryBody:  "slated to purge 500 lines",
		Report: PreCleanupReport{
			TotalLogsPurged:   500,
			TotalBytesReclaim: 1048576,
			TotalErrors:       5,
			TotalWarnings:     2,
			AlertsFired:       1,
			GeneratedAt:       time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		},
	}

	err := sender.SendReport(context.Background(), payload)
	if err != nil {
		t.Fatalf("SendReport failed: %v", err)
	}

	if receivedFilename != "report-test.md" {
		t.Errorf("expected filename report-test.md, got %q", receivedFilename)
	}
	if receivedFileContent != "# Full Markdown Report Content" {
		t.Errorf("unexpected file content: %s", receivedFileContent)
	}

	var parsedEmbed map[string]any
	if err := json.Unmarshal([]byte(receivedPayloadJSON), &parsedEmbed); err != nil {
		t.Fatalf("invalid json payload: %v", err)
	}

	if parsedEmbed["content"] != "@admin " {
		t.Errorf("expected mention content @admin , got %v", parsedEmbed["content"])
	}
}

func TestSMTPReportSender_MultipartMixedAttachment(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	var captured bytes.Buffer
	serverErr := make(chan error, 1)

	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		w := bufio.NewWriter(serverConn)

		send := func(code int, msg string) {
			_, _ = fmt.Fprintf(w, "%d %s\r\n", code, msg)
			_ = w.Flush()
		}

		send(220, "mock.smtp")

		for {
			line, err := r.ReadString('\n')
			if err != nil {
				serverErr <- err
				return
			}
			line = strings.TrimSpace(line)

			if strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO") {
				send(250, "mock.smtp at your service")
			} else if strings.HasPrefix(line, "MAIL FROM:") {
				send(250, "OK")
			} else if strings.HasPrefix(line, "RCPT TO:") {
				send(250, "OK")
			} else if line == "DATA" {
				send(354, "Start mail input; end with <CRLF>.<CRLF>")
				// Read until solitary dot
				for {
					dataLine, dErr := r.ReadString('\n')
					if dErr != nil {
						serverErr <- dErr
						return
					}
					if strings.TrimRight(dataLine, "\r\n") == "." {
						break
					}
					captured.WriteString(dataLine)
				}
				send(250, "OK message queued")
			} else if line == "QUIT" {
				send(221, "Bye")
				serverErr <- nil
				return
			}
		}
	}()

	sender := NewSMTPReportSender(config.EmailNotifierConfig{
		Enabled:  true,
		SMTPHost: "localhost",
		SMTPPort: 25,
		Security: "none",
		From:     "tarsier@local",
		To:       []string{"ops@local"},
	}, WithCustomDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return clientConn, nil
	}))

	payload := ReportPayload{
		Format:       "markdown",
		Filename:     "tarsier-precleanup.md",
		BodyText:     "# Pre-Cleanup Report Plain Body Text",
		Attachment:   []byte("# Attached Report Bytes"),
		SummaryTitle: "Digest Notice",
	}

	err := sender.SendReport(context.Background(), payload)
	if err != nil {
		t.Fatalf("SendReport failed: %v", err)
	}

	select {
	case sErr := <-serverErr:
		if sErr != nil {
			t.Fatalf("mock SMTP server error: %v", sErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for mock SMTP session")
	}

	dataMsg := captured.String()

	// Verify headers
	if !strings.Contains(dataMsg, "From: tarsier@local\r\n") {
		t.Errorf("missing From header")
	}
	if !strings.Contains(dataMsg, "To: ops@local\r\n") {
		t.Errorf("missing To header")
	}
	if !strings.Contains(dataMsg, "Content-Type: multipart/mixed; boundary=") {
		t.Errorf("missing multipart/mixed header")
	}

	// Parse multipart message
	boundary := ""
	for _, l := range strings.Split(dataMsg, "\r\n") {
		if strings.HasPrefix(l, "Content-Type: multipart/mixed; boundary=\"") {
			boundary = strings.TrimPrefix(l, "Content-Type: multipart/mixed; boundary=\"")
			boundary = strings.TrimSuffix(boundary, "\"")
			break
		}
	}
	if boundary == "" {
		t.Fatalf("boundary not found in message: %s", dataMsg)
	}

	// Locate start of multipart body (after empty line CRLF CRLF)
	bodyIdx := strings.Index(dataMsg, "\r\n\r\n")
	if bodyIdx == -1 {
		t.Fatalf("invalid email structure: header/body separator not found")
	}
	mpBody := dataMsg[bodyIdx+4:]

	mr := multipart.NewReader(strings.NewReader(mpBody), boundary)

	// Part 1: BodyText
	p1, err := mr.NextPart()
	if err != nil {
		t.Fatalf("failed reading part 1: %v", err)
	}
	p1Data, _ := io.ReadAll(p1)
	if !strings.Contains(string(p1Data), "# Pre-Cleanup Report Plain Body Text") {
		t.Errorf("part 1 did not contain expected body text: %s", string(p1Data))
	}

	// Part 2: Attachment
	p2, err := mr.NextPart()
	if err != nil {
		t.Fatalf("failed reading part 2: %v", err)
	}
	disp, _, _ := mime.ParseMediaType(p2.Header.Get("Content-Disposition"))
	if disp != "attachment" {
		t.Errorf("expected Content-Disposition attachment, got %q", disp)
	}

	p2Raw, _ := io.ReadAll(p2)
	// Base64 decode attachment
	cleanedBase64 := strings.ReplaceAll(string(p2Raw), "\r\n", "")
	cleanedBase64 = strings.TrimSpace(cleanedBase64)
	decodedBytes, dErr := base64.StdEncoding.DecodeString(cleanedBase64)
	if dErr != nil {
		t.Fatalf("failed base64 decoding attachment: %v\nRaw: %s", dErr, string(p2Raw))
	}

	if string(decodedBytes) != "# Attached Report Bytes" {
		t.Errorf("expected attachment '# Attached Report Bytes', got %q", string(decodedBytes))
	}
}

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "POST https://discord.com/api/webhooks/123456789/AbCdEfGhIjKlMnOpQrStUvWxYz failed: 404",
			expected: "POST https://discord.com/api/webhooks/123456789/*** failed: 404",
		},
		{
			input:    "dial tcp://admin:supersecret@smtp.example.com:587: connection refused",
			expected: "dial tcp://admin:***@smtp.example.com:587: connection refused",
		},
		{
			input:    "normal error message without secrets",
			expected: "normal error message without secrets",
		},
	}

	for _, tc := range tests {
		got := RedactSecrets(tc.input)
		if got != tc.expected {
			t.Errorf("RedactSecrets(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSMTPReportSender_HangingServerDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	// Mock server that never sends 220 greeting banner
	sender := NewSMTPReportSender(config.EmailNotifierConfig{
		Enabled:  true,
		SMTPHost: "smtp.mock.local",
		SMTPPort: 587,
		Security: "starttls",
		From:     "tarsier@local",
		To:       []string{"ops@local"},
	}, WithCustomDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return clientConn, nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	payload := ReportPayload{
		Format:       "markdown",
		Filename:     "report.md",
		BodyText:     "Report body",
		SummaryTitle: "Notice",
	}

	start := time.Now()
	err := sender.SendReport(ctx, payload)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from hanging SMTP server, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected timeout within ~200ms, took %v", elapsed)
	}
}

func TestSMTPReportSender_MissingSTARTTLSError(t *testing.T) {
	serverConn, clientConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		w := bufio.NewWriter(serverConn)

		_, _ = fmt.Fprintf(w, "220 mock.smtp ESMTP\r\n")
		_ = w.Flush()

		line, _ := r.ReadString('\n')
		if strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO") {
			// Reply without STARTTLS extension
			_, _ = fmt.Fprintf(w, "250-mock.smtp\r\n250 SIZE 35882577\r\n")
			_ = w.Flush()
		}
	}()

	sender := NewSMTPReportSender(config.EmailNotifierConfig{
		Enabled:  true,
		SMTPHost: "smtp.mock.local",
		SMTPPort: 587,
		Security: "starttls",
		From:     "tarsier@local",
		To:       []string{"ops@local"},
	}, WithCustomDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return clientConn, nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := sender.SendReport(ctx, ReportPayload{
		Format:       "markdown",
		BodyText:     "test",
		SummaryTitle: "Notice",
	})

	if err == nil {
		t.Fatal("expected error when STARTTLS is required but not advertised, got nil")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected error to mention STARTTLS, got %v", err)
	}
}

func TestParseRecipients(t *testing.T) {
	tests := []struct {
		input    []string
		expected []string
	}{
		{
			input:    []string{"dale@febinanddale.com,123daletomson@gmail.com"},
			expected: []string{"dale@febinanddale.com", "123daletomson@gmail.com"},
		},
		{
			input:    []string{"Dale Tomson <dale@febinanddale.com>", "ops@local"},
			expected: []string{"dale@febinanddale.com", "ops@local"},
		},
		{
			input:    []string{"a@x.com, b@x.com; c@x.com", "d@x.com"},
			expected: []string{"a@x.com", "b@x.com", "c@x.com", "d@x.com"},
		},
		{
			input:    []string{"a@x.com", "a@x.com", ""},
			expected: []string{"a@x.com"},
		},
	}

	for _, tc := range tests {
		got := ParseRecipients(tc.input)
		if len(got) != len(tc.expected) {
			t.Fatalf("ParseRecipients(%v) length = %d, want %d (%v)", tc.input, len(got), len(tc.expected), got)
		}
		for i := range got {
			if got[i] != tc.expected[i] {
				t.Errorf("ParseRecipients(%v)[%d] = %q, want %q", tc.input, i, got[i], tc.expected[i])
			}
		}
	}
}

func TestSMTPReportSender_CommaSeparatedRecipients(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	var rcptCommands []string
	serverErr := make(chan error, 1)

	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		w := bufio.NewWriter(serverConn)

		send := func(code int, msg string) {
			_, _ = fmt.Fprintf(w, "%d %s\r\n", code, msg)
			_ = w.Flush()
		}

		send(220, "mock.smtp")

		for {
			line, err := r.ReadString('\n')
			if err != nil {
				serverErr <- err
				return
			}
			line = strings.TrimSpace(line)

			if strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO") {
				send(250, "mock.smtp at your service")
			} else if strings.HasPrefix(line, "MAIL FROM:") {
				send(250, "OK")
			} else if strings.HasPrefix(line, "RCPT TO:") {
				rcptCommands = append(rcptCommands, line)
				send(250, "OK")
			} else if line == "DATA" {
				send(354, "Go ahead")
				for {
					dataLine, dErr := r.ReadString('\n')
					if dErr != nil {
						serverErr <- dErr
						return
					}
					if strings.TrimRight(dataLine, "\r\n") == "." {
						break
					}
				}
				send(250, "OK")
			} else if line == "QUIT" {
				send(221, "Bye")
				serverErr <- nil
				return
			}
		}
	}()

	sender := NewSMTPReportSender(config.EmailNotifierConfig{
		Enabled:  true,
		SMTPHost: "smtp.mock.local",
		SMTPPort: 25,
		Security: "none",
		From:     "tarsier@local",
		To:       []string{"dale@febinanddale.com,123daletomson@gmail.com"},
	}, WithCustomDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return clientConn, nil
	}))

	err := sender.SendReport(context.Background(), ReportPayload{
		Format:       "markdown",
		BodyText:     "test",
		SummaryTitle: "Notice",
	})
	if err != nil {
		t.Fatalf("SendReport failed: %v", err)
	}

	select {
	case sErr := <-serverErr:
		if sErr != nil {
			t.Fatalf("mock server error: %v", sErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for mock SMTP session")
	}

	if len(rcptCommands) != 2 {
		t.Fatalf("expected 2 distinct RCPT TO commands, got %d: %v", len(rcptCommands), rcptCommands)
	}
	if rcptCommands[0] != "RCPT TO:<dale@febinanddale.com>" {
		t.Errorf("expected first RCPT TO:<dale@febinanddale.com>, got %q", rcptCommands[0])
	}
	if rcptCommands[1] != "RCPT TO:<123daletomson@gmail.com>" {
		t.Errorf("expected second RCPT TO:<123daletomson@gmail.com>, got %q", rcptCommands[1])
	}
}
