package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/api"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/cleanup"
	"github.com/dale-tomson/Tarsier/internal/collector"
	"github.com/dale-tomson/Tarsier/internal/collector/host"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/docker"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

// runTestConfig validates the configuration at configPath and prints an effective summary.
func runTestConfig(configPath string) {
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

// bootstrapStorage initializes SQLite database stores (config.db and metrics.db).
func bootstrapStorage(ctx context.Context, cfg *config.Config) (*db.Store, func(), error) {
	store, err := db.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize database store at %s: %w", cfg.Storage.Path, err)
	}
	cleanupFn := func() {
		if err := store.Close(); err != nil {
			log.Printf("Error closing database store: %v", err)
		}
	}
	return store, cleanupFn, nil
}

// bootstrapAuth initializes single-admin authentication, session store, and rate limiter.
func bootstrapAuth(cfg *config.Config, store *db.Store) (*auth.Authenticator, *auth.SessionStore, *auth.RateLimiter, error) {
	authenticator, err := auth.NewAuthenticatorFromEnv()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fatal auth configuration error: %w (set TARSIER_ADMIN_PASSWORD_HASH)", err)
	}

	sessionTTL := parseDurationOrDefault(cfg.Auth.SessionTTL, 24*time.Hour)
	lockout := parseDurationOrDefault(cfg.Auth.Lockout, 15*time.Minute)

	sessions, err := auth.NewSessionStore(auth.SessionConfig{
		ConfigDB: store.Config,
		Secret:   authenticator.SessionSecret(),
		TTL:      sessionTTL,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fatal session store error: %w", err)
	}

	limiter := auth.NewRateLimiter(auth.RateLimiterConfig{
		MaxAttempts: cfg.Auth.MaxLoginAttempts,
		Lockout:     lockout,
	})

	return authenticator, sessions, limiter, nil
}

// bootstrapMetrics initializes the metric ingestor queue, in-memory ring buffer, and rollup engine.
func bootstrapMetrics(ctx context.Context, store *db.Store) (*metrics.Ingestor, *metrics.RollupEngine, func(), error) {
	ingestor, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  500,
		FlushInterval: 500 * time.Millisecond,
		QueueCapacity: 4096,
		MaxSeries:     10000,
		RingCapacity:  180,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to initialize metrics ingestor: %w", err)
	}

	cleanupFn := func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer flushCancel()
		if err := ingestor.Close(flushCtx); err != nil {
			log.Printf("Error flushing metrics ingestor: %v", err)
		}
	}

	rollup := metrics.NewRollupEngine(store.Metrics, nil)
	go rollup.Start(ctx)

	return ingestor, rollup, cleanupFn, nil
}

// alertingComponents bundles all alert subsystem handles created during bootstrap.
type alertingComponents struct {
	ruleStore          *alert.RuleStore
	ruleCache          *alert.RuleCache
	stateMachine       *alert.StateMachine
	dispatcher         *alert.Dispatcher
	heartbeatWorker    *alert.HeartbeatWorker
	containerEvaluator *alert.ContainerEvaluator
	logEvaluator       *alert.LogEvaluator
}

// bootstrapAlerting initializes rule storage, cache, state machine, evaluators, and dispatcher.
func bootstrapAlerting(ctx context.Context, cfg *config.Config, store *db.Store, ingestor *metrics.Ingestor) (*alertingComponents, error) {
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

	sourceResolver := alert.SourceResolverFunc(func(sourceID string) string {
		var kind, name string
		err := store.Config.RO.QueryRowContext(ctx,
			"SELECT kind, name FROM source WHERE id = ? OR name = ? LIMIT 1", sourceID, sourceID).
			Scan(&kind, &name)
		if err == nil {
			if kind == "container" {
				return "container:" + name
			}
			return kind + ":" + name
		}
		return sourceID
	})

	logEvaluator := alert.NewLogEvaluator(ruleCache, sourceResolver, stateMachine, nil)
	go logEvaluator.Run(ctx)

	heartbeatWorker, err := alert.NewHeartbeatWorker(cfg.Heartbeat, alert.WithHeartbeatVersion(Version))
	if err != nil {
		log.Printf("Warning: failed to initialize heartbeat worker: %v", err)
	} else {
		go heartbeatWorker.Run(ctx)
	}

	return &alertingComponents{
		ruleStore:          ruleStore,
		ruleCache:          ruleCache,
		stateMachine:       stateMachine,
		dispatcher:         dispatcher,
		heartbeatWorker:    heartbeatWorker,
		containerEvaluator: containerEvaluator,
		logEvaluator:       logEvaluator,
	}, nil
}

