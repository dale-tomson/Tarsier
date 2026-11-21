package api

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPISpecValidation(t *testing.T) {
	specPath := filepath.Join("..", "..", "..", "docs", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("failed to read openapi.yaml at %s: %v", specPath, err)
	}

	var parsed struct {
		OpenAPI string                            `yaml:"openapi"`
		Info    map[string]interface{}            `yaml:"info"`
		Paths   map[string]map[string]interface{} `yaml:"paths"`
	}

	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal openapi.yaml: %v", err)
	}

	if parsed.OpenAPI != "3.1.0" {
		t.Errorf("expected openapi version 3.1.0, got %s", parsed.OpenAPI)
	}

	// Verify key endpoints exist in the spec
	requiredEndpoints := []string{
		"/healthz",
		"/api/v1/auth/login",
		"/api/v1/auth/logout",
		"/api/v1/auth/me",
		"/api/v1/system/config",
		"/api/v1/system/health",
		"/api/v1/system/reload",
		"/api/v1/host/metrics",
		"/api/v1/metrics/daily/summary",
		"/api/v1/metrics/daily",
		"/api/v1/containers",
		"/api/v1/containers/{name}",
		"/api/v1/sources",
		"/api/v1/logs",
		"/api/v1/logs/context",
		"/api/v1/alerts/active",
		"/api/v1/alerts/rules",
		"/api/v1/alerts/rules/{id}",
		"/api/v1/alerts/templates",
		"/api/v1/alerts/rules/preview",
		"/api/v1/alerts/rules/export",
		"/api/v1/alerts/rules/import",
		"/api/v1/alerts/history",
		"/api/v1/alerts/test",
		"/api/v1/system/cleanup/history",
		"/api/v1/system/cleanup/{id}/report",
		"/api/v1/system/cleanup/run",
		"/api/v1/system/cleanup/template",
		"/api/v1/system/cleanup/preview",
	}

	for _, ep := range requiredEndpoints {
		if _, ok := parsed.Paths[ep]; !ok {
			t.Errorf("missing endpoint in openapi spec: %s", ep)
		}
	}
}
