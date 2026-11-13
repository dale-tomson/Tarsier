package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"
)

// ResolveConfigPath resolves the configuration file path following the precedence:
// 1. Explicit CLI flag (-config)
// 2. TARSIER_CONFIG environment variable
// 3. /config/tarsier.yaml (if file exists)
// 4. ./tarsier.yaml (fallback default)
func ResolveConfigPath(cliFlag string) string {
	if cliFlag != "" {
		return cliFlag
	}

	if envPath := os.Getenv("TARSIER_CONFIG"); envPath != "" {
		return envPath
	}

	containerConfig := "/config/tarsier.yaml"
	if fi, err := os.Stat(containerConfig); err == nil && !fi.IsDir() {
		return containerConfig
	}

	return "./tarsier.yaml"
}

// LoadBytes parses raw YAML configuration bytes with ${VAR} expansion,
// strict unknown field checking, default application, and semantic validation.
func LoadBytes(raw []byte) (*Config, error) {
	// 1. Expand environment variables
	expanded, err := ExpandEnv(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to expand environment variables: %w", err)
	}

	// 2. Decode YAML with strict KnownFields(true)
	cfg := NewDefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil {
		if err == io.EOF {
			// Empty input, apply defaults
			cfg.ApplyDefaults()
			return cfg, Validate(cfg)
		}
		return nil, fmt.Errorf("failed to parse yaml configuration: %w", err)
	}

	// 3. Ensure defaults for omitted fields
	cfg.ApplyDefaults()

	// 4. Validate
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// Load reads a configuration file from the specified path, expands environment variables,
// decodes YAML with strict field checking, applies defaults, and validates the configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
	}
	return LoadBytes(data)
}

// ListenSIGHUP starts a goroutine that triggers the reload callback whenever a SIGHUP signal is received.
func ListenSIGHUP(reload func()) chan os.Signal {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)

	go func() {
		for range sigChan {
			if reload != nil {
				reload()
			}
		}
	}()

	return sigChan
}
