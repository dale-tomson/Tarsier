package model

import (
	"fmt"
	"strings"
	"time"
)

// Level represents log severity level.
type Level uint8

const (
	LevelUnknown Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String returns the canonical uppercase representation of Level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// ParseLevel parses a level string into a Level enum.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG", "DBG", "TRACE":
		return LevelDebug
	case "INFO", "INF", "NOTICE":
		return LevelInfo
	case "WARN", "WARNING", "WRN":
		return LevelWarn
	case "ERROR", "ERR":
		return LevelError
	case "FATAL", "CRIT", "CRITICAL", "PANIC", "EMERG", "EMERGENCY":
		return LevelFatal
	default:
		return LevelUnknown
	}
}

// LevelSource indicates how the log level was determined.
type LevelSource uint8

const (
	LevelSourceUnknown LevelSource = iota
	LevelSourceStructured
	LevelSourceKeyword
	LevelSourceHint
	LevelSourceDefault
)

func (s LevelSource) String() string {
	switch s {
	case LevelSourceStructured:
		return "structured"
	case LevelSourceKeyword:
		return "keyword"
	case LevelSourceHint:
		return "hint"
	case LevelSourceDefault:
		return "default"
	default:
		return "unknown"
	}
}

// TimestampSource indicates how the log timestamp was obtained.
type TimestampSource uint8

const (
	TimestampSourceUnknown TimestampSource = iota
	TimestampSourceHeader
	TimestampSourceParsed
	TimestampSourceIngest
)

func (s TimestampSource) String() string {
	switch s {
	case TimestampSourceHeader:
		return "header"
	case TimestampSourceParsed:
		return "parsed"
	case TimestampSourceIngest:
		return "ingest"
	default:
		return "unknown"
	}
}

// Stream represents standard output stream classification.
type Stream uint8

const (
	StreamNA Stream = iota
	StreamStdout
	StreamStderr
)

func (s Stream) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "n/a"
	}
}

// LogEntry represents an ingested, parsed log line.
// Message and Raw use []byte to avoid heap allocations on hot paths.
type LogEntry struct {
	Timestamp       time.Time
	TimestampSource TimestampSource
	SourceID        string
	Level           Level
	LevelSource     LevelSource
	Stream          Stream
	Message         []byte
	Raw             []byte
}

func (e LogEntry) String() string {
	return fmt.Sprintf("[%s] [%s] [%s] %s",
		e.Timestamp.Format(time.RFC3339),
		e.Level,
		e.SourceID,
		string(e.Message),
	)
}

// MetricSample represents a single numeric telemetry measurement.
type MetricSample struct {
	Timestamp time.Time
	SeriesID  int64
	Value     float64
}
