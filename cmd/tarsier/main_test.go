package main

import (
	"testing"
	"time"
)

func TestParseDurationOrDefault(t *testing.T) {
	tests := []struct {
		name       string
		val        string
		defaultVal time.Duration
		expected   time.Duration
	}{
		{
			name:       "valid duration",
			val:        "30m",
			defaultVal: 15 * time.Minute,
			expected:   30 * time.Minute,
		},
		{
			name:       "empty string returns default",
			val:        "",
			defaultVal: 24 * time.Hour,
			expected:   24 * time.Hour,
		},
		{
			name:       "invalid format returns default",
			val:        "invalid",
			defaultVal: 10 * time.Second,
			expected:   10 * time.Second,
		},
		{
			name:       "zero duration returns default",
			val:        "0s",
			defaultVal: 5 * time.Minute,
			expected:   5 * time.Minute,
		},
		{
			name:       "negative duration returns default",
			val:        "-10m",
			defaultVal: 1 * time.Hour,
			expected:   1 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDurationOrDefault(tt.val, tt.defaultVal)
			if got != tt.expected {
				t.Errorf("parseDurationOrDefault(%q, %v) = %v; want %v", tt.val, tt.defaultVal, got, tt.expected)
			}
		})
	}
}
