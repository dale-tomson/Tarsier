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

	"github.com/dale-tomson/Tarsier/internal/api"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
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

	sessionTTL, err := time.ParseDuration(cfg.Auth.SessionTTL)
	if err != nil || sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}

	lockout, err := time.ParseDuration(cfg.Auth.Lockout)
	if err != nil || lockout <= 0 {
		lockout = 15 * time.Minute
	}

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

	// Initialize HTTP API and static asset server
	server, err := api.NewServer(api.ServerConfig{
		Config:    cfg,
		ConfigDB:  store.Config,
		MetricsDB: store.Metrics,
		Auth:      authenticator,
		Sessions:  sessions,
		Limiter:   limiter,
		Version:   Version,
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
