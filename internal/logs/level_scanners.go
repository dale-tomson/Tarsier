package logs

import (
	"github.com/dale-tomson/Tarsier/internal/model"
)

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

