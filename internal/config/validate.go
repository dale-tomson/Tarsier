package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var byteRegex = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

// ParseDuration parses duration strings including Go durations (e.g. "10s", "24h")
// and day suffixes (e.g. "7d", "30d", "90d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration string")
	}

	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		days, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		if days < 0 {
			return 0, fmt.Errorf("duration cannot be negative: %s", s)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration cannot be negative: %s", s)
	}
	return d, nil
}

// ParseBytes parses byte size strings (e.g. "256MiB", "3GiB", "8MiB", "512MB", "1024B").
// Supports SI and IEC prefixes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty byte size string")
	}

	matches := byteRegex.FindStringSubmatch(s)
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid byte size format %q", s)
	}

	num, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in byte size %q: %w", s, err)
	}
	if num <= 0 {
		return 0, fmt.Errorf("byte size must be positive: %q", s)
	}

	unit := strings.ToUpper(matches[2])
	var multiplier float64

	switch unit {
	case "", "B":
		multiplier = 1
	case "K", "KB":
		multiplier = 1e3
	case "KI", "KIB":
		multiplier = 1024
	case "M", "MB":
		multiplier = 1e6
	case "MI", "MIB":
		multiplier = 1024 * 1024
	case "G", "GB":
		multiplier = 1e9
	case "GI", "GIB":
		multiplier = 1024 * 1024 * 1024
	case "T", "TB":
		multiplier = 1e12
	case "TI", "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unrecognized byte size unit %q in %q", matches[2], s)
	}

	return int64(num * multiplier), nil
}

