package logs

import (
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// LevelConfig specifies toggles and scanning limits for log level detection.
type LevelConfig struct {
	MaxScanBytes  int  // Default 4096
	KeywordLevels bool // Tier 3 enabled (default true)
	Hints         bool // Tier 4 enabled (default true)
}

// LevelResult represents the detected log level and its source confidence classification.
type LevelResult struct {
	Level    model.Level       // 0=Unknown, 1=Debug, 2=Info, 3=Warn, 4=Error, 5=Fatal
	LevelSrc model.LevelSource // 0=None, 1=StructuredKey, 2=DelimitedToken, 3=KeywordInLine, 4=Hint, 5=Inherited
}

// SetLevelConfig updates the detector's level configuration.
func (d *SourceDetector) SetLevelConfig(cfg LevelConfig) {
	if cfg.MaxScanBytes <= 0 {
		cfg.MaxScanBytes = d.maxScanBytes
	}
	d.levelCfg = cfg
}

// DetectLevel detects the severity of an ingested log line in descending confidence order.
func (d *SourceDetector) DetectLevel(line []byte, tsNS int64, isContinuation bool) LevelResult {
	scanLen := len(line)
	maxScan := d.levelCfg.MaxScanBytes
	if maxScan <= 0 {
		maxScan = 4096
	}
	if scanLen > maxScan {
		scanLen = maxScan
	}
	scanned := line[:scanLen]

	// -------------------------------------------------------------------------
	// Tier 1 — Structured Key (JSON / Logfmt)
	// -------------------------------------------------------------------------
	trimmed := trimLeadingWhitespace(scanned)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if res, ok := d.scanJSONLevel(trimmed); ok {
			d.updateLastLevel(res.Level, tsNS)
			return res
		}
	}
	if res, ok := d.scanLogfmtLevel(scanned); ok {
		d.updateLastLevel(res.Level, tsNS)
		return res
	}

	// -------------------------------------------------------------------------
	// Tier 2 — Delimited Token Near Line Start (Prefix Window)
	// -------------------------------------------------------------------------
	if res, ok := d.scanDelimitedLevel(scanned); ok {
		d.updateLastLevel(res.Level, tsNS)
		return res
	}

	// -------------------------------------------------------------------------
	// Tier 3 — Keyword Anywhere in Line (Subject to Benign Contexts)
	// -------------------------------------------------------------------------
	if d.levelCfg.KeywordLevels {
		if res, ok := d.scanKeywordLevel(scanned); ok {
			d.updateLastLevel(res.Level, tsNS)
			return res
		}
	}

	// -------------------------------------------------------------------------
	// Tier 4 — Heuristic Hints (HTTP Status Codes & Error Keywords)
	// -------------------------------------------------------------------------
	if d.levelCfg.Hints {
		if res, ok := d.scanHintLevel(scanned); ok {
			d.updateLastLevel(res.Level, tsNS)
			return res
		}
	}

	// -------------------------------------------------------------------------
	// Tier 5 — Stack Trace / Continuation Inheritance
	// -------------------------------------------------------------------------
	if d.lastLevel != model.LevelUnknown && d.lastLevelTS > 0 && (tsNS-d.lastLevelTS) <= int64(5*time.Second) {
		if isContinuation || isStackTraceLine(scanned) {
			return LevelResult{
				Level:    d.lastLevel,
				LevelSrc: model.LevelSourceInherited,
			}
		}
	}

	return LevelResult{
		Level:    model.LevelUnknown,
		LevelSrc: model.LevelSourceNone,
	}
}

func (d *SourceDetector) updateLastLevel(lvl model.Level, tsNS int64) {
	if lvl != model.LevelUnknown {
		d.lastLevel = lvl
		d.lastLevelTS = tsNS
	}
}

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

