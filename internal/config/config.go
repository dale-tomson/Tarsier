package config

// Config represents the complete Tarsier configuration schema.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Auth      AuthConfig      `yaml:"auth"`
	Storage   StorageConfig   `yaml:"storage"`
	Retention RetentionConfig `yaml:"retention"`
	Host      HostConfig      `yaml:"host"`
	Docker    DockerConfig    `yaml:"docker"`
	Logs      LogsConfig      `yaml:"logs"`
	Notifiers NotifiersConfig `yaml:"notifiers"`
	Cleanup   CleanupConfig   `yaml:"cleanup"`
	Cache     CacheConfig     `yaml:"cache"`
	Heartbeat HeartbeatConfig `yaml:"heartbeat"`
}

type ServerConfig struct {
	Listen   string `yaml:"listen"`
	BaseURL  string `yaml:"base_url"`
	Timezone string `yaml:"timezone"`
}

type AuthConfig struct {
	SessionTTL       string `yaml:"session_ttl"`
	MaxLoginAttempts int    `yaml:"max_login_attempts"`
	Lockout          string `yaml:"lockout"`
}

type StorageConfig struct {
	Path           string `yaml:"path"`
	LogsMaxSize    string `yaml:"logs_max_size"`
	MetricsMaxSize string `yaml:"metrics_max_size"`
}

type RetentionConfig struct {
	Logs       string `yaml:"logs"`
	MetricsRaw string `yaml:"metrics_raw"`
	Metrics1m  string `yaml:"metrics_1m"`
	Metrics5m  string `yaml:"metrics_5m"`
	Metrics1d  string `yaml:"metrics_1d"`
}

type HostConfig struct {
	Enabled       *bool    `yaml:"enabled"`
	Interval      string   `yaml:"interval"`
	Disks         []string `yaml:"disks"`
	DiskExcludeFS []string `yaml:"disk_exclude_fs"`
}

type DockerConfig struct {
	Enabled        *bool    `yaml:"enabled"`
	Socket         string   `yaml:"socket"`
	StatsInterval  string   `yaml:"stats_interval"`
	CollectLogs    *bool    `yaml:"collect_logs"`
	CollectMetrics *bool    `yaml:"collect_metrics"`
	Include        []string `yaml:"include"`
	Exclude        []string `yaml:"exclude"`
}

type LogsConfig struct {
	DefaultTimezone string          `yaml:"default_timezone"`
	Detect          LogDetectConfig `yaml:"detect"`
	Files           []LogFileConfig `yaml:"files"`
}

type LogDetectConfig struct {
	MaxScanBytes  int   `yaml:"max_scan_bytes"`
	KeywordLevels *bool `yaml:"keyword_levels"`
	Hints         *bool `yaml:"hints"`
}

type LogFileConfig struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type NotifiersConfig struct {
	Email   EmailNotifierConfig   `yaml:"email"`
	Discord DiscordNotifierConfig `yaml:"discord"`
}

type EmailNotifierConfig struct {
	Enabled  bool     `yaml:"enabled"`
	SMTPHost string   `yaml:"smtp_host"`
	SMTPPort int      `yaml:"smtp_port"`
	Security string   `yaml:"security"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

type DiscordNotifierConfig struct {
	Enabled    bool   `yaml:"enabled"`
	WebhookURL string `yaml:"webhook_url"`
	Mention    string `yaml:"mention"`
}

type CleanupConfig struct {
	Schedule   string              `yaml:"schedule"`
	BatchSize  int                 `yaml:"batch_size"`
	BatchPause string              `yaml:"batch_pause"`
	Report     CleanupReportConfig `yaml:"report"`
}

type CleanupReportConfig struct {
	Enabled                 *bool    `yaml:"enabled"`
	Channels                []string `yaml:"channels"`
	MinAttempts             int      `yaml:"min_attempts"`
	RetryDelay              string   `yaml:"retry_delay"`
	OnFailure               string   `yaml:"on_failure"`
	TopPatterns             int      `yaml:"top_patterns"`
	SampleLinesPerPattern   int      `yaml:"sample_lines_per_pattern"`
	Format                  string   `yaml:"format"`
	Template                string   `yaml:"template"`
	MaxAttachment           string   `yaml:"max_attachment"`
}

type CacheConfig struct {
	MaxSources   int    `yaml:"max_sources"`
	LogTailLines int    `yaml:"log_tail_lines"`
	OverviewTTL  string `yaml:"overview_ttl"`
	QueryTTL     string `yaml:"query_ttl"`
}

type HeartbeatConfig struct {
	Enabled  bool   `yaml:"enabled"`
	URL      string `yaml:"url"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
}

