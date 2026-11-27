package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("TEST_VAR", "hello_world")
	t.Setenv("EMPTY_VAR", "")

	tests := []struct {
		name      string
		input     string
		want      string
		expectErr bool
	}{
		{
			name:      "resolves set variable",
			input:     "key: ${TEST_VAR}",
			want:      "key: hello_world",
			expectErr: false,
		},
		{
			name:      "resolves unset variable to empty string",
			input:     "key: ${UNSET_VAR_XYZ}",
			want:      "key: ",
			expectErr: false,
		},
		{
			name:      "resolves fallback when unset",
			input:     "key: ${UNSET_VAR_XYZ:-default_val}",
			want:      "key: default_val",
			expectErr: false,
		},
		{
			name:      "resolves fallback when empty",
			input:     "key: ${EMPTY_VAR:-default_val}",
			want:      "key: default_val",
			expectErr: false,
		},
		{
			name:      "uses set value instead of fallback",
			input:     "key: ${TEST_VAR:-default_val}",
			want:      "key: hello_world",
			expectErr: false,
		},
		{
			name:      "preserves raw dollar signs like bcrypt hashes",
			input:     `hash: "$2a$12$e8nZtGj0b9w47F8P4g9EueRklkL"`,
			want:      `hash: "$2a$12$e8nZtGj0b9w47F8P4g9EueRklkL"`,
			expectErr: false,
		},
		{
			name:      "handles URL in default value",
			input:     `webhook: "${WEBHOOK_URL:-https://discord.com/api/webhooks/123/abc}"`,
			want:      `webhook: "https://discord.com/api/webhooks/123/abc"`,
			expectErr: false,
		},
		{
			name:      "unclosed substitution returns error",
			input:     "key: ${UNCLOSED_VAR",
			want:      "",
			expectErr: true,
		},
		{
			name:      "invalid variable name returns error",
			input:     "key: ${123_INVALID:-default}",
			want:      "",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandEnv([]byte(tc.input))
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", string(got), tc.want)
			}
		})
	}
}

