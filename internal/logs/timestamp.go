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