// boolPtr returns a pointer to a boolean value.
func boolPtr(b bool) *bool {
	return &b
}

// NewDefaultConfig returns a Config populated with all default values.
func NewDefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:   ":8080",
			BaseURL:  "",
			Timezone: "UTC",
		},
		Auth: AuthConfig{
			SessionTTL:       "24h",
			MaxLoginAttempts: 5,
			Lockout:          "15m",
		},
		Storage: StorageConfig{
			Path:           "/data",
			LogsMaxSize:    "3GiB",
			MetricsMaxSize: "256MiB",
		},
		Retention: RetentionConfig{
			Logs:       "7d",
			MetricsRaw: "24h",
			Metrics1m:  "7d",
			Metrics5m:  "30d",
			Metrics1d:  "90d",
		},
		Host: HostConfig{
			Enabled:       boolPtr(true),
			Interval:      "10s",
			Disks:         []string{},
			DiskExcludeFS: []string{"tmpfs", "overlay", "squashfs", "devtmpfs"},
		},
		Docker: DockerConfig{
			Enabled:        boolPtr(true),
			Socket:         "/var/run/docker.sock",
			StatsInterval:  "10s",
			CollectLogs:    boolPtr(true),
			CollectMetrics: boolPtr(true),
			Include:        []string{},
			Exclude:        []string{},
		},
		Logs: LogsConfig{
			DefaultTimezone: "",
			Detect: LogDetectConfig{
				MaxScanBytes:  4096,
				KeywordLevels: boolPtr(true),
				Hints:         boolPtr(true),
			},
			Files: []LogFileConfig{},
		},
		Notifiers: NotifiersConfig{
			Email: EmailNotifierConfig{
				Enabled:  false,
				SMTPHost: "",
				SMTPPort: 587,
				Security: "starttls",
				Username: "",
				Password: "",
				From:     "",
				To:       []string{},
			},
			Discord: DiscordNotifierConfig{
				Enabled:    false,
				WebhookURL: "",
				Mention:    "",
			},
		},
		Cleanup: CleanupConfig{
			Schedule:   "0 3 * * *",
			BatchSize:  5000,
			BatchPause: "50ms",
			Report: CleanupReportConfig{
				Enabled:               boolPtr(true),
				Channels:              []string{},
				MinAttempts:           2,
				RetryDelay:            "10m",
				OnFailure:             "proceed",
				TopPatterns:           20,
				SampleLinesPerPattern: 2,
				Format:                "markdown",
				Template:              "",
				MaxAttachment:         "8MiB",
			},
		},
		Cache: CacheConfig{
			MaxSources:   500,
			LogTailLines: 300,
			OverviewTTL:  "3s",
			QueryTTL:     "3s",
		},
		Heartbeat: HeartbeatConfig{
			Enabled:  false,
			URL:      "",
			Interval: "5m",
			Timeout:  "10s",
		},
	}
}

