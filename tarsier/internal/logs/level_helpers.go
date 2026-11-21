package logs

import (
	"github.com/dale-tomson/Tarsier/internal/model"
)

// -----------------------------------------------------------------------------
// Canonical Vocabulary & O(1) Lookup
// -----------------------------------------------------------------------------

// matchLevelWord matches canonical level words case-insensitively with strict plural exclusion.
func matchLevelWord(b []byte) (model.Level, bool) {
	n := len(b)
	if n == 0 {
		return model.LevelUnknown, false
	}
	// Strict plural exclusion: any level candidate ending in 's' is excluded
	if b[n-1] == 's' || b[n-1] == 'S' {
		return model.LevelUnknown, false
	}

	c0 := b[0] | 0x20
	switch n {
	case 3: // err, wrn, inf, dbg
		if c0 == 'e' && (b[1]|0x20) == 'r' && (b[2]|0x20) == 'r' {
			return model.LevelError, true
		}
		if c0 == 'w' && (b[1]|0x20) == 'r' && (b[2]|0x20) == 'n' {
			return model.LevelWarn, true
		}
		if c0 == 'i' && (b[1]|0x20) == 'n' && (b[2]|0x20) == 'f' {
			return model.LevelInfo, true
		}
		if c0 == 'd' && (b[1]|0x20) == 'b' && (b[2]|0x20) == 'g' {
			return model.LevelDebug, true
		}
	case 4: // warn, info, fine, crit
		if c0 == 'w' && equalFold(b, "warn") {
			return model.LevelWarn, true
		}
		if c0 == 'i' && equalFold(b, "info") {
			return model.LevelInfo, true
		}
		if c0 == 'f' && equalFold(b, "fine") {
			return model.LevelDebug, true
		}
		if c0 == 'c' && equalFold(b, "crit") {
			return model.LevelFatal, true
		}
	case 5: // error, fatal, panic, debug, trace, finer, emerg, alert
		if c0 == 'e' {
			if equalFold(b, "error") {
				return model.LevelError, true
			}
			if equalFold(b, "emerg") {
				return model.LevelFatal, true
			}
		} else if c0 == 'f' {
			if equalFold(b, "fatal") {
				return model.LevelFatal, true
			}
			if equalFold(b, "finer") {
				return model.LevelDebug, true
			}
		} else if c0 == 'p' && equalFold(b, "panic") {
			return model.LevelFatal, true
		} else if c0 == 'd' && equalFold(b, "debug") {
			return model.LevelDebug, true
		} else if c0 == 't' && equalFold(b, "trace") {
			return model.LevelDebug, true
		} else if c0 == 'a' && equalFold(b, "alert") {
			return model.LevelFatal, true
		}
	case 6: // dpanic, severe, notice, finest
		if c0 == 'd' && equalFold(b, "dpanic") {
			return model.LevelFatal, true
		}
		if c0 == 's' && equalFold(b, "severe") {
			return model.LevelError, true
		}
		if c0 == 'n' && equalFold(b, "notice") {
			return model.LevelInfo, true
		}
		if c0 == 'f' && equalFold(b, "finest") {
			return model.LevelDebug, true
		}
	case 7: // warning, verbose
		if c0 == 'w' && equalFold(b, "warning") {
			return model.LevelWarn, true
		}
		if c0 == 'v' && equalFold(b, "verbose") {
			return model.LevelDebug, true
		}
	case 8: // critical
		if c0 == 'c' && equalFold(b, "critical") {
			return model.LevelFatal, true
		}
	case 9: // emergency
		if c0 == 'e' && equalFold(b, "emergency") {
			return model.LevelFatal, true
		}
	case 11: // information
		if c0 == 'i' && equalFold(b, "information") {
			return model.LevelInfo, true
		}
	}
	return model.LevelUnknown, false
}

// matchSingleLetterCode matches single-character severity indicators in delimited positions.
func matchSingleLetterCode(c byte) (model.Level, bool) {
	switch c {
	case 'F', 'f':
		return model.LevelFatal, true
	case 'E', 'e':
		return model.LevelError, true
	case 'W', 'w':
		return model.LevelWarn, true
	case 'I', 'i':
		return model.LevelInfo, true
	case 'D', 'd', 'T', 't':
		return model.LevelDebug, true
	}
	return model.LevelUnknown, false
}

// -----------------------------------------------------------------------------
// Tier 1 — Structured Level Scanners
// -----------------------------------------------------------------------------

func isJSONLevelKey(k []byte) bool {
	switch len(k) {
	case 3:
		return equalFold(k, "lvl")
	case 5:
		return equalFold(k, "level")
	case 6:
		return equalFold(k, "status")
	case 8:
		return equalFold(k, "severity") || equalFold(k, "loglevel")
	case 9:
		return equalFold(k, "log.level")
	}
	return false
}

func pinoLevelToModel(num int) model.Level {
	switch {
	case num >= 60:
		return model.LevelFatal
	case num >= 50:
		return model.LevelError
	case num >= 40:
		return model.LevelWarn
	case num >= 30:
		return model.LevelInfo
	case num >= 20:
		return model.LevelDebug
	case num >= 10:
		return model.LevelDebug
	default:
		return model.LevelUnknown
	}
}

func isPrecededByLevelLabel(b []byte) bool {
	// check ends with "level: " or "lvl: "
	i := len(b) - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	if i < 0 || b[i] != ':' {
		return false
	}
	i--
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	start := i
	for start >= 0 && isAlpha(b[start]) {
		start--
	}
	label := b[start+1 : i+1]
	return equalFold(label, "level") || equalFold(label, "lvl")
}