// Validate checks the configuration for semantic correctness and boundary constraints.
func Validate(c *Config) error {
	var errs []string

	// Server validation
	if strings.TrimSpace(c.Server.Listen) == "" {
		errs = append(errs, "server.listen must not be empty")
	}
	if c.Server.Timezone != "" {
		if _, err := time.LoadLocation(c.Server.Timezone); err != nil {
			errs = append(errs, fmt.Sprintf("server.timezone invalid %q: %v", c.Server.Timezone, err))
		}
	}
	if c.Server.BaseURL != "" {
		if u, err := url.Parse(c.Server.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Sprintf("server.base_url must be a valid absolute URL: %q", c.Server.BaseURL))
		}
	}

	// Auth validation
	if err := validatePositiveDuration("auth.session_ttl", c.Auth.SessionTTL); err != nil {
		errs = append(errs, err.Error())
	}
	if c.Auth.MaxLoginAttempts <= 0 {
		errs = append(errs, "auth.max_login_attempts must be > 0")
	}
	if err := validatePositiveDuration("auth.lockout", c.Auth.Lockout); err != nil {
		errs = append(errs, err.Error())
	}

	// Storage validation
	if strings.TrimSpace(c.Storage.Path) == "" {
		errs = append(errs, "storage.path must not be empty")
	}
	if _, err := ParseBytes(c.Storage.LogsMaxSize); err != nil {
		errs = append(errs, fmt.Sprintf("storage.logs_max_size invalid: %v", err))
	}
	if _, err := ParseBytes(c.Storage.MetricsMaxSize); err != nil {
		errs = append(errs, fmt.Sprintf("storage.metrics_max_size invalid: %v", err))
	}

	// Retention validation
	if err := validatePositiveDuration("retention.logs", c.Retention.Logs); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("retention.metrics_raw", c.Retention.MetricsRaw); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("retention.metrics_1m", c.Retention.Metrics1m); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("retention.metrics_5m", c.Retention.Metrics5m); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("retention.metrics_1d", c.Retention.Metrics1d); err != nil {
		errs = append(errs, err.Error())
	}

	// Host validation
	if err := validatePositiveDuration("host.interval", c.Host.Interval); err != nil {
		errs = append(errs, err.Error())
	}

	// Docker validation
	if c.Docker.Enabled != nil && *c.Docker.Enabled {
		if strings.TrimSpace(c.Docker.Socket) == "" {
			errs = append(errs, "docker.socket must not be empty when docker is enabled")
		}
	}
	if err := validatePositiveDuration("docker.stats_interval", c.Docker.StatsInterval); err != nil {
		errs = append(errs, err.Error())
	}

	// Logs validation
	if c.Logs.DefaultTimezone != "" {
		if _, err := time.LoadLocation(c.Logs.DefaultTimezone); err != nil {
			errs = append(errs, fmt.Sprintf("logs.default_timezone invalid %q: %v", c.Logs.DefaultTimezone, err))
		}
	}
	if c.Logs.Detect.MaxScanBytes <= 0 {
		errs = append(errs, "logs.detect.max_scan_bytes must be > 0")
	}
	for i, f := range c.Logs.Files {
		if strings.TrimSpace(f.Name) == "" {
			errs = append(errs, fmt.Sprintf("logs.files[%d].name must not be empty", i))
		}
		if strings.TrimSpace(f.Path) == "" {
			errs = append(errs, fmt.Sprintf("logs.files[%d].path must not be empty", i))
		}
	}

	// Notifiers validation
	if c.Notifiers.Email.Enabled {
		if strings.TrimSpace(c.Notifiers.Email.SMTPHost) == "" {
			errs = append(errs, "notifiers.email.smtp_host must not be empty when email is enabled")
		}
		if c.Notifiers.Email.SMTPPort <= 0 || c.Notifiers.Email.SMTPPort > 65535 {
			errs = append(errs, "notifiers.email.smtp_port must be between 1 and 65535")
		}
		sec := strings.ToLower(strings.TrimSpace(c.Notifiers.Email.Security))
		if sec != "none" && sec != "starttls" && sec != "tls" {
			errs = append(errs, fmt.Sprintf("notifiers.email.security must be 'none', 'starttls', or 'tls', got %q", c.Notifiers.Email.Security))
		}
		if strings.TrimSpace(c.Notifiers.Email.From) == "" {
			errs = append(errs, "notifiers.email.from must not be empty when email is enabled")
		}
		if len(c.Notifiers.Email.To) == 0 {
			errs = append(errs, "notifiers.email.to must contain at least one recipient when email is enabled")
		}
	}

	if c.Notifiers.Discord.Enabled {
		if strings.TrimSpace(c.Notifiers.Discord.WebhookURL) == "" {
			errs = append(errs, "notifiers.discord.webhook_url must not be empty when discord is enabled")
		} else {
			u, err := url.Parse(c.Notifiers.Discord.WebhookURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Sprintf("notifiers.discord.webhook_url must be a valid HTTP(S) URL: %q", c.Notifiers.Discord.WebhookURL))
			}
		}
	}

	// Cleanup validation
	if err := validateCron5(c.Cleanup.Schedule); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup.schedule invalid: %v", err))
	}
	if c.Cleanup.BatchSize <= 0 {
		errs = append(errs, "cleanup.batch_size must be > 0")
	}
	if err := validatePositiveDuration("cleanup.batch_pause", c.Cleanup.BatchPause); err != nil {
		errs = append(errs, err.Error())
	}

	// Cleanup Report validation
	if err := validatePositiveDuration("cleanup.report.retry_delay", c.Cleanup.Report.RetryDelay); err != nil {
		errs = append(errs, err.Error())
	}
	if c.Cleanup.Report.MinAttempts < 1 {
		errs = append(errs, "cleanup.report.min_attempts must be >= 1")
	}
	onFail := strings.ToLower(strings.TrimSpace(c.Cleanup.Report.OnFailure))
	if onFail != "proceed" && onFail != "block" {
		errs = append(errs, fmt.Sprintf("cleanup.report.on_failure must be 'proceed' or 'block', got %q", c.Cleanup.Report.OnFailure))
	}
	fmtLower := strings.ToLower(strings.TrimSpace(c.Cleanup.Report.Format))
	if fmtLower != "markdown" && fmtLower != "html" {
		errs = append(errs, fmt.Sprintf("cleanup.report.format must be 'markdown' or 'html', got %q", c.Cleanup.Report.Format))
	}
	if _, err := ParseBytes(c.Cleanup.Report.MaxAttachment); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup.report.max_attachment invalid: %v", err))
	}

	// Cache validation
	if c.Cache.MaxSources < 1 || c.Cache.MaxSources > 10000 {
		errs = append(errs, fmt.Sprintf("cache.max_sources must be between 1 and 10000, got %d", c.Cache.MaxSources))
	}
	if c.Cache.LogTailLines < 50 || c.Cache.LogTailLines > 2000 {
		errs = append(errs, fmt.Sprintf("cache.log_tail_lines must be between 50 and 2000, got %d", c.Cache.LogTailLines))
	}
	if err := validatePositiveDuration("cache.overview_ttl", c.Cache.OverviewTTL); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("cache.query_ttl", c.Cache.QueryTTL); err != nil {
		errs = append(errs, err.Error())
	}

	// Heartbeat validation
	if c.Heartbeat.Enabled {
		if strings.TrimSpace(c.Heartbeat.URL) == "" {
			errs = append(errs, "heartbeat.url must not be empty when heartbeat is enabled")
		} else {
			u, err := url.Parse(c.Heartbeat.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Sprintf("heartbeat.url must be a valid HTTP(S) URL: %q", c.Heartbeat.URL))
			}
		}
	}
	if err := validatePositiveDuration("heartbeat.interval", c.Heartbeat.Interval); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validatePositiveDuration("heartbeat.timeout", c.Heartbeat.Timeout); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func validatePositiveDuration(field, val string) error {
	d, err := ParseDuration(val)
	if err != nil {
		return fmt.Errorf("%s invalid: %w", field, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be a positive duration, got %s", field, val)
	}
	return nil
}

// validateCron5 validates a standard 5-field cron expression:
// minute (0-59), hour (0-23), day of month (1-31), month (1-12), day of week (0-7).
func validateCron5(expr string) error {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return fmt.Errorf("cron expression must have exactly 5 fields, got %d in %q", len(fields), expr)
	}

	ranges := []struct {
		min, max int
		name     string
	}{
		{0, 59, "minute"},
		{0, 23, "hour"},
		{1, 31, "day of month"},
		{1, 12, "month"},
		{0, 7, "day of week"},
	}

	for i, f := range fields {
		if err := validateCronField(f, ranges[i].min, ranges[i].max); err != nil {
			return fmt.Errorf("invalid %s field %q: %w", ranges[i].name, f, err)
		}
	}
	return nil
}

