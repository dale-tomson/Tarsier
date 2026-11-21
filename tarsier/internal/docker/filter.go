package docker

import (
	"fmt"
	"path"
	"strings"
)

// FilterMode defines how container names are evaluated against glob patterns.
type FilterMode int

const (
	// FilterModeAll matches all containers (neither include nor exclude specified).
	FilterModeAll FilterMode = iota

	// FilterModeInclude matches only containers matching at least one include pattern.
	FilterModeInclude

	// FilterModeExclude matches all containers except those matching at least one exclude pattern.
	FilterModeExclude
)

// ContainerFilter provides fast, zero-allocation container name filtering based on glob patterns.
type ContainerFilter struct {
	mode     FilterMode
	patterns []string
}

// ValidatePatterns verifies that all glob patterns in the given slice are syntactically valid.
func ValidatePatterns(patterns []string) error {
	for _, p := range patterns {
		clean := strings.TrimPrefix(strings.TrimSpace(p), "/")
		if clean == "" {
			continue
		}
		if _, err := path.Match(clean, "probe"); err != nil {
			return fmt.Errorf("invalid docker glob pattern %q: %w", p, err)
		}
	}
	return nil
}

// NewContainerFilter builds and pre-validates a ContainerFilter.
// If include is populated, include patterns take exclusive precedence (allowlist).
// If include is empty and exclude is populated, exclude patterns govern (denylist).
// If both are empty, all containers are matched.
func NewContainerFilter(include, exclude []string) (*ContainerFilter, error) {
	if err := ValidatePatterns(include); err != nil {
		return nil, err
	}
	if err := ValidatePatterns(exclude); err != nil {
		return nil, err
	}

	cleanInclude := make([]string, 0, len(include))
	for _, p := range include {
		clean := strings.TrimPrefix(strings.TrimSpace(p), "/")
		if clean != "" {
			cleanInclude = append(cleanInclude, clean)
		}
	}

	cleanExclude := make([]string, 0, len(exclude))
	for _, p := range exclude {
		clean := strings.TrimPrefix(strings.TrimSpace(p), "/")
		if clean != "" {
			cleanExclude = append(cleanExclude, clean)
		}
	}

	if len(cleanInclude) > 0 {
		return &ContainerFilter{
			mode:     FilterModeInclude,
			patterns: cleanInclude,
		}, nil
	}

	if len(cleanExclude) > 0 {
		return &ContainerFilter{
			mode:     FilterModeExclude,
			patterns: cleanExclude,
		}, nil
	}

	return &ContainerFilter{
		mode:     FilterModeAll,
		patterns: nil,
	}, nil
}

// Mode returns the active FilterMode.
func (f *ContainerFilter) Mode() FilterMode {
	if f == nil {
		return FilterModeAll
	}
	return f.mode
}

// Patterns returns a copy of the active filter patterns.
func (f *ContainerFilter) Patterns() []string {
	if f == nil || len(f.patterns) == 0 {
		return nil
	}
	out := make([]string, len(f.patterns))
	copy(out, f.patterns)
	return out
}

// String returns a human-readable summary of the filter policy.
func (f *ContainerFilter) String() string {
	if f == nil {
		return "all"
	}
	switch f.mode {
	case FilterModeInclude:
		return fmt.Sprintf("include(%d patterns)", len(f.patterns))
	case FilterModeExclude:
		return fmt.Sprintf("exclude(%d patterns)", len(f.patterns))
	default:
		return "all"
	}
}

// Match evaluates whether a container name should be monitored.
// Executes with zero heap allocations on the hot path.
func (f *ContainerFilter) Match(containerName string) bool {
	if f == nil {
		return true
	}

	name := containerName
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	if name == "" {
		return false
	}

	switch f.mode {
	case FilterModeAll:
		return true

	case FilterModeInclude:
		for _, p := range f.patterns {
			if matched, _ := path.Match(p, name); matched {
				return true
			}
		}
		return false

	case FilterModeExclude:
		for _, p := range f.patterns {
			if matched, _ := path.Match(p, name); matched {
				return false
			}
		}
		return true
	}

	return true
}

// FilterContainers returns a filtered slice containing only approved containers.
func (f *ContainerFilter) FilterContainers(containers []ContainerInfo) []ContainerInfo {
	if f == nil || f.mode == FilterModeAll {
		return containers
	}

	out := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		if f.Match(c.Name) {
			out = append(out, c)
		}
	}
	return out
}