func isPrecededByTimestamp(b []byte) bool {
	i := len(b) - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	if i < 7 {
		return false
	}
	// Timestamp typically ends with 'Z', digits, or offset like "+05:30"
	c := b[i]
	return (c >= '0' && c <= '9') || c == 'Z' || c == 'z' || c == ']' || c == ')'
}

// -----------------------------------------------------------------------------
// Tier 3 — Keyword Anywhere in Line
// -----------------------------------------------------------------------------

func hasFailureHint(line []byte) bool {
	failurePhrases := []string{
		"exception", "traceback", "stacktrace", "segfault",
		"oom-killer", "out of memory", "connection refused",
		"timed out", "timeout", "failed", "failure", "unhandled",
	}

	for _, phrase := range failurePhrases {
		idx := 0
		for {
			pos := indexOfFold(line[idx:], phrase)
			if pos < 0 {
				break
			}
			start := idx + pos
			end := start + len(phrase)
			if !isBenignContext(line, start, end) {
				return true
			}
			idx = end
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Benign-Context Exclusions (Applied to Tiers 3 & 4)
// -----------------------------------------------------------------------------

func isBenignContext(line []byte, start, end int) bool {
	// 1. File paths, URLs, code identifiers (adjacent symbols)
	if start > 0 {
		prev := line[start-1]
		if prev == '/' || prev == '.' || prev == '_' || prev == '-' || isAlpha(prev) {
			return true
		}
	}
	if end < len(line) {
		next := line[end]
		if next == '/' || next == '.' || next == '_' || next == '-' || isAlpha(next) {
			return true
		}
	}

	// 2. Zero counts & negations preceding token (within 24 bytes)
	lookbackStart := start - 24
	if lookbackStart < 0 {
		lookbackStart = 0
	}
	before := line[lookbackStart:start]
	if endsWithNegation(before) {
		return true
	}

	// 3. Falsy / empty assignments following token (within 24 bytes)
	lookaheadEnd := end + 24
	if lookaheadEnd > len(line) {
		lookaheadEnd = len(line)
	}
	after := line[end:lookaheadEnd]
	if startsWithFalsyAssignment(after) {
		return true
	}

	return false
}

func endsWithNegation(b []byte) bool {
	// Trim trailing spaces
	i := len(b) - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	if i < 0 {
		return false
	}
	sub := b[:i+1]

	// Check "0", "no", "without"
	if hasSuffixFold(sub, " 0") || equalFold(sub, "0") {
		return true
	}
	if hasSuffixFold(sub, " no") || equalFold(sub, "no") {
		return true
	}
	if hasSuffixFold(sub, " without") || equalFold(sub, "without") {
		return true
	}
	return false
}

func startsWithFalsyAssignment(b []byte) bool {
	i := 0
	// Skip leading whitespace and optional closing quote
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '"' || b[i] == '\'') {
		i++
	}
	if i >= len(b) {
		return false
	}

	// Assignment operator: '=', ':', or "is "
	hasAssign := false
	if b[i] == '=' || b[i] == ':' {
		hasAssign = true
		i++
	} else if i+3 <= len(b) && (b[i]|0x20) == 'i' && (b[i+1]|0x20) == 's' && b[i+2] == ' ' {
		hasAssign = true
		i += 3
	}

	if !hasAssign {
		return false
	}

	// Skip spaces after assignment
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	if i >= len(b) {
		return false
	}

	valSub := b[i:]
	falsyValues := []string{"nil", "<nil>", "null", "none", "false", "0", "\"\"", "''"}
	for _, f := range falsyValues {
		if hasPrefixFold(valSub, f) {
			// Check boundary after falsy value
			afterVal := valSub[len(f):]
			if len(afterVal) == 0 || afterVal[0] == ' ' || afterVal[0] == '\t' || afterVal[0] == ',' || afterVal[0] == '}' || afterVal[0] == ')' || afterVal[0] == ']' || afterVal[0] == '\r' || afterVal[0] == '\n' {
				return true
			}
		}
	}
	return false
}

func isStackTraceLine(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if b[0] == '\t' {
		return true
	}
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	sub := b[i:]
	return hasPrefix(sub, []byte("at ")) ||
		hasPrefix(sub, []byte("File \"")) ||
		hasPrefix(sub, []byte("goroutine ")) ||
		hasPrefix(sub, []byte("Caused by:")) ||
		hasPrefix(sub, []byte("panic:"))
}

// -----------------------------------------------------------------------------
// ASCII Helpers (Zero Allocation)
// -----------------------------------------------------------------------------

func equalFold(a []byte, s string) bool {
	if len(a) != len(s) {
		return false
	}
	for i := range a {
		if (a[i] | 0x20) != (s[i] | 0x20) {
			return false
		}
	}
	return true
}

func hasPrefixFold(a []byte, s string) bool {
	if len(a) < len(s) {
		return false
	}
	for i := range s {
		if (a[i] | 0x20) != (s[i] | 0x20) {
			return false
		}
	}
	return true
}

func hasSuffixFold(a []byte, s string) bool {
	if len(a) < len(s) {
		return false
	}
	start := len(a) - len(s)
	for i := range s {
		if (a[start+i] | 0x20) != (s[i] | 0x20) {
			return false
		}
	}
	return true
}

func indexOfFold(b []byte, s string) int {
	if len(s) == 0 {
		return 0
	}
	if len(b) < len(s) {
		return -1
	}
	first := s[0] | 0x20
	max := len(b) - len(s)
	for i := 0; i <= max; i++ {
		if (b[i] | 0x20) == first && hasPrefixFold(b[i:], s) {
			return i
		}
	}
	return -1
}