func findConfigFile(filename string) string {
	candidates := []string{
		filepath.Join("..", "..", filename),
		filepath.Join("..", "..", "..", filename),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return filepath.Join("..", "..", "..", filename)
}

func TestLoadConfig_Table(t *testing.T) {
	t.Run("load example config file", func(t *testing.T) {
		cfgPath := findConfigFile("tarsier.example.yaml")
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("failed to load tarsier.example.yaml: %v", err)
		}

		if cfg.Server.Listen != ":8080" {
			t.Errorf("expected listen :8080, got %s", cfg.Server.Listen)
		}
		if cfg.Server.Timezone != "UTC" {
			t.Errorf("expected timezone UTC, got %s", cfg.Server.Timezone)
		}
		if cfg.Storage.Path != "/data" {
			t.Errorf("expected storage path /data, got %s", cfg.Storage.Path)
		}
		if cfg.Host.Enabled == nil || !*cfg.Host.Enabled {
			t.Errorf("expected host enabled true")
		}
		if cfg.Cleanup.Schedule != "0 3 * * *" {
			t.Errorf("expected cleanup schedule '0 3 * * *', got %s", cfg.Cleanup.Schedule)
		}
	})

	t.Run("load local config file", func(t *testing.T) {
		cfgPath := findConfigFile("tarsier.yaml")
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("failed to load tarsier.yaml: %v", err)
		}

		if cfg.Server.Listen != ":8080" {
			t.Errorf("expected listen :8080, got %s", cfg.Server.Listen)
		}
		if cfg.Server.BaseURL != "http://localhost:8080" {
			t.Errorf("expected base_url http://localhost:8080, got %s", cfg.Server.BaseURL)
		}
		if cfg.Storage.Path != "/data" {
			t.Errorf("expected storage path /data, got %s", cfg.Storage.Path)
		}
	})

	t.Run("minimal yaml populates defaults", func(t *testing.T) {
		minimalYAML := []byte(`
server: {}
`)
		cfg, err := LoadBytes(minimalYAML)
		if err != nil {
			t.Fatalf("failed to load minimal yaml: %v", err)
		}

		if cfg.Server.Listen != ":8080" {
			t.Errorf("expected default listen :8080, got %s", cfg.Server.Listen)
		}
		if cfg.Server.Timezone != "UTC" {
			t.Errorf("expected default timezone UTC, got %s", cfg.Server.Timezone)
		}
		if cfg.Storage.Path != "/data" {
			t.Errorf("expected default storage path /data, got %s", cfg.Storage.Path)
		}
		if cfg.Storage.LogsMaxSize != "3GiB" {
			t.Errorf("expected default logs max size 3GiB, got %s", cfg.Storage.LogsMaxSize)
		}
		if cfg.Retention.Logs != "7d" {
			t.Errorf("expected default retention logs 7d, got %s", cfg.Retention.Logs)
		}
		if cfg.Retention.ContainersExited != "24h" {
			t.Errorf("expected default retention containers_exited 24h, got %s", cfg.Retention.ContainersExited)
		}
		if cfg.Cleanup.BatchSize != 5000 {
			t.Errorf("expected default batch size 5000, got %d", cfg.Cleanup.BatchSize)
		}
	})

	t.Run("empty input populates defaults", func(t *testing.T) {
		cfg, err := LoadBytes([]byte(""))
		if err != nil {
			t.Fatalf("failed to load empty input: %v", err)
		}
		if cfg.Server.Listen != ":8080" {
			t.Errorf("expected default listen :8080, got %s", cfg.Server.Listen)
		}
	})

	t.Run("malformed yaml syntax returns error", func(t *testing.T) {
		badYAML := []byte(`
server:
  listen: :8080
    invalid: indent
`)
		_, err := LoadBytes(badYAML)
		if err == nil {
			t.Fatal("expected error for malformed yaml syntax, got nil")
		}
	})

	t.Run("unknown yaml key rejected due to KnownFields", func(t *testing.T) {
		unknownFieldYAML := []byte(`
server:
  listen: ":8080"
  unknown_property: true
`)
		_, err := LoadBytes(unknownFieldYAML)
		if err == nil {
			t.Fatal("expected error for unknown yaml field, got nil")
		}
		if !strings.Contains(err.Error(), "unknown field") && !strings.Contains(err.Error(), "not found") {
			t.Logf("got error as expected: %v", err)
		}
	})
}

