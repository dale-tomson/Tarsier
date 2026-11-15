package logs

import (
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// formatID identifies each supported timestamp format.
type formatID uint8

const (
	fmtNone formatID = iota
	fmtISO8601
	fmtSlashGo
	fmtSyslog5424
	fmtSyslog3164
	fmtCLF
	fmtRFC2822
	fmtCTime
	fmtMonthName
	fmtNumericDate
	fmtGLog
	fmtEpoch
	fmtTimeOnly
)

// MinPlausibleTS is 2000-01-01T00:00:00Z in UTC nanoseconds.
const MinPlausibleTS int64 = 946684800000000000

// civilDate stores year, month, and day for anchoring time-only timestamps.
type civilDate struct {
	year  int
	month int
	day   int
}

// SourceDetector maintains lock-free, per-source state for timestamp and format detection.
// It is intended to be owned and invoked exclusively by a single source's ingest goroutine.
type SourceDetector struct {
	defaultTZ    *time.Location
	maxScanBytes int

	stickyFormat      formatID
	stickyOffset      int
	stickyMisses      int
	dayFirstAmbiguity int8 // 0 = unsettled, 1 = DD/MM/YYYY, 2 = MM/DD/YYYY
	lastDetectedTS    int64
	lastReadTS        int64
	lastKnownDate     civilDate

	levelCfg    LevelConfig
	lastLevel   model.Level
	lastLevelTS int64
}

// NewSourceDetector initializes a new SourceDetector for a single log source.
func NewSourceDetector(defaultTZ *time.Location, maxScanBytes int) *SourceDetector {
	if defaultTZ == nil {
		defaultTZ = time.Local
	}
	if maxScanBytes <= 0 {
		maxScanBytes = 4096
	}
	return &SourceDetector{
		defaultTZ:    defaultTZ,
		maxScanBytes: maxScanBytes,
		levelCfg: LevelConfig{
			MaxScanBytes:  maxScanBytes,
			KeywordLevels: true,
			Hints:         true,
		},
	}
}

// DetectInput provides input parameters for DetectTimestamp.
type DetectInput struct {
	Line       []byte
	ReadTimeNS int64 // Current ingest read timestamp in UTC nanoseconds
	DockerTSNS int64 // Optional Docker daemon timestamp in UTC nanoseconds (0 for file logs)
	IsDocker   bool
}

// TimestampResult represents the detected timestamp and its source classification.
type TimestampResult struct {
	TS    int64                 // Normalized UTC nanoseconds
	TSSrc model.TimestampSource // 0=ReadTime, 1=DetectedInLine, 2=CarriedPrevLine, 3=DockerTS
}

// parsedTime holds intermediate parsing results before plausibility checks.
type parsedTime struct {
	ts       int64
	consumed int
	fmt      formatID
}

// DetectTimestamp parses an ingested log line into a normalized UTC nanosecond timestamp.
func (d *SourceDetector) DetectTimestamp(in DetectInput) TimestampResult {
	prevLastReadTS := d.lastReadTS
	d.lastReadTS = in.ReadTimeNS

	scanLen := len(in.Line)
	if scanLen > d.maxScanBytes {
		scanLen = d.maxScanBytes
	}
	line := in.Line[:scanLen]

	// 1. Sticky Fast Path
	if d.stickyFormat != fmtNone && d.stickyOffset >= 0 && d.stickyOffset < len(line) {
		start := d.stickyOffset
		// Check for optional leading bracket/parenthesis wrapper
		if start > 0 && (line[start-1] == '[' || line[start-1] == '(') {
			// keep start
		} else if start+1 < len(line) && (line[start] == '[' || line[start] == '(') {
			start++
		}
		if res, ok := d.parseFormatAt(d.stickyFormat, line, start, in.ReadTimeNS); ok && d.isPlausible(res.ts, in.ReadTimeNS) {
			d.stickyMisses = 0
			d.updateLastDetected(res.ts)
			return TimestampResult{TS: res.ts, TSSrc: model.TimestampSourceLine}
		}
		d.stickyMisses++
		if d.stickyMisses >= 20 {
			d.stickyFormat = fmtNone
			d.stickyOffset = 0
		}
	}

	// 2. Structured JSON Object
	trimmed := trimLeadingWhitespace(line)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if res, ok := d.scanJSONTimestamp(trimmed, in.ReadTimeNS); ok && d.isPlausible(res.ts, in.ReadTimeNS) {
			if res.fmt != fmtNone {
				d.stickyFormat = res.fmt
				d.stickyOffset = 0
				d.stickyMisses = 0
			}
			d.updateLastDetected(res.ts)
			return TimestampResult{TS: res.ts, TSSrc: model.TimestampSourceLine}
		}
	}

	// 3. Anchored Prefix Scan
	if res, offset, ok := d.scanAnchoredPrefix(line, in.ReadTimeNS); ok && d.isPlausible(res.ts, in.ReadTimeNS) {
		d.stickyFormat = res.fmt
		d.stickyOffset = offset
		d.stickyMisses = 0
		d.updateLastDetected(res.ts)
		return TimestampResult{TS: res.ts, TSSrc: model.TimestampSourceLine}
	}

	// 4. 256-Byte Window Scan
	windowLen := len(line)
	if windowLen > 256 {
		windowLen = 256
	}
	if res, offset, ok := d.scanWindow(line[:windowLen], in.ReadTimeNS); ok && d.isPlausible(res.ts, in.ReadTimeNS) {
		d.stickyFormat = res.fmt
		d.stickyOffset = offset
		d.stickyMisses = 0
		d.updateLastDetected(res.ts)
		return TimestampResult{TS: res.ts, TSSrc: model.TimestampSourceLine}
	}

	// 5. Logfmt Key Scan
	if res, ok := d.scanLogfmt(line[:windowLen], in.ReadTimeNS); ok && d.isPlausible(res.ts, in.ReadTimeNS) {
		d.updateLastDetected(res.ts)
		return TimestampResult{TS: res.ts, TSSrc: model.TimestampSourceLine}
	}

	// 6. Fallbacks
	if in.IsDocker && in.DockerTSNS > 0 {
		return TimestampResult{TS: in.DockerTSNS, TSSrc: model.TimestampSourceDocker}
	}
	if !in.IsDocker && d.lastDetectedTS > 0 && prevLastReadTS > 0 && (in.ReadTimeNS-prevLastReadTS) <= 5*int64(time.Second) {
		return TimestampResult{TS: d.lastDetectedTS, TSSrc: model.TimestampSourceCarried}
	}
	return TimestampResult{TS: in.ReadTimeNS, TSSrc: model.TimestampSourceReadTime}
}

