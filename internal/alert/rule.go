package alert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Condition models serialized in alert_rule.condition_json

// MetricCondition defines evaluation parameters for numeric metric thresholds.
type MetricCondition struct {
	Metric    string  `json:"metric"`    // e.g. "cpu_pct", "mem_used_pct", "disk_used_pct", "load1"
	Operator  string  `json:"operator"`  // ">" | ">=" | "<" | "<=" | "=="
	Threshold float64 `json:"threshold"`
}

// LogCondition defines evaluation parameters for streaming log pattern matches.
type LogCondition struct {
	Pattern    string `json:"pattern"`     // Substring or regex pattern (max 512 bytes)
	IsRegex    bool   `json:"is_regex"`
	IgnoreCase bool   `json:"ignore_case"`
	MinLevel   uint8  `json:"min_level"`   // 0=any, 1=debug, 2=info, 3=warn, 4=error, 5=fatal
	Threshold  int    `json:"threshold"`   // Match count >= Threshold within WindowSec (1..10000)
	WindowSec  int    `json:"window_sec"`  // Sliding window duration (10..86400s)
}

// ContainerCondition defines evaluation parameters for Docker container lifecycle events.
type ContainerCondition struct {
	Events    []string `json:"events"`     // Subset of ["die", "oom", "stop", "restart", "down"]
	Threshold int      `json:"threshold"`  // e.g. >= 3 restarts within WindowSec (default 1)
	WindowSec int      `json:"window_sec"` // e.g. 300s (0 for immediate single-event triggers like OOM)
}