// ApplyDefaults sets default values for fields left unset in unmarshaled config.
func (c *Config) ApplyDefaults() {
	def := NewDefaultConfig()

	if c.Server.Listen == "" {
		c.Server.Listen = def.Server.Listen
	}
	if c.Server.Timezone == "" {
		c.Server.Timezone = def.Server.Timezone
	}

	if c.Auth.SessionTTL == "" {
		c.Auth.SessionTTL = def.Auth.SessionTTL
	}
	if c.Auth.MaxLoginAttempts == 0 {
		c.Auth.MaxLoginAttempts = def.Auth.MaxLoginAttempts
	}
	if c.Auth.Lockout == "" {
		c.Auth.Lockout = def.Auth.Lockout
	}

	if c.Storage.Path == "" {
		c.Storage.Path = def.Storage.Path
	}
	if c.Storage.LogsMaxSize == "" {
		c.Storage.LogsMaxSize = def.Storage.LogsMaxSize
	}
	if c.Storage.MetricsMaxSize == "" {
		c.Storage.MetricsMaxSize = def.Storage.MetricsMaxSize
	}

	if c.Retention.Logs == "" {
		c.Retention.Logs = def.Retention.Logs
	}
	if c.Retention.MetricsRaw == "" {
		c.Retention.MetricsRaw = def.Retention.MetricsRaw
	}
	if c.Retention.Metrics1m == "" {
		c.Retention.Metrics1m = def.Retention.Metrics1m
	}
	if c.Retention.Metrics5m == "" {
		c.Retention.Metrics5m = def.Retention.Metrics5m
	}
	if c.Retention.Metrics1d == "" {
		c.Retention.Metrics1d = def.Retention.Metrics1d
	}

	if c.Host.Enabled == nil {
		c.Host.Enabled = def.Host.Enabled
	}
	if c.Host.Interval == "" {
		c.Host.Interval = def.Host.Interval
	}
	if c.Host.Disks == nil {
		c.Host.Disks = []string{}
	}
	if c.Host.DiskExcludeFS == nil {
		c.Host.DiskExcludeFS = def.Host.DiskExcludeFS
	}

	if c.Docker.Enabled == nil {
		c.Docker.Enabled = def.Docker.Enabled
	}
	if c.Docker.Socket == "" {
		c.Docker.Socket = def.Docker.Socket
	}
	if c.Docker.StatsInterval == "" {
		c.Docker.StatsInterval = def.Docker.StatsInterval
	}
	if c.Docker.CollectLogs == nil {
		c.Docker.CollectLogs = def.Docker.CollectLogs
	}
	if c.Docker.CollectMetrics == nil {
		c.Docker.CollectMetrics = def.Docker.CollectMetrics
	}
	if c.Docker.Include == nil {
		c.Docker.Include = []string{}
	}
	if c.Docker.Exclude == nil {
		c.Docker.Exclude = []string{}
	}

	if c.Logs.Detect.MaxScanBytes == 0 {
		c.Logs.Detect.MaxScanBytes = def.Logs.Detect.MaxScanBytes
	}
	if c.Logs.Detect.KeywordLevels == nil {
		c.Logs.Detect.KeywordLevels = def.Logs.Detect.KeywordLevels
	}
	if c.Logs.Detect.Hints == nil {
		c.Logs.Detect.Hints = def.Logs.Detect.Hints
	}
	if c.Logs.Files == nil {
		c.Logs.Files = []LogFileConfig{}
	}

	if c.Notifiers.Email.SMTPPort == 0 {
		c.Notifiers.Email.SMTPPort = def.Notifiers.Email.SMTPPort
	}
	if c.Notifiers.Email.Security == "" {
		c.Notifiers.Email.Security = def.Notifiers.Email.Security
	}
	if c.Notifiers.Email.To == nil {
		c.Notifiers.Email.To = []string{}
	}

	if c.Cleanup.Schedule == "" {
		c.Cleanup.Schedule = def.Cleanup.Schedule
	}
	if c.Cleanup.BatchSize == 0 {
		c.Cleanup.BatchSize = def.Cleanup.BatchSize
	}
	if c.Cleanup.BatchPause == "" {
		c.Cleanup.BatchPause = def.Cleanup.BatchPause
	}
	if c.Cleanup.Report.Enabled == nil {
		c.Cleanup.Report.Enabled = def.Cleanup.Report.Enabled
	}
	if c.Cleanup.Report.Channels == nil {
		c.Cleanup.Report.Channels = []string{}
	}
	if c.Cleanup.Report.MinAttempts == 0 {
		c.Cleanup.Report.MinAttempts = def.Cleanup.Report.MinAttempts
	}
	if c.Cleanup.Report.RetryDelay == "" {
		c.Cleanup.Report.RetryDelay = def.Cleanup.Report.RetryDelay
	}
	if c.Cleanup.Report.OnFailure == "" {
		c.Cleanup.Report.OnFailure = def.Cleanup.Report.OnFailure
	}
	if c.Cleanup.Report.TopPatterns == 0 {
		c.Cleanup.Report.TopPatterns = def.Cleanup.Report.TopPatterns
	}
	if c.Cleanup.Report.SampleLinesPerPattern == 0 {
		c.Cleanup.Report.SampleLinesPerPattern = def.Cleanup.Report.SampleLinesPerPattern
	}
	if c.Cleanup.Report.Format == "" {
		c.Cleanup.Report.Format = def.Cleanup.Report.Format
	}
	if c.Cleanup.Report.MaxAttachment == "" {
		c.Cleanup.Report.MaxAttachment = def.Cleanup.Report.MaxAttachment
	}

	if c.Cache.MaxSources == 0 {
		c.Cache.MaxSources = def.Cache.MaxSources
	}
	if c.Cache.LogTailLines == 0 {
		c.Cache.LogTailLines = def.Cache.LogTailLines
	}
	if c.Cache.OverviewTTL == "" {
		c.Cache.OverviewTTL = def.Cache.OverviewTTL
	}
	if c.Cache.QueryTTL == "" {
		c.Cache.QueryTTL = def.Cache.QueryTTL
	}

	if c.Heartbeat.Interval == "" {
		c.Heartbeat.Interval = def.Heartbeat.Interval
	}
	if c.Heartbeat.Timeout == "" {
		c.Heartbeat.Timeout = def.Heartbeat.Timeout
	}
}