func (d *SourceDetector) isPlausible(ts int64, readTimeNS int64) bool {
	if ts < MinPlausibleTS {
		return false
	}
	maxFuture := readTimeNS + int64(24*time.Hour)
	return ts <= maxFuture
}

func (d *SourceDetector) updateLastDetected(ts int64) {
	d.lastDetectedTS = ts
	t := time.Unix(0, ts).In(d.defaultTZ)
	d.lastKnownDate = civilDate{
		year:  t.Year(),
		month: int(t.Month()),
		day:   t.Day(),
	}
}

// -----------------------------------------------------------------------------
// Anchored & Window Scanners
// -----------------------------------------------------------------------------

func (d *SourceDetector) scanAnchoredPrefix(line []byte, readTimeNS int64) (parsedTime, int, bool) {
	prefixes := [4]int{0, -1, -1, -1}
	numPrefixes := 1

	if len(line) > 1 && (line[0] == '[' || line[0] == '(') {
		prefixes[numPrefixes] = 1
		numPrefixes++
	}

	// Check Syslog <PRI> prefix at start
	if len(line) > 3 && line[0] == '<' {
		idx := 1
		for idx < len(line) && line[idx] >= '0' && line[idx] <= '9' && idx < 5 {
			idx++
		}
		if idx < len(line) && line[idx] == '>' {
			idx++
			// Syslog RFC 5424: <PRI>1
			if idx+1 < len(line) && line[idx] == '1' && line[idx+1] == ' ' {
				idx += 2
			}
			prefixes[numPrefixes] = idx
			numPrefixes++
		}
	}

	// Leading space-delimited tokens (e.g. "hostname app: ")
	spaces := 0
	for i := 0; i < len(line) && spaces < 2; i++ {
		if line[i] == ' ' {
			spaces++
			if i+1 < len(line) && numPrefixes < len(prefixes) {
				prefixes[numPrefixes] = i + 1
				numPrefixes++
			}
		}
	}

	for p := 0; p < numPrefixes; p++ {
		offset := prefixes[p]
		if offset < 0 || offset >= len(line) {
			continue
		}
		if res, ok := d.tryAllFormats(line, offset, readTimeNS); ok {
			return res, offset, true
		}
	}

	return parsedTime{}, 0, false
}

func (d *SourceDetector) scanWindow(line []byte, readTimeNS int64) (parsedTime, int, bool) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		// Optimization: fast skip if character cannot be the start of a format
		isCandidate := (c >= '0' && c <= '9') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			c == '[' || c == '<'
		if !isCandidate {
			continue
		}

		start := i
		if c == '[' && i+1 < len(line) {
			start = i + 1
		}

		if res, ok := d.tryAllFormats(line, start, readTimeNS); ok {
			// Reject mid-line epoch matches
			if res.fmt == fmtEpoch && start > 0 {
				continue
			}
			return res, start, true
		}
	}
	return parsedTime{}, 0, false
}