func validateCronField(field string, minVal, maxVal int) error {
	// Comma-separated parts
	parts := strings.Split(field, ",")
	for _, part := range parts {
		if part == "" {
			return errors.New("empty sub-expression")
		}

		// Check for step: e.g. */5 or 1-10/2
		stepParts := strings.Split(part, "/")
		if len(stepParts) > 2 {
			return fmt.Errorf("multiple step characters in %q", part)
		}

		if len(stepParts) == 2 {
			step, err := strconv.Atoi(stepParts[1])
			if err != nil || step <= 0 {
				return fmt.Errorf("invalid step value %q", stepParts[1])
			}
			part = stepParts[0]
		}

		if part == "*" {
			continue
		}

		// Check for range: e.g. 1-5
		if rangeParts := strings.Split(part, "-"); len(rangeParts) == 2 {
			start, err1 := strconv.Atoi(rangeParts[0])
			end, err2 := strconv.Atoi(rangeParts[1])
			if err1 != nil || err2 != nil {
				return fmt.Errorf("invalid range values in %q", part)
			}
			if start < minVal || start > maxVal || end < minVal || end > maxVal || start > end {
				return fmt.Errorf("range %d-%d out of bounds [%d-%d]", start, end, minVal, maxVal)
			}
			continue
		}

		// Single integer
		val, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("invalid integer %q", part)
		}
		if val < minVal || val > maxVal {
			return fmt.Errorf("value %d out of bounds [%d-%d]", val, minVal, maxVal)
		}
	}
	return nil
}
