package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

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
	log.Printf("Server listening on %s (timezone: %s)", sanitized.Server.Listen, sanitized.Server.Timezone)
	log.Printf("Storage directory: %s (logs cap: %s, metrics cap: %s)",
		sanitized.Storage.Path, sanitized.Storage.LogsMaxSize, sanitized.Storage.MetricsMaxSize)

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
}