func (d *SourceDetector) tryAllFormats(line []byte, offset int, readTimeNS int64) (parsedTime, bool) {
	if offset < 0 || offset >= len(line) {
		return parsedTime{}, false
	}
	sub := line[offset:]

	// 1. Syslog 5424 (starts with <)
	if len(sub) > 4 && sub[0] == '<' {
		if res, ok := d.parseSyslog5424(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 2. ISO 8601 (starts with 4-digit year 19xx or 20xx)
	if len(sub) >= 10 && (sub[0] == '1' || sub[0] == '2') {
		if res, ok := d.parseISO8601(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 3. Slash / Go Style (YYYY/MM/DD)
	if len(sub) >= 10 && (sub[0] == '1' || sub[0] == '2') {
		if res, ok := d.parseSlashGo(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 4. Common Log Format ([DD/Mon/YYYY:...)
	if len(sub) >= 20 {
		if res, ok := d.parseCLF(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 5. HTTP-date / RFC 2822 / RFC 1123 / ctime
	if len(sub) >= 15 {
		if res, ok := d.parseRFC2822(sub, readTimeNS); ok {
			return res, true
		}
		if res, ok := d.parseCTime(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 6. Month Name Dates (16 Oct 2026 or Oct 16, 2026)
	if len(sub) >= 15 {
		if res, ok := d.parseMonthName(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 7. Syslog RFC 3164 (Oct 16 03:47:12)
	if len(sub) >= 15 {
		if res, ok := d.parseSyslog3164(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 8. Numeric Date (DD/MM/YYYY or MM/DD/YYYY)
	if len(sub) >= 16 {
		if res, ok := d.parseNumericDate(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 9. glog / klog (E1016 03:47:12...)
	if len(sub) >= 16 {
		if res, ok := d.parseGLog(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 10. Time-only (03:47:12)
	if offset == 0 || (offset == 1 && (line[0] == '[' || line[0] == '(')) {
		if res, ok := d.parseTimeOnly(sub, readTimeNS); ok {
			return res, true
		}
	}

	// 11. Epoch (only at line start)
	if offset == 0 || (offset == 1 && (line[0] == '[' || line[0] == '(')) {
		if res, ok := d.parseEpoch(sub); ok {
			return res, true
		}
	}

	return parsedTime{}, false
}

func (d *SourceDetector) parseFormatAt(fmt formatID, line []byte, offset int, readTimeNS int64) (parsedTime, bool) {
	if offset < 0 || offset >= len(line) {
		return parsedTime{}, false
	}
	sub := line[offset:]
	switch fmt {
	case fmtISO8601:
		return d.parseISO8601(sub, readTimeNS)
	case fmtSlashGo:
		return d.parseSlashGo(sub, readTimeNS)
	case fmtSyslog5424:
		return d.parseSyslog5424(sub, readTimeNS)
	case fmtSyslog3164:
		return d.parseSyslog3164(sub, readTimeNS)
	case fmtCLF:
		return d.parseCLF(sub, readTimeNS)
	case fmtRFC2822:
		return d.parseRFC2822(sub, readTimeNS)
	case fmtCTime:
		return d.parseCTime(sub, readTimeNS)
	case fmtMonthName:
		return d.parseMonthName(sub, readTimeNS)
	case fmtNumericDate:
		return d.parseNumericDate(sub, readTimeNS)
	case fmtGLog:
		return d.parseGLog(sub, readTimeNS)
	case fmtEpoch:
		return d.parseEpoch(sub)
	case fmtTimeOnly:
		return d.parseTimeOnly(sub, readTimeNS)
	default:
		return parsedTime{}, false
	}
}

// -----------------------------------------------------------------------------
// JSON Top-Level Key Scanner (Zero Alloc)
// -----------------------------------------------------------------------------

func (d *SourceDetector) scanJSONTimestamp(line []byte, readTimeNS int64) (parsedTime, bool) {
	i := 1 // skip leading '{'
	for i < len(line) {
		// find opening quote for key
		for i < len(line) && line[i] != '"' && line[i] != '}' {
			i++
		}
		if i >= len(line) || line[i] == '}' {
			break
		}
		i++ // skip opening quote
		keyStart := i
		for i < len(line) && line[i] != '"' && line[i] != '\\' {
			i++
		}
		if i >= len(line) || line[i] != '"' {
			break
		}
		key := line[keyStart:i]
		i++ // skip closing quote

		// skip whitespace to ':'
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) || line[i] != ':' {
			break
		}
		i++ // skip ':'

		// skip whitespace to value
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) {
			break
		}

		isTimeKey := isJSONTimeKey(key)

		// Parse value
		if line[i] == '"' {
			i++ // skip opening quote
			valStart := i
			for i < len(line) && line[i] != '"' {
				i++
			}
			val := line[valStart:i]
			if i < len(line) && line[i] == '"' {
				i++
			}
			if isTimeKey && len(val) > 0 {
				// Try all formats on val
				if res, ok := d.tryAllFormats(val, 0, readTimeNS); ok {
					return res, true
				}
				// Also try epoch inside quoted string
				if res, ok := d.parseEpoch(val); ok {
					return res, true
				}
			}
		} else if line[i] >= '0' && line[i] <= '9' {
			valStart := i
			for i < len(line) && ((line[i] >= '0' && line[i] <= '9') || line[i] == '.') {
				i++
			}
			val := line[valStart:i]
			if isTimeKey && len(val) > 0 {
				if res, ok := d.parseEpoch(val); ok {
					return res, true
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
	return parsedTime{}, false
}

func isJSONTimeKey(k []byte) bool {
	switch len(k) {
	case 1:
		return k[0] == 't'
	case 2:
		return k[0] == 't' && k[1] == 's'
	case 4:
		if k[0] == 't' && k[1] == 'i' && k[2] == 'm' && k[3] == 'e' {
			return true
		}
		if k[0] == 'd' && k[1] == 'a' && k[2] == 't' && k[3] == 'e' {
			return true
		}
	case 8:
		if string(k) == "datetime" {
			return true
		}
	case 9:
		if string(k) == "timestamp" || string(k) == "eventTime" {
			return true
		}
	case 10:
		if string(k) == "@timestamp" {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Logfmt Scanner
// -----------------------------------------------------------------------------

func (d *SourceDetector) scanLogfmt(line []byte, readTimeNS int64) (parsedTime, bool) {
	for i := 0; i < len(line); i++ {
		// Check for word boundary before key
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' && line[i-1] != '[' && line[i-1] != '(' {
			continue
		}
		keyLen := 0
		if hasPrefix(line[i:], []byte("ts=")) {
			keyLen = 3
		} else if hasPrefix(line[i:], []byte("time=")) {
			keyLen = 5
		} else if hasPrefix(line[i:], []byte("timestamp=")) {
			keyLen = 10
		} else if hasPrefix(line[i:], []byte("@timestamp=")) {
			keyLen = 11
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
			if res, ok := d.tryAllFormats(val, 0, readTimeNS); ok {
				return res, true
			}
			if res, ok := d.parseEpoch(val); ok {
				return res, true
			}
		}
	}
	return parsedTime{}, false
}

// -----------------------------------------------------------------------------
// Format Parsers
// -----------------------------------------------------------------------------

// 1. ISO 8601 / RFC 3339
func (d *SourceDetector) parseISO8601(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 15 {
		return parsedTime{}, false
	}

	// YYYY-MM-DD
	if len(b) < 19 || b[4] != '-' || b[7] != '-' {
		// Check compact: YYYYMMDDTHHMMSSZ (15+ chars)
		if len(b) >= 15 && b[8] == 'T' {
			return d.parseCompactISO(b)
		}
		return parsedTime{}, false
	}
	year, ok1 := parseDigits(b[0:4], 4)
	month, ok2 := parseDigits(b[5:7], 2)
	day, ok3 := parseDigits(b[8:10], 2)
	if !ok1 || !ok2 || !ok3 || !isValidDate(year, month, day) {
		return parsedTime{}, false
	}

	sep := b[10]
	if sep != 'T' && sep != ' ' {
		return parsedTime{}, false
	}

	// HH:MM:SS
	if b[13] != ':' || b[16] != ':' {
		return parsedTime{}, false
	}
	hour, ok4 := parseDigits(b[11:13], 2)
	minute, ok5 := parseDigits(b[14:16], 2)
	second, ok6 := parseDigits(b[17:19], 2)
	if !ok4 || !ok5 || !ok6 || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	idx := 19
	nsec := 0
	if idx < len(b) && (b[idx] == '.' || b[idx] == ',') {
		frac, consumed, okFrac := parseFraction(b[idx:])
		if okFrac {
			nsec = frac
			idx += consumed
		}
	}

	// Optional timezone (with optional space separator, e.g. " +0530")
	offsetSec := 0
	hasOffset := false
	tzIdx := idx
	if tzIdx < len(b) && b[tzIdx] == ' ' && tzIdx+1 < len(b) && (b[tzIdx+1] == '+' || b[tzIdx+1] == '-') {
		tzIdx++
	}
	if tzIdx < len(b) {
		off, consumed, okOff := parseTimezoneOffset(b[tzIdx:])
		if okOff {
			offsetSec = off
			hasOffset = true
			idx = tzIdx + consumed
			// Skip trailing zone name (e.g. " IST") and Go monotonic " m=+..."
			for idx < len(b) && b[idx] != '\n' && b[idx] != '\r' && b[idx] != ']' && b[idx] != ')' {
				idx++
			}
		}
	}

	var ts int64
	if hasOffset {
		ts = utcToNanoseconds(year, month, day, hour, minute, second, nsec, offsetSec)
	} else {
		ts = localToUTC(year, month, day, hour, minute, second, nsec, d.defaultTZ)
	}

	return parsedTime{ts: ts, consumed: idx, fmt: fmtISO8601}, true
}

func (d *SourceDetector) parseCompactISO(b []byte) (parsedTime, bool) {
	year, ok1 := parseDigits(b[0:4], 4)
	month, ok2 := parseDigits(b[4:6], 2)
	day, ok3 := parseDigits(b[6:8], 2)
	if !ok1 || !ok2 || !ok3 || !isValidDate(year, month, day) {
		return parsedTime{}, false
	}
	if b[8] != 'T' {
		return parsedTime{}, false
	}
	hour, ok4 := parseDigits(b[9:11], 2)
	minute, ok5 := parseDigits(b[11:13], 2)
	second, ok6 := parseDigits(b[13:15], 2)
	if !ok4 || !ok5 || !ok6 || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}
	idx := 15
	offsetSec := 0
	hasOffset := false
	if idx < len(b) {
		off, consumed, okOff := parseTimezoneOffset(b[idx:])
		if okOff {
			offsetSec = off
			hasOffset = true
			idx += consumed
		}
	}
	var ts int64
	if hasOffset {
		ts = utcToNanoseconds(year, month, day, hour, minute, second, 0, offsetSec)
	} else {
		ts = localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	}
	return parsedTime{ts: ts, consumed: idx, fmt: fmtISO8601}, true
}

// 2. Slash / Go / Python Style (YYYY/MM/DD or YYYY-MM-DD ... +0530 IST m=+0.01)
func (d *SourceDetector) parseSlashGo(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 19 {
		return parsedTime{}, false
	}
	isSlash := b[4] == '/' && b[7] == '/'
	isDash := b[4] == '-' && b[7] == '-'
	if !isSlash && !isDash {
		return parsedTime{}, false
	}

	year, ok1 := parseDigits(b[0:4], 4)
	month, ok2 := parseDigits(b[5:7], 2)
	day, ok3 := parseDigits(b[8:10], 2)
	if !ok1 || !ok2 || !ok3 || !isValidDate(year, month, day) {
		return parsedTime{}, false
	}

	sep := b[10]
	if sep != ' ' && sep != 'T' {
		return parsedTime{}, false
	}

	if b[13] != ':' || b[16] != ':' {
		return parsedTime{}, false
	}
	hour, ok4 := parseDigits(b[11:13], 2)
	minute, ok5 := parseDigits(b[14:16], 2)
	second, ok6 := parseDigits(b[17:19], 2)
	if !ok4 || !ok5 || !ok6 || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	idx := 19
	nsec := 0
	if idx < len(b) && (b[idx] == '.' || b[idx] == ',') {
		frac, consumed, okFrac := parseFraction(b[idx:])
		if okFrac {
			nsec = frac
			idx += consumed
		}
	}

	// Check for space followed by timezone offset (e.g. "+0530" or "-0400")
	offsetSec := 0
	hasOffset := false
	if idx < len(b) && b[idx] == ' ' {
		tzIdx := idx + 1
		off, consumed, okOff := parseTimezoneOffset(b[tzIdx:])
		if okOff {
			offsetSec = off
			hasOffset = true
			idx = tzIdx + consumed
			// Skip trailing zone name (e.g. " IST") and Go monotonic " m=+..."
			for idx < len(b) && b[idx] != '\n' && b[idx] != '\r' && b[idx] != ']' && b[idx] != ')' {
				idx++
			}
		}
	}

	var ts int64
	if hasOffset {
		ts = utcToNanoseconds(year, month, day, hour, minute, second, nsec, offsetSec)
	} else {
		ts = localToUTC(year, month, day, hour, minute, second, nsec, d.defaultTZ)
	}

	return parsedTime{ts: ts, consumed: idx, fmt: fmtSlashGo}, true
}

// 3. Syslog RFC 5424 (<34>1 2026-10-16T03:47:12.003Z host app ...)
func (d *SourceDetector) parseSyslog5424(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 24 || b[0] != '<' {
		return parsedTime{}, false
	}
	idx := 1
	for idx < len(b) && b[idx] >= '0' && b[idx] <= '9' && idx < 5 {
		idx++
	}
	if idx >= len(b) || b[idx] != '>' {
		return parsedTime{}, false
	}
	idx++ // skip '>'

	// Version digit + space: e.g. "1 "
	if idx+1 >= len(b) || b[idx] < '0' || b[idx] > '9' || b[idx+1] != ' ' {
		return parsedTime{}, false
	}
	idx += 2

	res, ok := d.parseISO8601(b[idx:], readTimeNS)
	if !ok {
		return parsedTime{}, false
	}
	return parsedTime{ts: res.ts, consumed: idx + res.consumed, fmt: fmtSyslog5424}, true
}

// 4. Syslog RFC 3164 (Oct 16 03:47:12)
func (d *SourceDetector) parseSyslog3164(b []byte, readTimeNS int64) (parsedTime, bool) {
	idx := 0
	// Optional <PRI> prefix
	if len(b) > 3 && b[0] == '<' {
		p := 1
		for p < len(b) && b[p] >= '0' && b[p] <= '9' && p < 5 {
			p++
		}
		if p < len(b) && b[p] == '>' {
			idx = p + 1
		}
	}

	sub := b[idx:]
	if len(sub) < 15 {
		return parsedTime{}, false
	}

	month, okM := parseMonth(sub[:3])
	if !okM || sub[3] != ' ' {
		return parsedTime{}, false
	}

	dayIdx := 4
	if sub[dayIdx] == ' ' {
		dayIdx++
	}
	day, consumed, okD := parse1or2Digits(sub[dayIdx:])
	if !okD || day < 1 || day > 31 {
		return parsedTime{}, false
	}
	tIdx := dayIdx + consumed
	if tIdx >= len(sub) || sub[tIdx] != ' ' {
		return parsedTime{}, false
	}
	tIdx++ // skip space

	timeSub := sub[tIdx:]
	if len(timeSub) < 8 || timeSub[2] != ':' || timeSub[5] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(timeSub[0:2], 2)
	minute, okMin := parseDigits(timeSub[3:5], 2)
	second, okS := parseDigits(timeSub[6:8], 2)
	if !okH || !okMin || !okS || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	consumedTotal := idx + tIdx + 8

	// Infer year from readTimeNS in defaultTZ
	nowInTZ := time.Unix(0, readTimeNS).In(d.defaultTZ)
	year := nowInTZ.Year()

	ts := localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	// If resulting timestamp is > 24h ahead of readTimeNS, step back 1 year
	if ts > readTimeNS+int64(24*time.Hour) {
		ts = localToUTC(year-1, month, day, hour, minute, second, 0, d.defaultTZ)
	}

	return parsedTime{ts: ts, consumed: consumedTotal, fmt: fmtSyslog3164}, true
}

// 5. Common Log Format (CLF: [16/Oct/2026:03:47:12 +0530])
func (d *SourceDetector) parseCLF(b []byte, readTimeNS int64) (parsedTime, bool) {
	idx := 0
	hasBracket := false
	if b[0] == '[' {
		hasBracket = true
		idx = 1
	}
	sub := b[idx:]
	if len(sub) < 26 {
		return parsedTime{}, false
	}

	day, okD := parseDigits(sub[0:2], 2)
	if !okD || sub[2] != '/' {
		return parsedTime{}, false
	}
	month, okM := parseMonth(sub[3:6])
	if !okM || sub[6] != '/' {
		return parsedTime{}, false
	}
	year, okY := parseDigits(sub[7:11], 4)
	if !okY || sub[11] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(sub[12:14], 2)
	if !okH || sub[14] != ':' {
		return parsedTime{}, false
	}
	minute, okMin := parseDigits(sub[15:17], 2)
	if !okMin || sub[17] != ':' {
		return parsedTime{}, false
	}
	second, okS := parseDigits(sub[18:20], 2)
	if !okS || sub[20] != ' ' {
		return parsedTime{}, false
	}
	if !isValidDate(year, month, day) || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	offsetSec, consumed, okOff := parseTimezoneOffset(sub[21:])
	if !okOff {
		return parsedTime{}, false
	}

	consumedTotal := idx + 21 + consumed
	if hasBracket && consumedTotal < len(b) && b[consumedTotal] == ']' {
		consumedTotal++
	}

	ts := utcToNanoseconds(year, month, day, hour, minute, second, 0, offsetSec)
	return parsedTime{ts: ts, consumed: consumedTotal, fmt: fmtCLF}, true
}

// 6. HTTP-date / RFC 2822 / RFC 1123 (Fri, 16 Oct 2026 03:47:12 +0530)
func (d *SourceDetector) parseRFC2822(b []byte, readTimeNS int64) (parsedTime, bool) {
	idx := 0
	// Optional Day-of-week: "Fri, "
	if len(b) > 5 && isAlpha(b[0]) && isAlpha(b[1]) && isAlpha(b[2]) && b[3] == ',' && b[4] == ' ' {
		idx = 5
	}
	sub := b[idx:]
	if len(sub) < 20 {
		return parsedTime{}, false
	}

	day, consumed, okD := parse1or2Digits(sub)
	if !okD || sub[consumed] != ' ' {
		return parsedTime{}, false
	}
	mIdx := consumed + 1
	if mIdx+3 > len(sub) {
		return parsedTime{}, false
	}
	month, okM := parseMonth(sub[mIdx : mIdx+3])
	if !okM || sub[mIdx+3] != ' ' {
		return parsedTime{}, false
	}
	yIdx := mIdx + 4
	if yIdx+4 > len(sub) {
		return parsedTime{}, false
	}
	year, okY := parseDigits(sub[yIdx:yIdx+4], 4)
	if !okY || sub[yIdx+4] != ' ' {
		return parsedTime{}, false
	}

	tIdx := yIdx + 5
	if tIdx+8 > len(sub) || sub[tIdx+2] != ':' || sub[tIdx+5] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(sub[tIdx:tIdx+2], 2)
	minute, okMin := parseDigits(sub[tIdx+3:tIdx+5], 2)
	second, okS := parseDigits(sub[tIdx+6:tIdx+8], 2)
	if !okH || !okMin || !okS || !isValidDate(year, month, day) || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	consumedTotal := idx + tIdx + 8
	offsetSec := 0
	hasOffset := false
	if consumedTotal < len(b) && b[consumedTotal] == ' ' {
		off, consumedOff, okOff := parseTimezoneOffset(b[consumedTotal+1:])
		if okOff {
			offsetSec = off
			hasOffset = true
			consumedTotal += 1 + consumedOff
		}
	}

	var ts int64
	if hasOffset {
		ts = utcToNanoseconds(year, month, day, hour, minute, second, 0, offsetSec)
	} else {
		ts = localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	}

	return parsedTime{ts: ts, consumed: consumedTotal, fmt: fmtRFC2822}, true
}

// 7. ctime / asctime (Fri Oct 16 03:47:12 2026)
func (d *SourceDetector) parseCTime(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 24 {
		return parsedTime{}, false
	}
	// Weekday: 3 letters + space
	if !isAlpha(b[0]) || !isAlpha(b[1]) || !isAlpha(b[2]) || b[3] != ' ' {
		return parsedTime{}, false
	}
	// Month: 3 letters + space
	month, okM := parseMonth(b[4:7])
	if !okM || b[7] != ' ' {
		return parsedTime{}, false
	}
	dayIdx := 8
	if b[dayIdx] == ' ' {
		dayIdx++
	}
	day, consumed, okD := parse1or2Digits(b[dayIdx:])
	if !okD {
		return parsedTime{}, false
	}
	tIdx := dayIdx + consumed
	if tIdx >= len(b) || b[tIdx] != ' ' {
		return parsedTime{}, false
	}
	tIdx++

	if tIdx+8 > len(b) || b[tIdx+2] != ':' || b[tIdx+5] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(b[tIdx:tIdx+2], 2)
	minute, okMin := parseDigits(b[tIdx+3:tIdx+5], 2)
	second, okS := parseDigits(b[tIdx+6:tIdx+8], 2)
	if !okH || !okMin || !okS {
		return parsedTime{}, false
	}

	yIdx := tIdx + 8
	if yIdx >= len(b) || b[yIdx] != ' ' {
		return parsedTime{}, false
	}
	yIdx++
	if yIdx+4 > len(b) {
		return parsedTime{}, false
	}
	year, okY := parseDigits(b[yIdx:yIdx+4], 4)
	if !okY || !isValidDate(year, month, day) || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	ts := localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	return parsedTime{ts: ts, consumed: yIdx + 4, fmt: fmtCTime}, true
}

// 8. Month-name Dates (16 Oct 2026 03:47:12 or Oct 16, 2026 3:47:12 AM)
func (d *SourceDetector) parseMonthName(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 18 {
		return parsedTime{}, false
	}
	// Variant A: 16 Oct 2026 ...
	if b[0] >= '0' && b[0] <= '9' {
		day, consumed, okD := parse1or2Digits(b)
		if !okD || b[consumed] != ' ' {
			return parsedTime{}, false
		}
		mIdx := consumed + 1
		month, okM := parseMonth(b[mIdx : mIdx+3])
		if !okM || b[mIdx+3] != ' ' {
			return parsedTime{}, false
		}
		yIdx := mIdx + 4
		year, okY := parseDigits(b[yIdx:yIdx+4], 4)
		if !okY || b[yIdx+4] != ' ' {
			return parsedTime{}, false
		}
		tIdx := yIdx + 5
		hour, okH := parseDigits(b[tIdx:tIdx+2], 2)
		if !okH || b[tIdx+2] != ':' {
			return parsedTime{}, false
		}
		minute, okMin := parseDigits(b[tIdx+3:tIdx+5], 2)
		if !okMin || b[tIdx+5] != ':' {
			return parsedTime{}, false
		}
		second, okS := parseDigits(b[tIdx+6:tIdx+8], 2)
		if !okS || !isValidDate(year, month, day) || !isValidTime(hour, minute, second) {
			return parsedTime{}, false
		}
		ts := localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
		return parsedTime{ts: ts, consumed: tIdx + 8, fmt: fmtMonthName}, true
	}

	// Variant B: Oct 16, 2026 3:47:12 AM
	month, okM := parseMonth(b[0:3])
	if !okM || b[3] != ' ' {
		return parsedTime{}, false
	}
	dayIdx := 4
	if b[dayIdx] == ' ' {
		dayIdx++
	}
	day, consumed, okD := parse1or2Digits(b[dayIdx:])
	if !okD {
		return parsedTime{}, false
	}
	idx := dayIdx + consumed
	if idx < len(b) && b[idx] == ',' {
		idx++
	}
	if idx >= len(b) || b[idx] != ' ' {
		return parsedTime{}, false
	}
	idx++ // skip space
	year, okY := parseDigits(b[idx:idx+4], 4)
	if !okY || b[idx+4] != ' ' {
		return parsedTime{}, false
	}
	idx += 5

	hour, consumedH, okH := parse1or2Digits(b[idx:])
	if !okH || b[idx+consumedH] != ':' {
		return parsedTime{}, false
	}
	idx += consumedH + 1
	minute, okMin := parseDigits(b[idx:idx+2], 2)
	if !okMin || b[idx+2] != ':' {
		return parsedTime{}, false
	}
	idx += 3
	second, okS := parseDigits(b[idx:idx+2], 2)
	if !okS {
		return parsedTime{}, false
	}
	idx += 2

	// Check AM/PM
	if idx < len(b) && b[idx] == ' ' {
		if idx+3 <= len(b) {
			isAM := (b[idx+1] == 'A' || b[idx+1] == 'a') && (b[idx+2] == 'M' || b[idx+2] == 'm')
			isPM := (b[idx+1] == 'P' || b[idx+1] == 'p') && (b[idx+2] == 'M' || b[idx+2] == 'm')
			if isAM {
				if hour == 12 {
					hour = 0
				}
				idx += 3
			} else if isPM {
				if hour < 12 {
					hour += 12
				}
				idx += 3
			}
		}
	}

	if !isValidDate(year, month, day) || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	ts := localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	return parsedTime{ts: ts, consumed: idx, fmt: fmtMonthName}, true
}

// 9. Numeric Date (16/10/2026 or 10/16/2026 or 16-10-2026)
func (d *SourceDetector) parseNumericDate(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 19 {
		return parsedTime{}, false
	}
	part1, consumed1, ok1 := parse1or2Digits(b)
	if !ok1 {
		return parsedTime{}, false
	}
	sep := b[consumed1]
	if sep != '/' && sep != '-' {
		return parsedTime{}, false
	}
	sub2 := b[consumed1+1:]
	part2, consumed2, ok2 := parse1or2Digits(sub2)
	if !ok2 || sub2[consumed2] != sep {
		return parsedTime{}, false
	}
	yIdx := consumed1 + 1 + consumed2 + 1
	if yIdx+4 > len(b) {
		return parsedTime{}, false
	}
	year, okY := parseDigits(b[yIdx:yIdx+4], 4)
	if !okY || b[yIdx+4] != ' ' {
		return parsedTime{}, false
	}

	tIdx := yIdx + 5
	if tIdx+8 > len(b) || b[tIdx+2] != ':' || b[tIdx+5] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(b[tIdx:tIdx+2], 2)
	minute, okMin := parseDigits(b[tIdx+3:tIdx+5], 2)
	second, okS := parseDigits(b[tIdx+6:tIdx+8], 2)
	if !okH || !okMin || !okS || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	// Resolve DD/MM vs MM/DD ambiguity
	day := 0
	month := 0
	if part1 > 12 && part2 <= 12 {
		day = part1
		month = part2
		d.dayFirstAmbiguity = 1 // learned DD/MM
	} else if part2 > 12 && part1 <= 12 {
		month = part1
		day = part2
		d.dayFirstAmbiguity = 2 // learned MM/DD
	} else if part1 <= 12 && part2 <= 12 {
		if d.dayFirstAmbiguity == 1 {
			day = part1
			month = part2
		} else if d.dayFirstAmbiguity == 2 {
			month = part1
			day = part2
		} else {
			// Unsettled ambiguity: reject to prevent misinterpreting dates
			return parsedTime{}, false
		}
	} else {
		return parsedTime{}, false
	}

	if !isValidDate(year, month, day) {
		return parsedTime{}, false
	}

	ts := localToUTC(year, month, day, hour, minute, second, 0, d.defaultTZ)
	return parsedTime{ts: ts, consumed: tIdx + 8, fmt: fmtNumericDate}, true
}

// 10. glog / klog (E1016 03:47:12.123456 ...)
func (d *SourceDetector) parseGLog(b []byte, readTimeNS int64) (parsedTime, bool) {
	if len(b) < 16 {
		return parsedTime{}, false
	}
	sev := b[0]
	if sev != 'I' && sev != 'W' && sev != 'E' && sev != 'F' {
		return parsedTime{}, false
	}
	month, okM := parseDigits(b[1:3], 2)
	day, okD := parseDigits(b[3:5], 2)
	if !okM || !okD || b[5] != ' ' {
		return parsedTime{}, false
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return parsedTime{}, false
	}

	if b[8] != ':' || b[11] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(b[6:8], 2)
	minute, okMin := parseDigits(b[9:11], 2)
	second, okS := parseDigits(b[12:14], 2)
	if !okH || !okMin || !okS || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	idx := 14
	nsec := 0
	if idx < len(b) && b[idx] == '.' {
		frac, consumed, okFrac := parseFraction(b[idx:])
		if okFrac {
			nsec = frac
			idx += consumed
		}
	}

	nowInTZ := time.Unix(0, readTimeNS).In(d.defaultTZ)
	year := nowInTZ.Year()
	ts := localToUTC(year, month, day, hour, minute, second, nsec, d.defaultTZ)
	if ts > readTimeNS+int64(24*time.Hour) {
		ts = localToUTC(year-1, month, day, hour, minute, second, nsec, d.defaultTZ)
	}

	return parsedTime{ts: ts, consumed: idx, fmt: fmtGLog}, true
}

// 11. Epoch Timestamps (10, 13, 16, 19 digits or float seconds)
func (d *SourceDetector) parseEpoch(b []byte) (parsedTime, bool) {
	if len(b) < 10 {
		return parsedTime{}, false
	}

	idx := 0
	for idx < len(b) && b[idx] >= '0' && b[idx] <= '9' {
		idx++
	}
	if idx < 10 {
		return parsedTime{}, false
	}

	// Guard: trailing alphanumeric characters mean this is likely an ID or token, not an epoch
	if idx < len(b) && isAlpha(b[idx]) {
		return parsedTime{}, false
	}

	digits := b[:idx]

	// Check for fractional seconds (e.g. 1760579232.123)
	if idx < len(b) && b[idx] == '.' {
		fracStart := idx
		fIdx := idx + 1
		for fIdx < len(b) && b[fIdx] >= '0' && b[fIdx] <= '9' {
			fIdx++
		}
		if fIdx > fracStart+1 && len(digits) == 10 {
			frac, consumed, okFrac := parseFraction(b[fracStart:fIdx])
			if okFrac {
				sec, okS := parseUint64(digits)
				if okS {
					ts := int64(sec)*1e9 + int64(frac)
					return parsedTime{ts: ts, consumed: fracStart + consumed, fmt: fmtEpoch}, true
				}
			}
		}
	}

	num, ok := parseUint64(digits)
	if !ok {
		return parsedTime{}, false
	}

	var ts int64
	switch len(digits) {
	case 10: // Seconds
		ts = int64(num) * 1e9
	case 13: // Milliseconds
		ts = int64(num) * 1e6
	case 16: // Microseconds
		ts = int64(num) * 1e3
	case 19: // Nanoseconds
		ts = int64(num)
	default:
		return parsedTime{}, false
	}

	return parsedTime{ts: ts, consumed: idx, fmt: fmtEpoch}, true
}

// 12. Time-Only (03:47:12.123 or [03:47:12])
func (d *SourceDetector) parseTimeOnly(b []byte, readTimeNS int64) (parsedTime, bool) {
	idx := 0
	hasBracket := false
	if b[0] == '[' {
		hasBracket = true
		idx = 1
	}
	sub := b[idx:]
	if len(sub) < 8 || sub[2] != ':' || sub[5] != ':' {
		return parsedTime{}, false
	}
	hour, okH := parseDigits(sub[0:2], 2)
	minute, okMin := parseDigits(sub[3:5], 2)
	second, okS := parseDigits(sub[6:8], 2)
	if !okH || !okMin || !okS || !isValidTime(hour, minute, second) {
		return parsedTime{}, false
	}

	consumed := idx + 8
	nsec := 0
	if consumed < len(b) && b[consumed] == '.' {
		frac, fracLen, okFrac := parseFraction(b[consumed:])
		if okFrac {
			nsec = frac
			consumed += fracLen
		}
	}
	if hasBracket && consumed < len(b) && b[consumed] == ']' {
		consumed++
	}

	year, month, day := d.lastKnownDate.year, d.lastKnownDate.month, d.lastKnownDate.day
	if year == 0 {
		nowInTZ := time.Unix(0, readTimeNS).In(d.defaultTZ)
		year = nowInTZ.Year()
		month = int(nowInTZ.Month())
		day = nowInTZ.Day()
	}

	ts := localToUTC(year, month, day, hour, minute, second, nsec, d.defaultTZ)
	// If result lands > 1h in the future, roll back 1 day (midnight rollover)
	if ts > readTimeNS+int64(1*time.Hour) {
		ts -= int64(24 * time.Hour)
	}

	return parsedTime{ts: ts, consumed: consumed, fmt: fmtTimeOnly}, true
}

// -----------------------------------------------------------------------------
// Pure Arithmetic Date & Time Utilities (Zero Alloc)
// -----------------------------------------------------------------------------

func parseDigits(b []byte, count int) (int, bool) {
	if len(b) < count {
		return 0, false
	}
	val := 0
	for i := 0; i < count; i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		val = val*10 + int(c-'0')
	}
	return val, true
}

func parse1or2Digits(b []byte) (val int, consumed int, ok bool) {
	if len(b) == 0 || b[0] < '0' || b[0] > '9' {
		return 0, 0, false
	}
	val = int(b[0] - '0')
	if len(b) > 1 && b[1] >= '0' && b[1] <= '9' {
		val = val*10 + int(b[1]-'0')
		return val, 2, true
	}
	return val, 1, true
}

func parseUint64(b []byte) (uint64, bool) {
	var val uint64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		val = val*10 + uint64(c-'0')
	}
	return val, true
}

func parseFraction(b []byte) (nsec int, consumed int, ok bool) {
	if len(b) == 0 || (b[0] != '.' && b[0] != ',') {
		return 0, 0, false
	}
	idx := 1
	digits := 0
	count := 0
	for idx < len(b) && b[idx] >= '0' && b[idx] <= '9' {
		if count < 9 {
			digits = digits*10 + int(b[idx]-'0')
			count++
		}
		idx++
	}
	if count == 0 {
		return 0, 0, false
	}
	for count < 9 {
		digits *= 10
		count++
	}
	return digits, idx, true
}

func parseTimezoneOffset(b []byte) (offsetSec int, consumed int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	if b[0] == 'Z' || b[0] == 'z' {
		return 0, 1, true
	}
	if len(b) >= 3 {
		c0 := b[0] | 0x20
		c1 := b[1] | 0x20
		c2 := b[2] | 0x20
		if (c0 == 'g' && c1 == 'm' && c2 == 't') || (c0 == 'u' && c1 == 't' && c2 == 'c') {
			return 0, 3, true
		}
	}
	if b[0] != '+' && b[0] != '-' {
		return 0, 0, false
	}
	sign := 1
	if b[0] == '-' {
		sign = -1
	}
	if len(b) >= 6 && b[3] == ':' {
		h, okH := parseDigits(b[1:3], 2)
		m, okM := parseDigits(b[4:6], 2)
		if okH && okM && h < 24 && m < 60 {
			return sign * (h*3600 + m*60), 6, true
		}
	}
	if len(b) >= 5 {
		h, okH := parseDigits(b[1:3], 2)
		m, okM := parseDigits(b[3:5], 2)
		if okH && okM && h < 24 && m < 60 {
			return sign * (h*3600 + m*60), 5, true
		}
	}
	if len(b) >= 3 {
		h, okH := parseDigits(b[1:3], 2)
		if okH && h < 24 {
			return sign * (h * 3600), 3, true
		}
	}
	return 0, 0, false
}

func parseMonth(b []byte) (int, bool) {
	if len(b) < 3 {
		return 0, false
	}
	c0 := b[0] | 0x20
	c1 := b[1] | 0x20
	c2 := b[2] | 0x20
	switch c0 {
	case 'j':
		if c1 == 'a' && c2 == 'n' {
			return 1, true
		}
		if c1 == 'u' && c2 == 'n' {
			return 6, true
		}
		if c1 == 'u' && c2 == 'l' {
			return 7, true
		}
	case 'f':
		if c1 == 'e' && c2 == 'b' {
			return 2, true
		}
	case 'm':
		if c1 == 'a' && c2 == 'r' {
			return 3, true
		}
		if c1 == 'a' && c2 == 'y' {
			return 5, true
		}
	case 'a':
		if c1 == 'p' && c2 == 'r' {
			return 4, true
		}
		if c1 == 'u' && c2 == 'g' {
			return 8, true
		}
	case 's':
		if c1 == 'e' && c2 == 'p' {
			return 9, true
		}
	case 'o':
		if c1 == 'c' && c2 == 't' {
			return 10, true
		}
	case 'n':
		if c1 == 'o' && c2 == 'v' {
			return 11, true
		}
	case 'd':
		if c1 == 'e' && c2 == 'c' {
			return 12, true
		}
	}
	return 0, false
}

func isAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func hasPrefix(s, prefix []byte) bool {
	return len(s) >= len(prefix) && equalBytes(s[:len(prefix)], prefix)
}

func equalBytes(a, b []byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trimLeadingWhitespace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return b[i:]
}

func isValidDate(year, month, day int) bool {
	if year < 1970 || year > 2200 || month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	if month == 4 || month == 6 || month == 9 || month == 11 {
		return day <= 30
	}
	if month == 2 {
		isLeap := (year%4 == 0 && year%100 != 0) || (year%400 == 0)
		if isLeap {
			return day <= 29
		}
		return day <= 28
	}
	return true
}

func isValidTime(hour, minute, second int) bool {
	return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59 && second >= 0 && second <= 60
}

// daysSinceEpoch returns days between 1970-01-01 and the given civil date (Euclidean affine algorithm).
func daysSinceEpoch(year, month, day int) int64 {
	y := int64(year)
	m := int64(month)
	d := int64(day)
	if m <= 2 {
		y--
		m += 12
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	doy := (153*(m-3)+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func utcToNanoseconds(year, month, day, hour, minute, second, nsec, offsetSec int) int64 {
	days := daysSinceEpoch(year, month, day)
	utcSec := days*86400 + int64(hour*3600+minute*60+second) - int64(offsetSec)
	return utcSec*1e9 + int64(nsec)
}

func localToUTC(year, month, day, hour, minute, second, nsec int, loc *time.Location) int64 {
	if loc == nil || loc == time.UTC {
		days := daysSinceEpoch(year, month, day)
		utcSec := days*86400 + int64(hour*3600+minute*60+second)
		return utcSec*1e9 + int64(nsec)
	}
	days := daysSinceEpoch(year, month, day)
	approxSec := days*86400 + int64(hour*3600+minute*60+second)
	t := time.Unix(approxSec, int64(nsec)).In(loc)
	_, offsetSec := t.Zone()
	t2 := time.Unix(approxSec-int64(offsetSec), int64(nsec)).In(loc)
	_, offsetSec2 := t2.Zone()
	utcSec := approxSec - int64(offsetSec2)
	return utcSec*1e9 + int64(nsec)
}