func (d *SourceDetector) scanJSONLevel(line []byte) (LevelResult, bool) {
	i := 1 // skip leading '{'
	for i < len(line) {
		for i < len(line) && line[i] != '"' && line[i] != '}' {
			i++
		}
		if i >= len(line) || line[i] == '}' {
			break
		}
		i++ // skip opening quote
		kStart := i
		for i < len(line) && line[i] != '"' && line[i] != '\\' {
			i++
		}
		if i >= len(line) || line[i] != '"' {
			break
		}
		key := line[kStart:i]
		i++ // skip closing quote

		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) || line[i] != ':' {
			break
		}
		i++ // skip ':'

		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) {
			break
		}

		isLvlKey := isJSONLevelKey(key)

		// Parse value
		if line[i] == '"' {
			i++
			valStart := i
			for i < len(line) && line[i] != '"' {
				i++
			}
			val := line[valStart:i]
			if i < len(line) && line[i] == '"' {
				i++
			}
			if isLvlKey && len(val) > 0 {
				if lvl, ok := matchLevelWord(val); ok {
					return LevelResult{Level: lvl, LevelSrc: model.LevelSourceStructured}, true
				}
				if len(val) == 1 {
					if lvl, ok := matchSingleLetterCode(val[0]); ok {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceStructured}, true
					}
				}
			}
		} else if line[i] >= '0' && line[i] <= '9' {
			valStart := i
			for i < len(line) && line[i] >= '0' && line[i] <= '9' {
				i++
			}
			val := line[valStart:i]
			if isLvlKey && len(val) > 0 {
				num, ok := parseUint64(val)
				if ok {
					lvl := pinoLevelToModel(int(num))
					if lvl != model.LevelUnknown {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceStructured}, true
					}
				}
			}
		}

		// Advance to next key: skip until ',' or '}'
		for i < len(line) && line[i] != ',' && line[i] != '}' {
			if line[i] == '"' {
				i++
				for i < len(line) && line[i] != '"' {
					if line[i] == '\\' {
						i++
					}
					i++
				}
				if i < len(line) {
					i++
				}
			} else {
				i++
			}
		}
		if i < len(line) && line[i] == ',' {
			i++
		}
	}
	return LevelResult{}, false
}

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

func (d *SourceDetector) scanLogfmtLevel(line []byte) (LevelResult, bool) {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' && line[i-1] != '[' && line[i-1] != '(' {
			continue
		}
		keyLen := 0
		if hasPrefixFold(line[i:], "level=") {
			keyLen = 6
		} else if hasPrefixFold(line[i:], "lvl=") {
			keyLen = 4
		} else if hasPrefixFold(line[i:], "severity=") {
			keyLen = 9
		} else if hasPrefixFold(line[i:], "loglevel=") {
			keyLen = 9
		}
		if keyLen == 0 {
			continue
		}

		valSub := line[i+keyLen:]
		if len(valSub) == 0 {
			continue
		}

		var val []byte
		if valSub[0] == '"' || valSub[0] == '\'' {
			quote := valSub[0]
			j := 1
			for j < len(valSub) && valSub[j] != quote {
				j++
			}
			val = valSub[1:j]
		} else {
			j := 0
			for j < len(valSub) && valSub[j] != ' ' && valSub[j] != '\t' && valSub[j] != '\r' && valSub[j] != '\n' && valSub[j] != ']' && valSub[j] != ')' {
				j++
			}
			val = valSub[:j]
		}

		if len(val) > 0 {
			if lvl, ok := matchLevelWord(val); ok {
				return LevelResult{Level: lvl, LevelSrc: model.LevelSourceStructured}, true
			}
			if len(val) == 1 {
				if lvl, ok := matchSingleLetterCode(val[0]); ok {
					return LevelResult{Level: lvl, LevelSrc: model.LevelSourceStructured}, true
				}
			}
		}
	}
	return LevelResult{}, false
}

// -----------------------------------------------------------------------------
// Tier 2 — Delimited Level Scanner
// -----------------------------------------------------------------------------

