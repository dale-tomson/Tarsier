package cleanup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMessage_Table(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "UUID grouping",
			input:    "failed to load user 550e8400-e29b-41d4-a716-446655440000: not found",
			expected: "failed to load user {uuid}: not found",
		},
		{
			name:     "UUID grouping second sample",
			input:    "failed to load user 123e4567-e89b-12d3-a456-426614174000: not found",
			expected: "failed to load user {uuid}: not found",
		},
		{
			name:     "IPv4 with port",
			input:    "dial tcp 10.0.1.12:5432: connect: connection refused",
			expected: "dial tcp {ip}:{num}: connect: connection refused",
		},
		{
			name:     "IPv4 with port second sample",
			input:    "dial tcp 10.0.2.99:5432: connect: connection refused",
			expected: "dial tcp {ip}:{num}: connect: connection refused",
		},
		{
			name:     "IPv4 standalone",
			input:    "received ping from 192.168.1.1 successfully",
			expected: "received ping from {ip} successfully",
		},
		{
			name:     "IPv6 with port",
			input:    "dial tcp [::1]:8080: connect: connection refused",
			expected: "dial tcp {ip}:{num}: connect: connection refused",
		},
		{
			name:     "Hex container ID and memory address",
			input:    "container 8f14e45fceea exited with code 137 (addr 0xdeadbeef)",
			expected: "container {hex} exited with code {num} (addr {hex})",
		},
		{
			name:     "Double quoted path",
			input:    `open "/var/lib/app/tmp/file_99.json": permission denied`,
			expected: `open "{str}": permission denied`,
		},
		{
			name:     "Single quoted path",
			input:    `open '/var/lib/app/tmp/file_99.json': permission denied`,
			expected: `open '{str}': permission denied`,
		},
		{
			name:     "Unquoted path",
			input:    "open /var/lib/app/tmp/file_99.json: permission denied",
			expected: "open {path}: permission denied",
		},
		{
			name:     "Embedded RFC3339 timestamp",
			input:    "2026-11-15T13:55:01.123Z [ERROR] failed to connect to database",
			expected: "{ts} [ERROR] failed to connect to database",
		},
		{
			name:     "Embedded space-separated timestamp",
			input:    "2026-11-15 13:55:01 [ERROR] worker panic",
			expected: "{ts} [ERROR] worker panic",
		},
		{
			name:     "Durations and percentages",
			input:    "query latency 1532ms exceeded threshold 98.6% on attempt 3",
			expected: "query latency {num}ms exceeded threshold {num}% on attempt {num}",
		},
		{
			name:     "Negative numbers",
			input:    "process exited with status -42 and signal -9",
			expected: "process exited with status {num} and signal {num}",
		},
		{
			name:     "Preserves diagnostic SQL keywords and HTTP classes",
			input:    "SELECT * FROM users WHERE id = 42 returned HTTP 500 Internal Server Error",
			expected: "SELECT * FROM users WHERE id = {num} returned HTTP {num} Internal Server Error",
		},
		{
			name:     "Empty and whitespace string",
			input:    "   \t\n  ",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeMessage(tc.input)
			if got != tc.expected {
				t.Errorf("\nInput:    %q\nExpected: %q\nGot:      %q", tc.input, tc.expected, got)
			}
		})
	}
}

func TestNormalizeMessage_TruncationGuard(t *testing.T) {
	// 10 KB line
	hugeInput := "ERROR database query failed with " + strings.Repeat("A", 10000)
	normalized := NormalizeMessage(hugeInput)

	if len(normalized) > 256 {
		t.Fatalf("expected output length <= 256, got %d", len(normalized))
	}
	if !strings.HasPrefix(normalized, "ERROR database query failed with") {
		t.Fatalf("expected normalized output to preserve prefix, got %q", normalized)
	}
}

func TestPatternCounter_TopNAndBounds(t *testing.T) {
	counter := NewPatternCounter(100) // Small cap for testing

	now := time.Now().UnixNano()

	// Feed 1,000 mixed lines across 5 recurring error templates
	templates := []string{
		"failed to load user %s: connection refused",
		"disk usage at %d%% reached critical threshold",
		"container %s died with code %d",
		"timeout waiting %dms for response from 10.0.1.%d:8080",
		"syntax error in /var/log/app_%d.log at line %d",
	}

	// Template frequencies:
	// Template 0: 300 times
	// Template 1: 250 times
	// Template 2: 200 times
	// Template 3: 150 times
	// Template 4: 100 times
	counts := []int{300, 250, 200, 150, 100}

	for i, tmpl := range templates {
		for c := 0; c < counts[i]; c++ {
			var line string
			switch i {
			case 0:
				line = fmt.Sprintf(tmpl, fmt.Sprintf("550e8400-e29b-41d4-a716-%012d", c))
			case 1:
				line = fmt.Sprintf(tmpl, 80+c%15)
			case 2:
				line = fmt.Sprintf(tmpl, fmt.Sprintf("8f14e45f%04x", c), 137)
			case 3:
				line = fmt.Sprintf(tmpl, 500+c, c%10)
			case 4:
				line = fmt.Sprintf(tmpl, c, c*10)
			}
			counter.Add("api-server", now+int64(c), line)
		}
	}

	top3 := counter.TopN(3)
	if len(top3) != 3 {
		t.Fatalf("expected 3 patterns, got %d", len(top3))
	}

	// Verify top 3 counts match descending order
	if top3[0].Count != 300 {
		t.Errorf("expected top pattern count 300, got %d (pattern: %s)", top3[0].Count, top3[0].Pattern)
	}
	if top3[1].Count != 250 {
		t.Errorf("expected 2nd pattern count 250, got %d (pattern: %s)", top3[1].Count, top3[1].Pattern)
	}
	if top3[2].Count != 200 {
		t.Errorf("expected 3rd pattern count 200, got %d (pattern: %s)", top3[2].Count, top3[2].Pattern)
	}

	for _, p := range top3 {
		if p.SampleLine == "" {
			t.Errorf("expected non-empty sample line for %s", p.Pattern)
		}
		if p.SourceName != "api-server" {
			t.Errorf("expected source api-server, got %s", p.SourceName)
		}
	}

	// Verify bounded capacity under flood of 20,000 unique un-normalizable lines
	for i := 0; i < 20000; i++ {
		counter.Add("noisy-source", now+int64(i), fmt.Sprintf("unique_message_marker_alpha_%d_beta_gamma", i))
	}

	if counter.Len() > 100 {
		t.Fatalf("expected bounded entries <= 100, got %d", counter.Len())
	}
}

func BenchmarkNormalizeMessage(b *testing.B) {
	b.Run("CompositeAllTokens", func(b *testing.B) {
		line := "2026-11-15T13:55:01.123Z [ERROR] failed to query container 8f14e45fceea at 10.0.1.12:5432 after 1532ms: open \"/var/lib/app/tmp/file_99.json\": permission denied (user 550e8400-e29b-41d4-a716-446655440000, code -42, usage 98.6%)"
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = NormalizeMessage(line)
		}
	})

	b.Run("RealisticStandardError", func(b *testing.B) {
		line := "dial tcp 10.0.1.12:5432: connect: connection refused (failed loading user 550e8400-e29b-41d4-a716-446655440000 after 3 attempts)"
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = NormalizeMessage(line)
		}
	})
}
