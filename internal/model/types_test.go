package model

import (
	"strings"
	"testing"
	"time"
)

func TestLevelStringAndParse(t *testing.T) {
	levels := []struct {
		lvl  Level
		str  string
		name string
	}{
		{LevelUnknown, "UNKNOWN", "unknown"},
		{LevelDebug, "DEBUG", "debug"},
		{LevelInfo, "INFO", "info"},
		{LevelWarn, "WARN", "warn"},
		{LevelError, "ERROR", "error"},
		{LevelFatal, "FATAL", "fatal"},
	}

	for _, l := range levels {
		if l.lvl.String() != l.str {
			t.Errorf("Level %d: expected %s, got %s", l.lvl, l.str, l.lvl.String())
		}
		parsed := ParseLevel(l.name)
		if parsed != l.lvl {
			t.Errorf("ParseLevel(%q): expected %v, got %v", l.name, l.lvl, parsed)
		}
	}

	// Test aliases
	if ParseLevel("trace") != LevelDebug {
		t.Errorf("expected LevelDebug for trace")
	}
	if ParseLevel("panic") != LevelFatal {
		t.Errorf("expected LevelFatal for panic")
	}
	if ParseLevel("nonexistent") != LevelUnknown {
		t.Errorf("expected LevelUnknown for nonexistent")
	}
}

func TestEnums(t *testing.T) {
	if LevelSourceStructured.String() != "structured" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceStructured)
	}
	if LevelSourceDelimited.String() != "delimited" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceDelimited)
	}
	if LevelSourceKeyword.String() != "keyword" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceKeyword)
	}
	if LevelSourceHint.String() != "hint" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceHint)
	}
	if LevelSourceInherited.String() != "inherited" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceInherited)
	}
	if LevelSourceNone.String() != "none" {
		t.Errorf("unexpected LevelSource string: %s", LevelSourceNone)
	}

	if TimestampSourceLine.String() != "line" {
		t.Errorf("unexpected TimestampSource string: %s", TimestampSourceLine)
	}
	if TimestampSourceCarried.String() != "carried" {
		t.Errorf("unexpected TimestampSource string: %s", TimestampSourceCarried)
	}
	if TimestampSourceDocker.String() != "docker" {
		t.Errorf("unexpected TimestampSource string: %s", TimestampSourceDocker)
	}
	if TimestampSourceReadTime.String() != "read_time" {
		t.Errorf("unexpected TimestampSource string: %s", TimestampSourceReadTime)
	}

	if StreamStdout.String() != "stdout" {
		t.Errorf("unexpected Stream string: %s", StreamStdout)
	}
	if StreamStderr.String() != "stderr" {
		t.Errorf("unexpected Stream string: %s", StreamStderr)
	}
	if StreamNA.String() != "n/a" {
		t.Errorf("unexpected Stream string: %s", StreamNA)
	}
}

func TestLogEntryString(t *testing.T) {
	entry := LogEntry{
		Timestamp:       time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		TimestampSource: TimestampSourceParsed,
		SourceID:        "app-server",
		Level:           LevelError,
		LevelSource:     LevelSourceStructured,
		Stream:          StreamStderr,
		Message:         []byte("database connection lost"),
	}

	s := entry.String()
	if !strings.Contains(s, "2026-11-13T10:00:00Z") ||
		!strings.Contains(s, "ERROR") ||
		!strings.Contains(s, "app-server") ||
		!strings.Contains(s, "database connection lost") {
		t.Errorf("unexpected LogEntry string output: %s", s)
	}
}

func TestMetricSample(t *testing.T) {
	s := MetricSample{
		Entity: "host",
		Metric: "cpu_pct",
		TS:     1731650000,
		Value:  42.5,
	}
	var alias Sample = s
	if alias.Entity != "host" || alias.Metric != "cpu_pct" || alias.TS != 1731650000 || alias.Value != 42.5 {
		t.Fatalf("unexpected MetricSample fields: %+v", alias)
	}
}
