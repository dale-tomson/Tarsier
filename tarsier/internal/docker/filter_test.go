package docker

import (
	"errors"
	"path"
	"reflect"
	"testing"
)

func TestContainerFilter_MatchTable(t *testing.T) {
	type candidateMatch struct {
		name      string
		wantMatch bool
	}

	tests := []struct {
		name         string
		include      []string
		exclude      []string
		wantMode     FilterMode
		wantString   string
		wantPatterns []string
		candidates   []candidateMatch
	}{
		{
			name:         "default match all",
			include:      nil,
			exclude:      nil,
			wantMode:     FilterModeAll,
			wantString:   "all",
			wantPatterns: nil,
			candidates: []candidateMatch{
				{name: "nginx", wantMatch: true},
				{name: "/api-worker-1", wantMatch: true},
				{name: "postgres", wantMatch: true},
				{name: "", wantMatch: false}, // empty string should never match
			},
		},
		{
			name:         "include allowlist",
			include:      []string{"api-*", "postgres"},
			exclude:      nil,
			wantMode:     FilterModeInclude,
			wantString:   "include(2 patterns)",
			wantPatterns: []string{"api-*", "postgres"},
			candidates: []candidateMatch{
				{name: "api-gateway", wantMatch: true},
				{name: "/api-gateway", wantMatch: true},
				{name: "api-", wantMatch: true},
				{name: "postgres", wantMatch: true},
				{name: "postgres-exporter", wantMatch: false},
				{name: "redis", wantMatch: false},
				{name: "", wantMatch: false},
			},
		},
		{
			name:         "exclude denylist",
			include:      nil,
			exclude:      []string{"tarsier", "watchtower", "tmp-*"},
			wantMode:     FilterModeExclude,
			wantString:   "exclude(3 patterns)",
			wantPatterns: []string{"tarsier", "watchtower", "tmp-*"},
			candidates: []candidateMatch{
				{name: "tarsier", wantMatch: false},
				{name: "/tarsier", wantMatch: false},
				{name: "watchtower", wantMatch: false},
				{name: "tmp-job-99", wantMatch: false},
				{name: "nginx", wantMatch: true},
				{name: "api-service", wantMatch: true},
				{name: "", wantMatch: false},
			},
		},
		{
			name:         "include precedence over exclude",
			include:      []string{"api-*"},
			exclude:      []string{"api-internal", "tarsier"},
			wantMode:     FilterModeInclude,
			wantString:   "include(1 patterns)",
			wantPatterns: []string{"api-*"},
			candidates: []candidateMatch{
				{name: "api-internal", wantMatch: true}, // matched by include, exclude ignored
				{name: "api-worker", wantMatch: true},
				{name: "tarsier", wantMatch: false},
				{name: "web", wantMatch: false},
			},
		},
		{
			name:         "leading slash and whitespace normalization",
			include:      []string{" /tarsier ", ""},
			exclude:      nil,
			wantMode:     FilterModeInclude,
			wantString:   "include(1 patterns)",
			wantPatterns: []string{"tarsier"},
			candidates: []candidateMatch{
				{name: "/tarsier", wantMatch: true},
				{name: "tarsier", wantMatch: true},
				{name: "other", wantMatch: false},
			},
		},
		{
			name:         "empty and whitespace patterns normalize to match all",
			include:      []string{" ", "/"},
			exclude:      []string{"", "  "},
			wantMode:     FilterModeAll,
			wantString:   "all",
			wantPatterns: nil,
			candidates: []candidateMatch{
				{name: "nginx", wantMatch: true},
				{name: "", wantMatch: false},
			},
		},
		{
			name:         "wildcard and character class globs",
			include:      []string{"worker-?", "shard-[0-9]"},
			exclude:      nil,
			wantMode:     FilterModeInclude,
			wantString:   "include(2 patterns)",
			wantPatterns: []string{"worker-?", "shard-[0-9]"},
			candidates: []candidateMatch{
				{name: "worker-1", wantMatch: true},
				{name: "worker-a", wantMatch: true},
				{name: "worker-10", wantMatch: false}, // ? matches single char
				{name: "shard-0", wantMatch: true},
				{name: "shard-9", wantMatch: true},
				{name: "shard-x", wantMatch: false}, // outside [0-9]
				{name: "shard-10", wantMatch: false},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filter, err := NewContainerFilter(tc.include, tc.exclude)
			if err != nil {
				t.Fatalf("unexpected error creating filter: %v", err)
			}

			if got := filter.Mode(); got != tc.wantMode {
				t.Errorf("Mode() = %v; want %v", got, tc.wantMode)
			}
			if got := filter.String(); got != tc.wantString {
				t.Errorf("String() = %q; want %q", got, tc.wantString)
			}
			if got := filter.Patterns(); !reflect.DeepEqual(got, tc.wantPatterns) {
				t.Errorf("Patterns() = %v; want %v", got, tc.wantPatterns)
			}

			for _, c := range tc.candidates {
				if got := filter.Match(c.name); got != c.wantMatch {
					t.Errorf("Match(%q) = %v; want %v", c.name, got, c.wantMatch)
				}
			}
		})
	}

	t.Run("nil filter receiver", func(t *testing.T) {
		var filter *ContainerFilter
		if got := filter.Mode(); got != FilterModeAll {
			t.Errorf("Mode() = %v; want %v", got, FilterModeAll)
		}
		if got := filter.String(); got != "all" {
			t.Errorf("String() = %q; want %q", got, "all")
		}
		if got := filter.Patterns(); got != nil {
			t.Errorf("Patterns() = %v; want nil", got)
		}
		if !filter.Match("nginx") {
			t.Errorf("Match(%q) = false; want true", "nginx")
		}
	})
}