// collectorHandles bundles collectors and their lifecycle controls.
type collectorHandles struct {
	dockerConnectedFunc func() bool
	stop                func()
}

// bootstrapCollectors initializes host metrics, docker collection, and host file tailers.
func bootstrapCollectors(
	ctx context.Context,
	cfg *config.Config,
	store *db.Store,
	ingestor *metrics.Ingestor,
	containerEvaluator *alert.ContainerEvaluator,
	logIngestor *logs.LogIngestor,
) *collectorHandles {
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

	// Initialize Docker collector, log followers, and event listener if enabled
	var dockerConnectedFunc func() bool
	var dockerLogManager *docker.DockerLogManager
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

			initList, listErr := dockerClient.ListContainers(ctx, true)
			if listErr == nil {
				_ = docker.SyncContainers(ctx, store.Config, initList, cfg.Cache.MaxSources)
			}

			if logIngestor != nil && (cfg.Docker.CollectLogs == nil || *cfg.Docker.CollectLogs) {
				dockerLogManager = docker.NewLogManager(docker.LogManagerConfig{
					Client:        dockerClient,
					Filter:        dockerFilter,
					ConfigDB:      store.Config,
					Sink:          logIngestor,
					FlushInterval: 3 * time.Second,
					MaxLineBytes:  65536,
				})
				if listErr == nil {
					for _, c := range initList {
						if c.State == "running" {
							dockerLogManager.StartContainer(ctx, c)
						}
					}
				}
				log.Printf("Docker container log manager started")
			}

			eventWatcher := docker.NewEventWatcher(dockerClient, dockerFilter, store.Config)
			dockerConnectedFunc = eventWatcher.Connected

			statsInterval := parseDurationOrDefault(cfg.Docker.StatsInterval, 10*time.Second)

			statsCollector := docker.NewStatsCollector(dockerClient, dockerFilter, ingestor, docker.StatsCollectorOptions{
				Interval:    statsInterval,
				Concurrency: 4,
				MaxSources:  cfg.Cache.MaxSources,
			})

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
						if dockerLogManager != nil {
							dockerLogManager.HandleEvent(ev)
						}
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
	} else {
		dockerConnectedFunc = func() bool { return false }
	}

	// Initialize host log file tailers if configured
	var fileTailerManager *collector.FileTailerManager
	if logIngestor != nil && len(cfg.Logs.Files) > 0 {
		loc, err := time.LoadLocation(cfg.Logs.DefaultTimezone)
		if err != nil {
			loc = time.UTC
		}
		fileTailerManager = collector.NewFileTailerManager(collector.FileTailerConfig{
			Files:          cfg.Logs.Files,
			ConfigDB:       store.Config,
			Sink:           logIngestor,
			MaxSources:     cfg.Cache.MaxSources,
			MaxLineBytes:   65536,
			DefaultTZ:      loc,
			PollInterval:   250 * time.Millisecond,
			RescanInterval: 30 * time.Second,
			FlushInterval:  3 * time.Second,
		})
		fileTailerManager.Start(ctx)
		log.Printf("Host file tailer started (%d configured sources)", len(cfg.Logs.Files))
	}

	stopFn := func() {
		if dockerLogManager != nil {
			dockerLogManager.Stop()
		}
		if fileTailerManager != nil {
			fileTailerManager.Stop()
		}
	}

	return &collectorHandles{
		dockerConnectedFunc: dockerConnectedFunc,
		stop:                stopFn,
	}
}

// cleanupComponents bundles cleanup subsystem objects.
type cleanupComponents struct {
	deliverer     *cleanup.ReportDeliverer
	maintainer    *cleanup.DatabaseMaintainer
	reportBuilder *cleanup.ReportBuilder
	manager       *cleanup.CleanupManager
}

// bootstrapCleanup initializes pre-cleanup report deliverers, builders, maintainers, and manager.
func bootstrapCleanup(ctx context.Context, cfg *config.Config, store *db.Store) (*cleanupComponents, func(), error) {
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

	var stopFn func()
	if err != nil {
		log.Printf("Warning: failed to initialize cleanup manager: %v", err)
		stopFn = func() {}
	} else {
		cleanupMgr.Start(ctx)
		stopFn = func() { cleanupMgr.Stop() }
	}

	return &cleanupComponents{
		deliverer:     deliverer,
		maintainer:    maintainer,
		reportBuilder: reportBuilder,
		manager:       cleanupMgr,
	}, stopFn, nil
}

