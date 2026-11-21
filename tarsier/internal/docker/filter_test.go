package docker

import (
	"errors"
	"path"
	"testing"
)

func TestContainerFilter_DefaultMatchAll(t *testing.T) {
	filter, err := NewContainerFilter(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error creating filter: %v", err)
	}

	if filter.Mode() != FilterModeAll {
		t.Errorf("expected FilterModeAll, got %v", filter.Mode())
	}
	if filter.String() != "all" {
		t.Errorf("expected string 'all', got %q", filter.String())
	}

	testCases := []struct {
		name     string
		expected bool
	}{
		{"nginx", true},
		{"/api-worker-1", true},
		{"postgres", true},
		{"", false}, // empty string should never match
	}

	for _, tc := range testCases {
		if got := filter.Match(tc.name); got != tc.expected {
			t.Errorf("Match(%q) = %v; want %v", tc.name, got, tc.expected)
		}
	}
}

func TestContainerFilter_IncludeAllowlist(t *testing.T) {
	filter, err := NewContainerFilter([]string{"api-*", "postgres"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filter.Mode() != FilterModeInclude {
		t.Errorf("expected FilterModeInclude, got %v", filter.Mode())
	}
	if filter.String() != "include(2 patterns)" {
		t.Errorf("expected 'include(2 patterns)', got %q", filter.String())
	}

	testCases := []struct {
		name     string
		expected bool
	}{
		{"api-gateway", true},
		{"/api-gateway", true},
		{"api-", true},
		{"postgres", true},
		{"postgres-exporter", false},
		{"redis", false},
		{"", false},
	}

	for _, tc := range testCases {
		if got := filter.Match(tc.name); got != tc.expected {
			t.Errorf("Match(%q) = %v; want %v", tc.name, got, tc.expected)
		}
	}
}

func TestContainerFilter_ExcludeDenylist(t *testing.T) {
	filter, err := NewContainerFilter(nil, []string{"tarsier", "watchtower", "tmp-*"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filter.Mode() != FilterModeExclude {
		t.Errorf("expected FilterModeExclude, got %v", filter.Mode())
	}
	if filter.String() != "exclude(3 patterns)" {
		t.Errorf("expected 'exclude(3 patterns)', got %q", filter.String())
	}

	testCases := []struct {
		name     string
		expected bool
	}{
		{"tarsier", false},
		{"/tarsier", false},
		{"watchtower", false},
		{"tmp-job-99", false},
		{"nginx", true},
		{"api-service", true},
		{"", false},
	}

	for _, tc := range testCases {
		if got := filter.Match(tc.name); got != tc.expected {
			t.Errorf("Match(%q) = %v; want %v", tc.name, got, tc.expected)
		}
	}
}

func TestContainerFilter_IncludePrecedenceOverExclude(t *testing.T) {
	// When include is non-empty, include takes exclusive precedence
	filter, err := NewContainerFilter([]string{"api-*"}, []string{"api-internal", "tarsier"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filter.Mode() != FilterModeInclude {
		t.Errorf("expected FilterModeInclude, got %v", filter.Mode())
	}

	testCases := []struct {
		name     string
		expected bool
	}{
		{"api-internal", true}, // matched by include, exclude ignored
		{"api-worker", true},
		{"tarsier", false},
		{"web", false},
	}

	for _, tc := range testCases {
		if got := filter.Match(tc.name); got != tc.expected {
			t.Errorf("Match(%q) = %v; want %v", tc.name, got, tc.expected)
		}
	}
}

func TestContainerFilter_LeadingSlashAndWhitespaceNormalization(t *testing.T) {
	filter, err := NewContainerFilter([]string{" /tarsier ", ""}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	patterns := filter.Patterns()
	if len(patterns) != 1 || patterns[0] != "tarsier" {
		t.Fatalf("expected cleaned pattern ['tarsier'], got %v", patterns)
	}

	if !filter.Match("/tarsier") {
		t.Errorf("expected Match('/tarsier') to be true")
	}
	if !filter.Match("tarsier") {
		t.Errorf("expected Match('tarsier') to be true")
	}
}

func TestContainerFilter_WildcardAndCharacterClassGlobs(t *testing.T) {
	filter, err := NewContainerFilter([]string{"worker-?", "shard-[0-9]"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	testCases := []struct {
		name     string
		expected bool
	}{
		{"worker-1", true},
		{"worker-a", true},
		{"worker-10", false}, // ? matches single char
		{"shard-0", true},
		{"shard-9", true},
		{"shard-x", false}, // outside [0-9]
		{"shard-10", false},
	}

	for _, tc := range testCases {
		if got := filter.Match(tc.name); got != tc.expected {
			t.Errorf("Match(%q) = %v; want %v", tc.name, got, tc.expected)
		}
	}
}

func TestContainerFilter_InvalidGlobSyntaxRejection(t *testing.T) {
	// Malformed pattern with unclosed bracket
	_, err := NewContainerFilter([]string{"api-[unclosed"}, nil)
	if err == nil {
		t.Fatal("expected error for unclosed bracket in include, got nil")
	}
	if !errors.Is(err, path.ErrBadPattern) {
		t.Errorf("expected error wrapping path.ErrBadPattern, got: %v", err)
	}

	_, err = NewContainerFilter(nil, []string{"invalid-[0-"})
	if err == nil {
		t.Fatal("expected error for malformed pattern in exclude, got nil")
	}
	if !errors.Is(err, path.ErrBadPattern) {
		t.Errorf("expected error wrapping path.ErrBadPattern, got: %v", err)
	}
}

func TestContainerFilter_FilterContainers(t *testing.T) {
	filter, err := NewContainerFilter([]string{"prod-*"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input := []ContainerInfo{
		{ID: "1", Name: "prod-api"},
		{ID: "2", Name: "dev-api"},
		{ID: "3", Name: "prod-worker"},
		{ID: "4", Name: "test-db"},
	}

	filtered := filter.FilterContainers(input)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(filtered))
	}
	if filtered[0].Name != "prod-api" || filtered[1].Name != "prod-worker" {
		t.Errorf("unexpected filtered results: %v", filtered)
	}
}

func BenchmarkContainerFilter_Match(b *testing.B) {
	filter, err := NewContainerFilter([]string{"api-*", "worker-?", "postgres"}, []string{"tarsier"})
	if err != nil {
		b.Fatalf("unexpected filter error: %v", err)
	}

	candidate := "/api-service-prod"

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !filter.Match(candidate) {
			b.Fatal("expected match")
		}
	}
}