// AlertRule represents a persistent alert rule in config.db.
type AlertRule struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"` // "metric" | "log" | "container"
	Enabled        bool      `json:"enabled"`
	Severity       string    `json:"severity"` // "info" | "warning" | "critical"
	Target         string    `json:"target"`   // e.g. "host", "disk:*", "container:*", "*", "source-name"
	ConditionJSON  string    `json:"condition_json"`
	ForSec         int       `json:"for_sec"`      // 0..86400
	CooldownSec    int       `json:"cooldown_sec"` // 0..604800 (default 300)
	Channels       []string  `json:"channels"`     // subset of {"email", "discord"}
	NotifyRecovery bool      `json:"notify_recovery"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CompiledRule encapsulates an AlertRule with pre-compiled matchers and condition structs.
type CompiledRule struct {
	AlertRule
	MetricCond    *MetricCondition
	LogCond       *LogCondition
	ContainerCond *ContainerCondition
	Regex         *regexp.Regexp
	ByteNeedle    []byte
	MatchesTarget func(target string) bool
	MatchLogLine  func(line []byte) bool
}

// Allowed values and limits
const (
	MaxRuleNameLen    = 120
	MaxPatternLen     = 512
	MaxForSec         = 86400   // 24 hours
	MaxCooldownSec    = 604800  // 7 days
	MaxRulesCount     = 250
	MaxLogThreshold   = 10000
	MinLogWindowSec   = 10
	MaxLogWindowSec   = 86400
)

var (
	validKinds      = map[string]bool{"metric": true, "log": true, "container": true}
	validSeverities = map[string]bool{"info": true, "warning": true, "critical": true}
	validOperators  = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true}
	validChannels   = map[string]bool{"email": true, "discord": true}
	validEvents     = map[string]bool{"die": true, "oom": true, "stop": true, "restart": true, "down": true}
)

// CompileRule strictly validates an AlertRule and prepares zero-alloc/pre-compiled matchers.
func CompileRule(r AlertRule) (*CompiledRule, error) {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return nil, errors.New("rule name cannot be empty")
	}
	if len(name) > MaxRuleNameLen {
		return nil, fmt.Errorf("rule name exceeds maximum length of %d chars", MaxRuleNameLen)
	}

	kind := strings.ToLower(strings.TrimSpace(r.Kind))
	if !validKinds[kind] {
		return nil, fmt.Errorf("invalid rule kind %q; must be 'metric', 'log', or 'container'", r.Kind)
	}

	severity := strings.ToLower(strings.TrimSpace(r.Severity))
	if !validSeverities[severity] {
		return nil, fmt.Errorf("invalid severity %q; must be 'info', 'warning', or 'critical'", r.Severity)
	}

	target := strings.TrimSpace(r.Target)
	if target == "" {
		return nil, errors.New("target cannot be empty")
	}

	if r.ForSec < 0 || r.ForSec > MaxForSec {
		return nil, fmt.Errorf("for_sec %d out of range (0..%d)", r.ForSec, MaxForSec)
	}

	if r.CooldownSec < 0 || r.CooldownSec > MaxCooldownSec {
		return nil, fmt.Errorf("cooldown_sec %d out of range (0..%d)", r.CooldownSec, MaxCooldownSec)
	}

	if len(r.Channels) == 0 {
		return nil, errors.New("at least one notification channel must be specified")
	}

	cleanChannels := make([]string, 0, len(r.Channels))
	seenCh := make(map[string]bool)
	for _, ch := range r.Channels {
		chNorm := strings.ToLower(strings.TrimSpace(ch))
		if !validChannels[chNorm] {
			return nil, fmt.Errorf("unsupported notification channel %q; must be 'email' or 'discord'", ch)
		}
		if !seenCh[chNorm] {
			seenCh[chNorm] = true
			cleanChannels = append(cleanChannels, chNorm)
		}
	}

	compiled := &CompiledRule{
		AlertRule: r,
	}
	compiled.Name = name
	compiled.Kind = kind
	compiled.Severity = severity
	compiled.Target = target
	compiled.Channels = cleanChannels

	// Pre-compile target matcher
	compiled.MatchesTarget = buildTargetMatcher(target)

	// Validate & compile condition by kind
	switch kind {
	case "metric":
		var mc MetricCondition
		if err := json.Unmarshal([]byte(r.ConditionJSON), &mc); err != nil {
			return nil, fmt.Errorf("invalid metric condition JSON: %w", err)
		}
		mc.Metric = strings.TrimSpace(mc.Metric)
		if mc.Metric == "" {
			return nil, errors.New("metric condition: metric name cannot be empty")
		}
		if !validOperators[mc.Operator] {
			return nil, fmt.Errorf("metric condition: invalid operator %q", mc.Operator)
		}
		compiled.MetricCond = &mc

	case "log":
		var lc LogCondition
		if err := json.Unmarshal([]byte(r.ConditionJSON), &lc); err != nil {
			return nil, fmt.Errorf("invalid log condition JSON: %w", err)
		}
		if len(lc.Pattern) == 0 {
			return nil, errors.New("log condition: pattern cannot be empty")
		}
		if len(lc.Pattern) > MaxPatternLen {
			return nil, fmt.Errorf("log condition: pattern exceeds maximum length of %d bytes", MaxPatternLen)
		}
		if lc.Threshold < 1 || lc.Threshold > MaxLogThreshold {
			return nil, fmt.Errorf("log condition: threshold %d out of range (1..%d)", lc.Threshold, MaxLogThreshold)
		}
		if lc.WindowSec < MinLogWindowSec || lc.WindowSec > MaxLogWindowSec {
			return nil, fmt.Errorf("log condition: window_sec %d out of range (%d..%d)", lc.WindowSec, MinLogWindowSec, MaxLogWindowSec)
		}
		if lc.MinLevel > 5 {
			return nil, fmt.Errorf("log condition: min_level %d out of range (0..5)", lc.MinLevel)
		}

		if lc.IsRegex {
			pat := lc.Pattern
			if lc.IgnoreCase && !strings.HasPrefix(pat, "(?i)") {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("log condition: invalid regex pattern %q: %w", lc.Pattern, err)
			}
			compiled.Regex = re
			compiled.MatchLogLine = func(line []byte) bool {
				return re.Match(line)
			}
		} else {
			needle := []byte(lc.Pattern)
			if lc.IgnoreCase {
				lowerNeedle := bytes.ToLower(needle)
				compiled.ByteNeedle = lowerNeedle
				compiled.MatchLogLine = func(line []byte) bool {
					return bytes.Contains(bytes.ToLower(line), lowerNeedle)
				}
			} else {
				compiled.ByteNeedle = needle
				compiled.MatchLogLine = func(line []byte) bool {
					return bytes.Contains(line, needle)
				}
			}
		}
		compiled.LogCond = &lc

	case "container":
		var cc ContainerCondition
		if err := json.Unmarshal([]byte(r.ConditionJSON), &cc); err != nil {
			return nil, fmt.Errorf("invalid container condition JSON: %w", err)
		}
		if len(cc.Events) == 0 {
			return nil, errors.New("container condition: events list cannot be empty")
		}
		cleanEvents := make([]string, 0, len(cc.Events))
		seenEv := make(map[string]bool)
		for _, ev := range cc.Events {
			evNorm := strings.ToLower(strings.TrimSpace(ev))
			if !validEvents[evNorm] {
				return nil, fmt.Errorf("container condition: invalid event %q; must be one of die, oom, stop, restart, down", ev)
			}
			if !seenEv[evNorm] {
				seenEv[evNorm] = true
				cleanEvents = append(cleanEvents, evNorm)
			}
		}
		cc.Events = cleanEvents
		if cc.Threshold < 1 {
			cc.Threshold = 1
		}
		if cc.WindowSec < 0 || cc.WindowSec > MaxLogWindowSec {
			return nil, fmt.Errorf("container condition: window_sec %d out of range (0..%d)", cc.WindowSec, MaxLogWindowSec)
		}
		compiled.ContainerCond = &cc
	}

	return compiled, nil
}

func buildTargetMatcher(target string) func(string) bool {
	if target == "*" {
		return func(string) bool { return true }
	}
	if strings.HasSuffix(target, ":*") {
		prefix := strings.TrimSuffix(target, "*")
		return func(actual string) bool {
			return strings.HasPrefix(actual, prefix)
		}
	}
	return func(actual string) bool {
		return actual == target
	}
}