func TestValidateConfig(t *testing.T) {
	baseValid := func() *Config {
		c := NewDefaultConfig()
		c.ApplyDefaults()
		return c
	}

	tests := []struct {
		name      string
		modify    func(c *Config)
		expectErr string
	}{
		{
			name: "empty server.listen",
			modify: func(c *Config) {
				c.Server.Listen = ""
			},
			expectErr: "server.listen must not be empty",
		},
		{
			name: "invalid server.timezone",
			modify: func(c *Config) {
				c.Server.Timezone = "Mars/Phobos"
			},
			expectErr: "server.timezone invalid",
		},
		{
			name: "invalid server.base_url",
			modify: func(c *Config) {
				c.Server.BaseURL = "not-a-valid-url"
			},
			expectErr: "server.base_url must be a valid absolute URL",
		},
		{
			name: "out of bounds cache.log_tail_lines low",
			modify: func(c *Config) {
				c.Cache.LogTailLines = 10
			},
			expectErr: "cache.log_tail_lines must be between 50 and 2000",
		},
		{
			name: "out of bounds cache.log_tail_lines high",
			modify: func(c *Config) {
				c.Cache.LogTailLines = 5000
			},
			expectErr: "cache.log_tail_lines must be between 50 and 2000",
		},
		{
			name: "out of bounds cache.max_sources low",
			modify: func(c *Config) {
				c.Cache.MaxSources = 0
			},
			expectErr: "cache.max_sources must be between 1 and 10000",
		},
		{
			name: "out of bounds cache.max_sources high",
			modify: func(c *Config) {
				c.Cache.MaxSources = 20000
			},
			expectErr: "cache.max_sources must be between 1 and 10000",
		},
		{
			name: "out of bounds cache.metric_max_series low",
			modify: func(c *Config) {
				c.Cache.MetricMaxSeries = 50
			},
			expectErr: "cache.metric_max_series must be between 100 and 20000",
		},
		{
			name: "out of bounds cache.metric_max_series high",
			modify: func(c *Config) {
				c.Cache.MetricMaxSeries = 30000
			},
			expectErr: "cache.metric_max_series must be between 100 and 20000",
		},
		{
			name: "out of bounds cache.metric_ring_points low",
			modify: func(c *Config) {
				c.Cache.MetricRingPoints = 10
			},
			expectErr: "cache.metric_ring_points must be between 30 and 360",
		},
		{
			name: "out of bounds cache.metric_ring_points high",
			modify: func(c *Config) {
				c.Cache.MetricRingPoints = 500
			},
			expectErr: "cache.metric_ring_points must be between 30 and 360",
		},
		{
			name: "invalid cleanup.schedule not a cron",
			modify: func(c *Config) {
				c.Cleanup.Schedule = "not-a-cron"
			},
			expectErr: "cleanup.schedule invalid",
		},
		{
			name: "invalid cleanup.schedule out of bounds minute",
			modify: func(c *Config) {
				c.Cleanup.Schedule = "60 * * * *"
			},
			expectErr: "cleanup.schedule invalid",
		},
		{
			name: "invalid byte size negative",
			modify: func(c *Config) {
				c.Storage.LogsMaxSize = "-5GiB"
			},
			expectErr: "storage.logs_max_size invalid",
		},
		{
			name: "invalid byte size unknown unit",
			modify: func(c *Config) {
				c.Storage.LogsMaxSize = "10XB"
			},
			expectErr: "storage.logs_max_size invalid",
		},
		{
			name: "negative duration",
			modify: func(c *Config) {
				c.Retention.Logs = "-5d"
			},
			expectErr: "retention.logs invalid",
		},
		{
			name: "zero duration where positive required",
			modify: func(c *Config) {
				c.Retention.Logs = "0s"
			},
			expectErr: "retention.logs must be a positive duration",
		},
		{
			name: "invalid retention.containers_exited negative",
			modify: func(c *Config) {
				c.Retention.ContainersExited = "-1h"
			},
			expectErr: "retention.containers_exited invalid",
		},
		{
			name: "invalid retention.containers_exited zero",
			modify: func(c *Config) {
				c.Retention.ContainersExited = "0s"
			},
			expectErr: "retention.containers_exited must be a positive duration",
		},
		{
			name: "discord enabled without webhook",
			modify: func(c *Config) {
				c.Notifiers.Discord.Enabled = true
				c.Notifiers.Discord.WebhookURL = ""
			},
			expectErr: "notifiers.discord.webhook_url must not be empty",
		},
		{
			name: "email enabled without smtp_host",
			modify: func(c *Config) {
				c.Notifiers.Email.Enabled = true
				c.Notifiers.Email.SMTPHost = ""
				c.Notifiers.Email.From = "a@b.com"
				c.Notifiers.Email.To = []string{"c@d.com"}
			},
			expectErr: "notifiers.email.smtp_host must not be empty",
		},
		{
			name: "email enabled invalid security",
			modify: func(c *Config) {
				c.Notifiers.Email.Enabled = true
				c.Notifiers.Email.SMTPHost = "smtp.test.com"
				c.Notifiers.Email.From = "a@b.com"
				c.Notifiers.Email.To = []string{"c@d.com"}
				c.Notifiers.Email.Security = "invalid_sec"
			},
			expectErr: "notifiers.email.security must be 'none', 'starttls', or 'tls'",
		},
		{
			name: "log file empty name or path",
			modify: func(c *Config) {
				c.Logs.Files = []LogFileConfig{
					{Name: "", Path: "/var/log/test.log"},
				}
			},
			expectErr: "logs.files[0].name must not be empty",
		},
		{
			name: "cleanup report invalid on_failure",
			modify: func(c *Config) {
				c.Cleanup.Report.OnFailure = "ignore"
			},
			expectErr: "cleanup.report.on_failure must be 'proceed' or 'block'",
		},
		{
			name: "cleanup report invalid format",
			modify: func(c *Config) {
				c.Cleanup.Report.Format = "json"
			},
			expectErr: "cleanup.report.format must be 'text' or 'html'",
		},
		{
			name: "cleanup report markdown format rejected",
			modify: func(c *Config) {
				c.Cleanup.Report.Format = "markdown"
			},
			expectErr: "cleanup.report.format must be 'text' or 'html'",
		},
		{
			name: "heartbeat enabled without url",
			modify: func(c *Config) {
				c.Heartbeat.Enabled = true
				c.Heartbeat.URL = ""
			},
			expectErr: "heartbeat.url must not be empty",
		},
		{
			name: "docker include invalid glob syntax",
			modify: func(c *Config) {
				c.Docker.Include = []string{"api-[unclosed"}
			},
			expectErr: "docker.include invalid",
		},
		{
			name: "docker exclude invalid glob syntax",
			modify: func(c *Config) {
				c.Docker.Exclude = []string{"bad-[range"}
			},
			expectErr: "docker.exclude invalid",
		},
		{
			name: "invalid logs.default_timezone",
			modify: func(c *Config) {
				c.Logs.DefaultTimezone = "Invalid/TZ"
			},
			expectErr: "logs.default_timezone invalid",
		},
		{
			name: "logs.detect.max_scan_bytes <= 0",
			modify: func(c *Config) {
				c.Logs.Detect.MaxScanBytes = 0
			},
			expectErr: "logs.detect.max_scan_bytes must be > 0",
		},
		{
			name: "empty docker.socket when enabled",
			modify: func(c *Config) {
				c.Docker.Socket = ""
			},
			expectErr: "docker.socket must not be empty",
		},
		{
			name: "out of bounds email smtp_port",
			modify: func(c *Config) {
				c.Notifiers.Email.Enabled = true
				c.Notifiers.Email.SMTPHost = "smtp.example.com"
				c.Notifiers.Email.From = "a@b.com"
				c.Notifiers.Email.To = []string{"c@d.com"}
				c.Notifiers.Email.SMTPPort = 0
			},
			expectErr: "notifiers.email.smtp_port must be between",
		},
		{
			name: "empty email to recipients",
			modify: func(c *Config) {
				c.Notifiers.Email.Enabled = true
				c.Notifiers.Email.SMTPHost = "smtp.example.com"
				c.Notifiers.Email.From = "a@b.com"
				c.Notifiers.Email.To = []string{}
				c.Notifiers.Email.SMTPPort = 587
			},
			expectErr: "notifiers.email.to must contain at least one recipient",
		},
		{
			name: "invalid discord webhook URL",
			modify: func(c *Config) {
				c.Notifiers.Discord.Enabled = true
				c.Notifiers.Discord.WebhookURL = "ftp://invalid"
			},
			expectErr: "notifiers.discord.webhook_url must be a valid HTTP(S) URL",
		},
		{
			name: "invalid heartbeat URL",
			modify: func(c *Config) {
				c.Heartbeat.Enabled = true
				c.Heartbeat.URL = "invalid-url"
			},
			expectErr: "heartbeat.url must be a valid HTTP(S) URL",
		},
		{
			name: "cleanup report min_attempts < 1",
			modify: func(c *Config) {
				c.Cleanup.Report.MinAttempts = 0
			},
			expectErr: "cleanup.report.min_attempts must be >= 1",
		},
		{
			name: "cleanup batch_size <= 0",
			modify: func(c *Config) {
				c.Cleanup.BatchSize = 0
			},
			expectErr: "cleanup.batch_size must be > 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValid()
			tc.modify(cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.expectErr)
			}
			if !strings.Contains(err.Error(), tc.expectErr) {
				t.Errorf("expected error containing %q, got %q", tc.expectErr, err.Error())
			}
		})
	}
}

