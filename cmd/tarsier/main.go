package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/api"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/cleanup"
	"github.com/dale-tomson/Tarsier/internal/collector/host"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/docker"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// Build metadata injected via -ldflags during compilation.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	var (
		configFlag     string
		testConfigFlag bool
		versionFlag    bool
	)

	flag.StringVar(&configFlag, "config", "", "Path to tarsier configuration file")
	flag.BoolVar(&testConfigFlag, "test-config", false, "Validate configuration file and exit")
	flag.BoolVar(&versionFlag, "version", false, "Print version and build metadata and exit")
	flag.Parse()

	// Handle -version flag
	if versionFlag {
		fmt.Printf("Tarsier %s (commit: %s, built: %s, go: %s, target: %s/%s)\n",
			Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	configPath := config.ResolveConfigPath(configFlag)

	// Handle -test-config flag
	if testConfigFlag {
		fmt.Printf("Testing configuration at %s...\n", configPath)
		cfg, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
			os.Exit(1)
		}
		sanitized := cfg.Sanitized()
		fmt.Printf("Configuration is valid! Effective non-sensitive summary:\n")
		fmt.Printf("  Server: listen=%s, timezone=%s\n", sanitized.Server.Listen, sanitized.Server.Timezone)
		fmt.Printf("  Storage: path=%s, logs_max=%s, metrics_max=%s\n",
			sanitized.Storage.Path, sanitized.Storage.LogsMaxSize, sanitized.Storage.MetricsMaxSize)
		fmt.Printf("  Retention: logs=%s, metrics_raw=%s, metrics_1d=%s\n",
			sanitized.Retention.Logs, sanitized.Retention.MetricsRaw, sanitized.Retention.Metrics1d)
		os.Exit(0)
	}

	// Normal startup
	log.Printf("Starting Tarsier %s (commit: %s, built: %s, go: %s, target: %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Fatal: failed to load configuration from %s: %v", configPath, err)
	}

	sanitized := cfg.Sanitized()
	log.Printf("Configuration loaded successfully from %s", configPath)
	log.Printf("Storage directory: %s (logs cap: %s, metrics cap: %s)",
		sanitized.Storage.Path, sanitized.Storage.LogsMaxSize, sanitized.Storage.MetricsMaxSize)

	// Initialize SQLite database stores (config.db and metrics.db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := db.Open(ctx, cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Fatal: failed to initialize database store at %s: %v", cfg.Storage.Path, err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("Error closing database store: %v", err)
		}
	}()

	// Initialize single-admin authentication and session store
	authenticator, err := auth.NewAuthenticatorFromEnv()
	if err != nil {
		log.Fatalf("Fatal auth configuration error: %v (set TARSIER_ADMIN_PASSWORD_HASH)", err)
	}

	sessionTTL := parseDurationOrDefault(cfg.Auth.SessionTTL, 24*time.Hour)
	lockout := parseDurationOrDefault(cfg.Auth.Lockout, 15*time.Minute)

	sessions, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   authenticator.SessionSecret(),
		TTL:      sessionTTL,
	})
	if err != nil {
		log.Fatalf("Fatal session store error: %v", err)
	}

	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: cfg.Auth.MaxLoginAttempts,
		Lockout:     lockout,
	})

	// Initialize metrics ingestion pipeline & in-memory hot cache
	ingestor, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  500,
		FlushInterval: 500 * time.Millisecond,
		QueueCapacity: 4096,
		MaxSeries:     10000,
		RingCapacity:  180,
	})
	if err != nil {
		log.Fatalf("Fatal: failed to initialize metrics ingestor: %v", err)
	}
	defer func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer flushCancel()
		if err := ingestor.Close(flushCtx); err != nil {
			log.Printf("Error flushing metrics ingestor: %v", err)
		}
	}()

	// Initialize background metric rollup downsampler (1m, 5m, 1d)
	rollup := metrics.NewRollupEngine(store.Metrics, nil)
	go rollup.Start(ctx)

	// Initialize Alerting Engine (Dispatcher, RuleStore, RuleCache, StateMachine, Evaluators)
	dispatcher, err := alert.NewDispatcher(cfg.Notifiers, store)
	if err != nil {
		log.Printf("Warning: failed to initialize alert dispatcher: %v", err)
	}
	ruleStore := alert.NewRuleStore(store.Config)
	ruleCache := alert.NewRuleCache(ruleStore)
	if err := ruleCache.Reload(ctx); err != nil {
		log.Printf("Warning: failed initial alert rules cache reload: %v", err)
	}

	stateMachine := alert.NewStateMachine(ruleCache, ruleStore, store.Config, dispatcher, nil)
	if err := stateMachine.Restore(ctx); err != nil {
		log.Printf("Warning: failed to restore active alert states: %v", err)
	}

	metricEvaluator := alert.NewMetricEvaluator(ruleCache, ingestor.Cache(), stateMachine, nil)
	go metricEvaluator.Run(ctx)

	containerEvaluator := alert.NewContainerEvaluator(ruleCache, stateMachine, nil)
	go containerEvaluator.Run(ctx)

	logEvaluator := alert.NewLogEvaluator(ruleCache, nil, stateMachine, nil)
	go logEvaluator.Run(ctx)

	heartbeatWorker, err := alert.NewHeartbeatWorker(cfg.Heartbeat, alert.WithHeartbeatVersion(Version))
	if err != nil {
		log.Printf("Warning: failed to initialize heartbeat worker: %v", err)
	} else {
		go heartbeatWorker.Run(ctx)
	}

	// Start host collector if enabled
	if cfg.Host.Enabled == nil || *cfg.Host.Enabled {
		hostChan := make(chan model.MetricSample, 4096)
		hostCollector, err := host.NewCollector(cfg.Host, hostChan)
		if err != nil {
			log.Printf("Warning: failed to initialize host collector: %v", err)
		} else {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case sample, ok := <-hostChan:
						if !ok {
							return
						}
						ingestor.Push(sample)
					}
				}
			}()
			go hostCollector.Run(ctx)
			log.Printf("Host collector started (interval: %s)", cfg.Host.Interval)
		}
	}

	// Initialize Docker collector and event listener if enabled
	var dockerConnectedFunc func() bool
	if cfg.Docker.Enabled == nil || *cfg.Docker.Enabled {
		dockerClient := docker.NewClient(cfg.Docker.Socket)
		dockerFilter, err := docker.NewContainerFilter(cfg.Docker.Include, cfg.Docker.Exclude)
		if err != nil {
			log.Printf("Warning: invalid docker filter patterns: %v", err)
		}

		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		pingErr := dockerClient.Ping(pingCtx)
		pingCancel()

		if pingErr != nil {
			log.Printf("Warning: Docker socket %s unavailable: %v (degrading to host-only monitoring)", cfg.Docker.Socket, pingErr)
			dockerConnectedFunc = func() bool { return false }
		} else {
			log.Printf("Docker daemon connected at %s (mode: %s)", cfg.Docker.Socket, dockerFilter)

			// Initial discovery and sync into config.db
			initList, listErr := dockerClient.ListContainers(ctx, true)
			if listErr == nil {
				_ = docker.SyncContainers(ctx, store.Config, initList, cfg.Cache.MaxSources)
			}

			eventWatcher := docker.NewEventWatcher(dockerClient, dockerFilter, store.Config)
			dockerConnectedFunc = eventWatcher.Connected

			statsInterval := parseDurationOrDefault(cfg.Docker.StatsInterval, 10*time.Second)

			statsCollector := docker.NewStatsCollector(dockerClient, dockerFilter, ingestor, docker.StatsCollectorOptions{
				Interval:    statsInterval,
				Concurrency: 4,
				MaxSources:  cfg.Cache.MaxSources,
			})

			// Route lifecycle events from watcher into stats collector and container alert evaluator
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case ev, ok := <-eventWatcher.Events():
						if !ok {
							return
						}
						statsCollector.HandleEvent(ev)
						containerEvaluator.OnContainerEvent(ctx, ev)
					}
				}
			}()

			go func() {
				if err := eventWatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("Docker event watcher terminated: %v", err)
				}
			}()

			if cfg.Docker.CollectMetrics == nil || *cfg.Docker.CollectMetrics {
				go statsCollector.Run(ctx)
				log.Printf("Docker stats collector started (interval: %s)", statsInterval)
			}
		}
	}

	// Initialize in-memory hot log tail cache and unified log query engine
	tailCache := logs.NewTailCache(cfg.Cache.MaxSources, cfg.Cache.LogTailLines)
	logEngine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir:   filepath.Join(cfg.Storage.Path, "logs"),
		TailCache: tailCache,
	})
	if err != nil {
		log.Fatalf("Fatal: failed to initialize log query engine: %v", err)
	}

	// Initialize Cleanup & Retention subsystem
	var cleanupSenders []cleanup.ReportSender
	if cfg.Notifiers.Discord.WebhookURL != "" {
		cleanupSenders = append(cleanupSenders, cleanup.NewDiscordReportSender(cfg.Notifiers.Discord, nil))
	}
	if cfg.Notifiers.Email.SMTPHost != "" {
		cleanupSenders = append(cleanupSenders, cleanup.NewSMTPReportSender(cfg.Notifiers.Email))
	}
	deliverer := cleanup.NewReportDeliverer(cfg.Cleanup.Report, cleanupSenders)
	maintainer := cleanup.NewDatabaseMaintainer(cleanup.DatabaseMaintainerConfig{
		Store: store,
	})
	tmplEngine, _ := cleanup.NewTemplateEngine(cfg.Cleanup.Report.Template, "")
	reportBuilder, err := cleanup.NewReportBuilder(cleanup.ReportBuilderConfig{
		DataDir:   cfg.Storage.Path,
		MetricsRO: store.Metrics.RO,
		ConfigRO:  store.Config.RO,
	})
	if err != nil {
		log.Printf("Warning: failed to initialize cleanup report builder: %v", err)
	}
	cleanupMgr, err := cleanup.NewCleanupManager(cleanup.CleanupManagerConfig{
		Config:         cfg,
		Store:          store,
		ReportBuilder:  reportBuilder,
		TemplateEngine: tmplEngine,
		Deliverer:      deliverer,
		Maintainer:     maintainer,
	})
	if err != nil {
		log.Printf("Warning: failed to initialize cleanup manager: %v", err)
	} else {
		cleanupMgr.Start(ctx)
		defer cleanupMgr.Stop()
	}

	// Initialize HTTP API and static asset server
	server, err := api.NewServer(api.ServerConfig{
		Config:          cfg,
		ConfigDB:        store.Config,
		MetricsDB:       store.Metrics,
		HotCache:        ingestor.Cache(),
		Rollup:          rollup,
		DockerConnected: dockerConnectedFunc,
		LogEngine:       logEngine,
		TailCache:       tailCache,
		Auth:            authenticator,
		Sessions:        sessions,
		Limiter:         limiter,
		RuleStore:       ruleStore,
		RuleCache:       ruleCache,
		StateMachine:    stateMachine,
		Dispatcher:      dispatcher,
		Heartbeat:       heartbeatWorker,
		CleanupManager:  cleanupMgr,
		ReportBuilder:   reportBuilder,
		Deliverer:       deliverer,
		Maintainer:      maintainer,
		ConfigPath:      configPath,
		Version:         Version,
	})
	if err != nil {
		log.Fatalf("Fatal: failed to initialize HTTP server: %v", err)
	}

	// Start HTTP server in background goroutine
	go func() {
		log.Printf("HTTP server listening on %s (timezone: %s)", sanitized.Server.Listen, sanitized.Server.Timezone)
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Fatal: HTTP server failed: %v", err)
		}
	}()

	// Listen for SIGHUP reload
	config.ListenSIGHUP(func() {
		log.Printf("Received SIGHUP, reloading configuration from %s...", configPath)
		reloaded, err := config.Load(configPath)
		if err != nil {
			log.Printf("Error reloading configuration: %v (retaining previous configuration)", err)
			return
		}
		cfg = reloaded
		if dispatcher != nil {
			dispatcher.Reload(reloaded.Notifiers)
		}
		if heartbeatWorker != nil {
			_ = heartbeatWorker.Reload(reloaded.Heartbeat)
		}
		log.Printf("Configuration successfully reloaded")
	})

	// Wait for OS shutdown signals
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	log.Printf("Tarsier engine ready. Press Ctrl+C or send SIGTERM to shutdown.")
	sig := <-stopChan
	log.Printf("Received termination signal (%s), shutting down cleanly...", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown warning: %v", err)
	}
	log.Printf("Tarsier stopped cleanly")
}

func parseDurationOrDefault(val string, defaultVal time.Duration) time.Duration {
	d, err := time.ParseDuration(val)
	if err != nil || d <= 0 {
		return defaultVal
	}
	return d
}