func TestContainerFilter_ValidationTable(t *testing.T) {
	t.Run("ValidatePatterns", func(t *testing.T) {
		tests := []struct {
			name     string
			patterns []string
			wantErr  error
		}{
			{
				name:     "nil patterns",
				patterns: nil,
				wantErr:  nil,
			},
			{
				name:     "blank and slash-only entries skipped",
				patterns: []string{"", "   ", "/"},
				wantErr:  nil,
			},
			{
				name:     "valid glob patterns",
				patterns: []string{" /api-* ", "worker-?", "shard-[0-9]"},
				wantErr:  nil,
			},
			{
				name:     "unclosed bracket",
				patterns: []string{"api-[unclosed"},
				wantErr:  path.ErrBadPattern,
			},
			{
				name:     "malformed character range",
				patterns: []string{"ok-*", "invalid-[0-"},
				wantErr:  path.ErrBadPattern,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidatePatterns(tc.patterns)
				if tc.wantErr == nil {
					if err != nil {
						t.Fatalf("ValidatePatterns(%v) unexpected error: %v", tc.patterns, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("ValidatePatterns(%v) = nil; want error wrapping %v", tc.patterns, tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("ValidatePatterns(%v) error = %v; want errors.Is(..., %v)", tc.patterns, err, tc.wantErr)
				}
			})
		}
	})

	t.Run("NewContainerFilter", func(t *testing.T) {
		tests := []struct {
			name    string
			include []string
			exclude []string
			wantErr error
		}{
			{
				name:    "valid include and exclude",
				include: []string{"api-*"},
				exclude: []string{"tarsier"},
				wantErr: nil,
			},
			{
				name:    "invalid glob in include",
				include: []string{"api-[unclosed"},
				exclude: nil,
				wantErr: path.ErrBadPattern,
			},
			{
				name:    "invalid glob in exclude",
				include: nil,
				exclude: []string{"invalid-[0-"},
				wantErr: path.ErrBadPattern,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewContainerFilter(tc.include, tc.exclude)
				if tc.wantErr == nil {
					if err != nil {
						t.Fatalf("NewContainerFilter(%v, %v) unexpected error: %v", tc.include, tc.exclude, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("NewContainerFilter(%v, %v) = nil; want error wrapping %v", tc.include, tc.exclude, tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("NewContainerFilter(%v, %v) error = %v; want errors.Is(..., %v)", tc.include, tc.exclude, err, tc.wantErr)
				}
			})
		}
	})
}

func TestContainerFilter_FilterContainers(t *testing.T) {
	input := []ContainerInfo{
		{ID: "1", Name: "prod-api"},
		{ID: "2", Name: "dev-api"},
		{ID: "3", Name: "prod-worker"},
		{ID: "4", Name: "test-db"},
	}

	tests := []struct {
		name      string
		nilFilter bool
		include   []string
		exclude   []string
		wantNames []string
	}{
		{
			name:      "include allowlist filters slice",
			include:   []string{"prod-*"},
			wantNames: []string{"prod-api", "prod-worker"},
		},
		{
			name:      "exclude denylist filters slice",
			exclude:   []string{"dev-*", "test-db"},
			wantNames: []string{"prod-api", "prod-worker"},
		},
		{
			name:      "match all returns all containers",
			wantNames: []string{"prod-api", "dev-api", "prod-worker", "test-db"},
		},
		{
			name:      "nil filter returns all containers",
			nilFilter: true,
			wantNames: []string{"prod-api", "dev-api", "prod-worker", "test-db"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var filter *ContainerFilter
			if !tc.nilFilter {
				var err error
				filter, err = NewContainerFilter(tc.include, tc.exclude)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			filtered := filter.FilterContainers(input)
			gotNames := make([]string, len(filtered))
			for i, c := range filtered {
				gotNames[i] = c.Name
			}

			if !reflect.DeepEqual(gotNames, tc.wantNames) {
				t.Errorf("FilterContainers() names = %v; want %v", gotNames, tc.wantNames)
			}
		})
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