func TestSanitizedConfig(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.ApplyDefaults()
	cfg.Notifiers.Email.Password = "super_secret_smtp_pass"
	cfg.Notifiers.Discord.WebhookURL = "https://discord.com/api/webhooks/12345/abcdef"

	sanitized := cfg.Sanitized()

	// Ensure copy is masked
	if sanitized.Notifiers.Email.Password != "********" {
		t.Errorf("expected sanitized email password '********', got %s", sanitized.Notifiers.Email.Password)
	}
	if sanitized.Notifiers.Discord.WebhookURL != "********" {
		t.Errorf("expected sanitized discord webhook '********', got %s", sanitized.Notifiers.Discord.WebhookURL)
	}

	// Ensure original is not mutated
	if cfg.Notifiers.Email.Password != "super_secret_smtp_pass" {
		t.Errorf("original email password was mutated: %s", cfg.Notifiers.Email.Password)
	}
	if cfg.Notifiers.Discord.WebhookURL != "https://discord.com/api/webhooks/12345/abcdef" {
		t.Errorf("original discord webhook was mutated: %s", cfg.Notifiers.Discord.WebhookURL)
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		input    string
		expected time.Duration
		hasErr   bool
	}{
		{"10s", 10 * time.Second, false},
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"90d", 90 * 24 * time.Hour, false},
		{"", 0, true},
		{"invalid", 0, true},
		{"-5d", 0, true},
	}

	for _, tc := range cases {
		d, err := ParseDuration(tc.input)
		if tc.hasErr && err == nil {
			t.Errorf("expected error for %q, got nil", tc.input)
		}
		if !tc.hasErr && err != nil {
			t.Errorf("unexpected error for %q: %v", tc.input, err)
		}
		if !tc.hasErr && d != tc.expected {
			t.Errorf("for %q expected %v, got %v", tc.input, tc.expected, d)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		input    string
		expected int64
		hasErr   bool
	}{
		{"1024", 1024, false},
		{"1024B", 1024, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"256MiB", 256 * 1024 * 1024, false},
		{"3GiB", 3 * 1024 * 1024 * 1024, false},
		{"8MiB", 8 * 1024 * 1024, false},
		{"512MB", 512 * 1000 * 1000, false},
		{"", 0, true},
		{"-5GiB", 0, true},
		{"invalid", 0, true},
		{"100PB", 0, true},
	}

	for _, tc := range cases {
		b, err := ParseBytes(tc.input)
		if tc.hasErr && err == nil {
			t.Errorf("expected error for %q, got nil", tc.input)
		}
		if !tc.hasErr && err != nil {
			t.Errorf("unexpected error for %q: %v", tc.input, err)
		}
		if !tc.hasErr && b != tc.expected {
			t.Errorf("for %q expected %d, got %d", tc.input, tc.expected, b)
		}
	}
}