func (d *SourceDetector) scanDelimitedLevel(line []byte) (LevelResult, bool) {
	prefixLimit := 100
	if prefixLimit > len(line) {
		prefixLimit = len(line)
	}
	prefix := line[:prefixLimit]

	// 1. glog/klog prefix at byte 0: [IWEF]MMDD (e.g. E1016 03:47:12)
	if len(prefix) >= 5 {
		c := prefix[0]
		if (c == 'E' || c == 'W' || c == 'I' || c == 'F') &&
			(prefix[1] >= '0' && prefix[1] <= '9') &&
			(prefix[2] >= '0' && prefix[2] <= '9') &&
			(prefix[3] >= '0' && prefix[3] <= '9') &&
			(prefix[4] >= '0' && prefix[4] <= '9') {
			if lvl, ok := matchSingleLetterCode(c); ok {
				return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
			}
		}
	}

	// 2. Syslog <PRI> facility/severity decoding at byte 0
	if len(prefix) > 3 && prefix[0] == '<' {
		idx := 1
		for idx < len(prefix) && prefix[idx] >= '0' && prefix[idx] <= '9' && idx < 5 {
			idx++
		}
		if idx < len(prefix) && prefix[idx] == '>' {
			priVal, ok := parseUint64(prefix[1:idx])
			if ok {
				sev := priVal & 0x07
				var lvl model.Level
				switch {
				case sev <= 2: // Emergency, Alert, Critical
					lvl = model.LevelFatal
				case sev == 3: // Error
					lvl = model.LevelError
				case sev == 4: // Warning
					lvl = model.LevelWarn
				case sev <= 6: // Notice, Informational
					lvl = model.LevelInfo
				case sev == 7: // Debug
					lvl = model.LevelDebug
				}
				if lvl != model.LevelUnknown {
					return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
				}
			}
		}
	}

	// 3. Delimited brackets / parens / pipes: [ERROR], (WARN), <info>, | INFO |, [E]
	for i := 0; i < len(prefix); i++ {
		openDelim := prefix[i]
		var closeDelim byte
		switch openDelim {
		case '[':
			closeDelim = ']'
		case '(':
			closeDelim = ')'
		case '<':
			closeDelim = '>'
		case '|':
			closeDelim = '|'
		}

		if closeDelim != 0 {
			j := i + 1
			for j < len(prefix) && (prefix[j] == ' ' || prefix[j] == '\t') {
				j++
			}
			tokenStart := j
			for j < len(prefix) && prefix[j] != closeDelim && prefix[j] != ' ' && prefix[j] != '\t' {
				j++
			}
			token := prefix[tokenStart:j]
			endJ := j
			for endJ < len(prefix) && (prefix[endJ] == ' ' || prefix[endJ] == '\t') {
				endJ++
			}
			if endJ < len(prefix) && prefix[endJ] == closeDelim {
				if lvl, ok := matchLevelWord(token); ok {
					return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
				}
				if len(token) == 1 {
					if lvl, ok := matchSingleLetterCode(token[0]); ok {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
					}
				}
			}
		}

		// Colon-delimited (e.g. "ERROR: ", "WARN:") or preceded by "level: " / "lvl: "
		if i == 0 || prefix[i-1] == ' ' || prefix[i-1] == '\t' {
			j := i
			for j < len(prefix) && isAlpha(prefix[j]) {
				j++
			}
			if j > i {
				token := prefix[i:j]
				if lvl, ok := matchLevelWord(token); ok {
					// Check if followed by colon: "ERROR:"
					if j < len(prefix) && prefix[j] == ':' {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
					}
					// Check if preceded by "level: " or "lvl: "
					if isPrecededByLevelLabel(prefix[:i]) {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
					}
					// At byte 0 followed by space: "ERROR ..."
					if i == 0 && j < len(prefix) && (prefix[j] == ' ' || prefix[j] == '\t') {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
					}
					// Following a timestamp: "2026-10-16 12:00:00 INFO ..."
					if isPrecededByTimestamp(prefix[:i]) && j < len(prefix) && (prefix[j] == ' ' || prefix[j] == '\t') {
						return LevelResult{Level: lvl, LevelSrc: model.LevelSourceDelimited}, true
					}
				}
			}
		}
	}

	return LevelResult{}, false
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

func (d *SourceDetector) scanKeywordLevel(line []byte) (LevelResult, bool) {
	bestLevel := model.LevelUnknown

	i := 0
	for i < len(line) {
		// Find word boundary start
		for i < len(line) && !isAlpha(line[i]) {
			i++
		}
		if i >= len(line) {
			break
		}
		start := i
		for i < len(line) && isAlpha(line[i]) {
			i++
		}
		word := line[start:i]

		lvl, ok := matchLevelWord(word)
		if ok && lvl > bestLevel {
			// Check benign-context exclusions
			if !isBenignContext(line, start, i) {
				bestLevel = lvl
			}
		}
	}

	if bestLevel != model.LevelUnknown {
		return LevelResult{
			Level:    bestLevel,
			LevelSrc: model.LevelSourceKeyword,
		}, true
	}
	return LevelResult{}, false
}

// -----------------------------------------------------------------------------
// Tier 4 — Heuristic Hints
// -----------------------------------------------------------------------------

func (d *SourceDetector) scanHintLevel(line []byte) (LevelResult, bool) {
	// 1. HTTP Status Codes in Access Logs
	// Format: "METHOD /path HTTP/x.x" STATUS
	if lvl, ok := scanHTTPStatusHint(line); ok {
		return LevelResult{Level: lvl, LevelSrc: model.LevelSourceHint}, true
	}

	// 2. Failure / Exception Keywords -> Error
	if hasFailureHint(line) {
		return LevelResult{Level: model.LevelError, LevelSrc: model.LevelSourceHint}, true
	}

	return LevelResult{}, false
}

func scanHTTPStatusHint(line []byte) (model.Level, bool) {
	// Find HTTP method inside quotes or tokens
	for i := 0; i < len(line)-12; i++ {
		isMethod := false
		var mLen int
		if hasPrefix(line[i:], []byte("\"GET ")) || hasPrefix(line[i:], []byte("GET ")) {
			isMethod = true
			mLen = 4
		} else if hasPrefix(line[i:], []byte("\"POST ")) || hasPrefix(line[i:], []byte("POST ")) {
			isMethod = true
			mLen = 5
		} else if hasPrefix(line[i:], []byte("\"PUT ")) || hasPrefix(line[i:], []byte("PUT ")) {
			isMethod = true
			mLen = 4
		} else if hasPrefix(line[i:], []byte("\"DELETE ")) || hasPrefix(line[i:], []byte("DELETE ")) {
			isMethod = true
			mLen = 7
		} else if hasPrefix(line[i:], []byte("\"PATCH ")) || hasPrefix(line[i:], []byte("PATCH ")) {
			isMethod = true
			mLen = 6
		} else if hasPrefix(line[i:], []byte("\"HEAD ")) || hasPrefix(line[i:], []byte("HEAD ")) {
			isMethod = true
			mLen = 5
		} else if hasPrefix(line[i:], []byte("\"OPTIONS ")) || hasPrefix(line[i:], []byte("OPTIONS ")) {
			isMethod = true
			mLen = 8
		}

		if !isMethod {
			continue
		}

		// Find " HTTP/"
		j := i + mLen
		for j < len(line)-8 && line[j] != '"' && line[j] != '\n' {
			if line[j] == ' ' && hasPrefix(line[j:], []byte(" HTTP/")) {
				// skip to closing quote or space after HTTP/1.x
				k := j + 7
				for k < len(line) && line[k] != '"' && line[k] != ' ' {
					k++
				}
				if k < len(line) && line[k] == '"' {
					k++
				}
				for k < len(line) && (line[k] == ' ' || line[k] == '\t') {
					k++
				}
				// 3-digit status code
				if k+3 <= len(line) {
					c0 := line[k]
					c1 := line[k+1]
					c2 := line[k+2]
					if c0 >= '2' && c0 <= '5' && c1 >= '0' && c1 <= '9' && c2 >= '0' && c2 <= '9' {
						// must have boundary after 3 digits
						if k+3 == len(line) || line[k+3] == ' ' || line[k+3] == '\t' || line[k+3] == '\r' || line[k+3] == '\n' {
							switch c0 {
							case '5':
								return model.LevelError, true
							case '4':
								return model.LevelWarn, true
							case '2', '3':
								return model.LevelInfo, true
							}
						}
					}
				}
			}
			j++
		}
	}
	return model.LevelUnknown, false
}

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