// logSubsystem bundles all log infrastructure handles.
type logSubsystem struct {
	idxMgr    *logs.IndexManager
	chunkMgr  *logs.ChunkWriterManager
	tailCache *logs.TailCache
	logEngine *logs.LogQueryEngine
	ingestor  *logs.LogIngestor
}

// bootstrapLogs initializes the complete log storage, indexing, tail cache, query engine, and ingestor.
func bootstrapLogs(ctx context.Context, cfg *config.Config, store *db.Store, logEvaluator *alert.LogEvaluator) (*logSubsystem, func(), error) {
	logsDir := filepath.Join(cfg.Storage.Path, "logs")
	if err := os.MkdirAll(logsDir, 0750); err != nil {
		return nil, nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	idxMgr := logs.NewIndexManager(logsDir)

	chunkCfg := logs.ChunkWriterConfig{
		BaseDir:          logsDir,
		BlockTargetBytes: 64 * 1024,
		FlushInterval:    3 * time.Second,
		MaxOpenFiles:     64,
		ZstdLevel:        zstd.SpeedDefault,
		OnBlockFlushed: func(meta logs.BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}
	chunkMgr, err := logs.NewChunkWriterManager(chunkCfg)
	if err != nil {
		_ = idxMgr.Close()
		return nil, nil, fmt.Errorf("failed to initialize chunk writer manager: %w", err)
	}

	tailCache := logs.NewTailCache(cfg.Cache.MaxSources, cfg.Cache.LogTailLines)

	logEngine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir:     logsDir,
		TailCache:   tailCache,
		IndexReader: logs.NewIndexReader(logsDir),
	})
	if err != nil {
		_ = chunkMgr.Close(context.Background())
		_ = idxMgr.Close()
		return nil, nil, fmt.Errorf("failed to initialize log query engine: %w", err)
	}

	ingestor := logs.NewLogIngestor(logs.LogIngestorConfig{
		TailCache:   tailCache,
		ChunkWriter: chunkMgr,
		Evaluator:   logEvaluator,
		QueueCap:    8192,
	})

	cleanupFn := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = ingestor.Close(closeCtx)
		_ = chunkMgr.Close(closeCtx)
		_ = idxMgr.Close()
	}

	return &logSubsystem{
		idxMgr:    idxMgr,
		chunkMgr:  chunkMgr,
		tailCache: tailCache,
		logEngine: logEngine,
		ingestor:  ingestor,
	}, cleanupFn, nil
}

// serverParams bundles arguments required to construct the API server.
type serverParams struct {
	cfg                 *config.Config
	configPath          string
	store               *db.Store
	ingestor            *metrics.Ingestor
	rollup              *metrics.RollupEngine
	dockerConnectedFunc func() bool
	tailCache           *logs.TailCache
	logEngine           *logs.LogQueryEngine
	auth                *auth.Authenticator
	sessions            *auth.SessionStore
	limiter             *auth.RateLimiter
	alerts              *alertingComponents
	cleanup             *cleanupComponents
}

// bootstrapServer builds the HTTP API server.
func bootstrapServer(p serverParams) (*api.Server, error) {
	return api.NewServer(api.ServerConfig{
		Config:          p.cfg,
		ConfigDB:        p.store.Config,
		MetricsDB:       p.store.Metrics,
		HotCache:        p.ingestor.Cache(),
		Rollup:          p.rollup,
		DockerConnected: p.dockerConnectedFunc,
		LogEngine:       p.logEngine,
		TailCache:       p.tailCache,
		Auth:            p.auth,
		Sessions:        p.sessions,
		Limiter:         p.limiter,
		RuleStore:       p.alerts.ruleStore,
		RuleCache:       p.alerts.ruleCache,
		StateMachine:    p.alerts.stateMachine,
		Dispatcher:      p.alerts.dispatcher,
		Heartbeat:       p.alerts.heartbeatWorker,
		CleanupManager:  p.cleanup.manager,
		ReportBuilder:   p.cleanup.reportBuilder,
		Deliverer:       p.cleanup.deliverer,
		Maintainer:      p.cleanup.maintainer,
		ConfigPath:      p.configPath,
		Version:         Version,
	})
}

func parseDurationOrDefault(val string, defaultVal time.Duration) time.Duration {
	d, err := time.ParseDuration(val)
	if err != nil || d <= 0 {
		return defaultVal
	}
	return d
}
