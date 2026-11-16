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
	"runtime"
	"syscall"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
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

	if versionFlag {
		fmt.Printf("Tarsier %s (commit: %s, built: %s, go: %s, target: %s/%s)\n",
			Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	configPath := config.ResolveConfigPath(configFlag)

	if testConfigFlag {
		runTestConfig(configPath)
	}

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, closeStore, err := bootstrapStorage(ctx, cfg)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	defer closeStore()

	authenticator, sessions, limiter, err := bootstrapAuth(cfg, store)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	ingestor, rollup, closeIngestor, err := bootstrapMetrics(ctx, store)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	defer closeIngestor()

	alerting, err := bootstrapAlerting(ctx, cfg, store, ingestor)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	logsSubsystem, closeLogs, err := bootstrapLogs(ctx, cfg, store, alerting.logEvaluator)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	defer closeLogs()

	collectors := bootstrapCollectors(ctx, cfg, store, ingestor, alerting.containerEvaluator, logsSubsystem.ingestor)
	defer collectors.stop()

	cleanupSubsystem, stopCleanup, err := bootstrapCleanup(ctx, cfg, store)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	defer stopCleanup()

	server, err := bootstrapServer(serverParams{
		cfg:                 cfg,
		configPath:          configPath,
		store:               store,
		ingestor:            ingestor,
		rollup:              rollup,
		dockerConnectedFunc: collectors.dockerConnectedFunc,
		tailCache:           logsSubsystem.tailCache,
		logEngine:           logsSubsystem.logEngine,
		auth:                authenticator,
		sessions:            sessions,
		limiter:             limiter,
		alerts:              alerting,
		cleanup:             cleanupSubsystem,
	})
	if err != nil {
		log.Fatalf("Fatal: failed to initialize HTTP server: %v", err)
	}

	go func() {
		log.Printf("HTTP server listening on %s (timezone: %s)", sanitized.Server.Listen, sanitized.Server.Timezone)
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Fatal: HTTP server failed: %v", err)
		}
	}()

	config.ListenSIGHUP(func() {
		log.Printf("Received SIGHUP, reloading configuration from %s...", configPath)
		reloaded, err := config.Load(configPath)
		if err != nil {
			log.Printf("Error reloading configuration: %v (retaining previous configuration)", err)
			return
		}
		cfg = reloaded
		if alerting.dispatcher != nil {
			alerting.dispatcher.Reload(reloaded.Notifiers)
		}
		if alerting.heartbeatWorker != nil {
			_ = alerting.heartbeatWorker.Reload(reloaded.Heartbeat)
		}
		log.Printf("Configuration successfully reloaded")
	})

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