func TestResolveConfigPath(t *testing.T) {
	// 1. Explicit CLI flag takes highest precedence
	got := ResolveConfigPath("/custom/path.yaml")
	if got != "/custom/path.yaml" {
		t.Errorf("expected /custom/path.yaml, got %s", got)
	}

	// 2. TARSIER_CONFIG env takes precedence over defaults
	t.Setenv("TARSIER_CONFIG", "/env/tarsier.yaml")
	got = ResolveConfigPath("")
	if got != "/env/tarsier.yaml" {
		t.Errorf("expected /env/tarsier.yaml, got %s", got)
	}

	// 3. Fallback when neither flag nor env is provided
	t.Setenv("TARSIER_CONFIG", "")
	got = ResolveConfigPath("")
	if got != "./tarsier.yaml" && got != "/config/tarsier.yaml" {
		t.Errorf("expected fallback path, got %s", got)
	}
}

func TestListenSIGHUP(t *testing.T) {
	reloaded := make(chan bool, 1)
	sigChan := ListenSIGHUP(func() {
		reloaded <- true
	})
	defer close(sigChan)

	sigChan <- syscall.SIGHUP

	select {
	case <-reloaded:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for SIGHUP callback")
	}
}

func TestApplyDefaultsAllBranches(t *testing.T) {
	// Start with completely zeroed config
	c := &Config{}
	c.ApplyDefaults()

	if c.Server.Listen != ":8080" {
		t.Errorf("expected :8080, got %s", c.Server.Listen)
	}
	if c.Auth.SessionTTL != "24h" {
		t.Errorf("expected 24h, got %s", c.Auth.SessionTTL)
	}
	if c.Storage.Path != "/data" {
		t.Errorf("expected /data, got %s", c.Storage.Path)
	}
	if c.Retention.Metrics1d != "90d" {
		t.Errorf("expected 90d, got %s", c.Retention.Metrics1d)
	}
	if c.Host.Interval != "10s" {
		t.Errorf("expected 10s, got %s", c.Host.Interval)
	}
	if c.Docker.StatsInterval != "10s" {
		t.Errorf("expected 10s, got %s", c.Docker.StatsInterval)
	}
	if c.Logs.Detect.MaxScanBytes != 4096 {
		t.Errorf("expected 4096, got %d", c.Logs.Detect.MaxScanBytes)
	}
	if c.Notifiers.Email.SMTPPort != 587 {
		t.Errorf("expected 587, got %d", c.Notifiers.Email.SMTPPort)
	}
	if c.Cleanup.Schedule != "0 3 * * *" {
		t.Errorf("expected '0 3 * * *', got %s", c.Cleanup.Schedule)
	}
	if c.Cleanup.Report.Format != "text" {
		t.Errorf("expected cleanup report format 'text', got %s", c.Cleanup.Report.Format)
	}
	if c.Cache.MaxSources != 50 {
		t.Errorf("expected 50, got %d", c.Cache.MaxSources)
	}
	if c.Cache.LogTailLines != 150 {
		t.Errorf("expected 150, got %d", c.Cache.LogTailLines)
	}
	if c.Cache.MetricMaxSeries != 2000 {
		t.Errorf("expected 2000, got %d", c.Cache.MetricMaxSeries)
	}
	if c.Cache.MetricRingPoints != 60 {
		t.Errorf("expected 60, got %d", c.Cache.MetricRingPoints)
	}
	if c.Heartbeat.Interval != "5m" {
		t.Errorf("expected 5m, got %s", c.Heartbeat.Interval)
	}
}

func TestValidateCronFieldCases(t *testing.T) {
	cases := []struct {
		field     string
		min, max  int
		expectErr bool
	}{
		{"*", 0, 59, false},
		{"*/15", 0, 59, false},
		{"1-5", 0, 10, false},
		{"1,2,3", 0, 10, false},
		{"1-5/2", 0, 10, false},
		{"", 0, 10, true},
		{"*/0", 0, 10, true},
		{"*/*/*", 0, 10, true},
		{"10-5", 0, 10, true},
		{"15-20", 0, 10, true},
		{"-5", 0, 10, true},
		{"abc", 0, 10, true},
		{"12", 0, 10, true},
	}

	for _, tc := range cases {
		err := validateCronField(tc.field, tc.min, tc.max)
		if tc.expectErr && err == nil {
			t.Errorf("expected error for field %q [%d-%d], got nil", tc.field, tc.min, tc.max)
		}
		if !tc.expectErr && err != nil {
			t.Errorf("unexpected error for field %q [%d-%d]: %v", tc.field, tc.min, tc.max, err)
		}
	}
}