// Sanitized returns a deep copy of Config with sensitive credentials masked as "********".
func (c *Config) Sanitized() *Config {
	cp := *c

	// Deep copy slices and pointers
	if c.Host.Enabled != nil {
		b := *c.Host.Enabled
		cp.Host.Enabled = &b
	}
	cp.Host.Disks = append([]string(nil), c.Host.Disks...)
	cp.Host.DiskExcludeFS = append([]string(nil), c.Host.DiskExcludeFS...)

	if c.Docker.Enabled != nil {
		b := *c.Docker.Enabled
		cp.Docker.Enabled = &b
	}
	if c.Docker.CollectLogs != nil {
		b := *c.Docker.CollectLogs
		cp.Docker.CollectLogs = &b
	}
	if c.Docker.CollectMetrics != nil {
		b := *c.Docker.CollectMetrics
		cp.Docker.CollectMetrics = &b
	}
	cp.Docker.Include = append([]string(nil), c.Docker.Include...)
	cp.Docker.Exclude = append([]string(nil), c.Docker.Exclude...)

	if c.Logs.Detect.KeywordLevels != nil {
		b := *c.Logs.Detect.KeywordLevels
		cp.Logs.Detect.KeywordLevels = &b
	}
	if c.Logs.Detect.Hints != nil {
		b := *c.Logs.Detect.Hints
		cp.Logs.Detect.Hints = &b
	}
	cp.Logs.Files = append([]LogFileConfig(nil), c.Logs.Files...)

	cp.Notifiers.Email.To = append([]string(nil), c.Notifiers.Email.To...)
	if cp.Notifiers.Email.Password != "" {
		cp.Notifiers.Email.Password = "********"
	}

	if cp.Notifiers.Discord.WebhookURL != "" {
		cp.Notifiers.Discord.WebhookURL = "********"
	}

	if c.Cleanup.Report.Enabled != nil {
		b := *c.Cleanup.Report.Enabled
		cp.Cleanup.Report.Enabled = &b
	}
	cp.Cleanup.Report.Channels = append([]string(nil), c.Cleanup.Report.Channels...)

	return &cp
}
