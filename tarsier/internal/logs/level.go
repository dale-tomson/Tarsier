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

