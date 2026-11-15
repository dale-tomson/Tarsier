package logs

import (
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestDetectLevel_AllTiers(t *testing.T) {
	utc := time.UTC
	refTS := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	tests := []struct {
		name        string
		line        string
		prevLevel   model.Level
		prevTSDelta time.Duration
		expectedLvl model.Level
		expectedSrc model.LevelSource
	}{
		// Tier 1 — Structured JSON
		{
			name:        "Tier1_JSON_Error",
			line:        `{"level": "error", "msg": "database down"}`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_Warn_Upper",
			line:        `{"lvl": "WARN", "msg": "high latency"}`,
			expectedLvl: model.LevelWarn,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_Pino_Fatal_60",
			line:        `{"level": 60, "msg": "process killed"}`,
			expectedLvl: model.LevelFatal,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_Pino_Error_50",
			line:        `{"level": 50, "msg": "connection failed"}`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_Pino_Warn_40",
			line:        `{"level": 40, "msg": "disk nearly full"}`,
			expectedLvl: model.LevelWarn,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_Pino_Info_30",
			line:        `{"level": 30, "msg": "server listening"}`,
			expectedLvl: model.LevelInfo,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_JSON_SingleLetter_F",
			line:        `{"severity": "F", "msg": "fatal error"}`,
			expectedLvl: model.LevelFatal,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_Logfmt_Warn",
			line:        `ts=2026-10-16T12:00:00Z lvl=warn msg="cache eviction"`,
			expectedLvl: model.LevelWarn,
			expectedSrc: model.LevelSourceStructured,
		},
		{
			name:        "Tier1_Logfmt_Quoted_Fatal",
			line:        `severity="FATAL" component=storage err=corrupted`,
			expectedLvl: model.LevelFatal,
			expectedSrc: model.LevelSourceStructured,
		},

		// Tier 2 — Delimited Tokens Near Line Start
		{
			name:        "Tier2_Bracket_Error",
			line:        `2026-10-16T12:00:00Z [ERROR] failed to connect to redis`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_Parens_Warn",
			line:        `2026-10-16T12:00:00Z (WARN) memory usage at 85%`,
			expectedLvl: model.LevelWarn,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_Pipe_Info",
			line:        `2026-10-16 12:00:00 | INFO | starting http listener`,
			expectedLvl: model.LevelInfo,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_SingleLetter_Bracket_E",
			line:        `[E] disk quota reached`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_GLog_Prefix_E",
			line:        `E1016 03:47:12.123456 1234 server.go:42] connection reset by peer`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_GLog_Prefix_I",
			line:        `I1016 03:47:12.000001 987 main.go:10] bootstrap complete`,
			expectedLvl: model.LevelInfo,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_Syslog_PRI_35_Error",
			line:        `<35>1 2026-10-16T12:00:00Z host app - - error encountered`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_Syslog_PRI_34_Fatal",
			line:        `<34>1 2026-10-16T12:00:00Z host app - - emergency crash`,
			expectedLvl: model.LevelFatal,
			expectedSrc: model.LevelSourceDelimited,
		},
		{
			name:        "Tier2_Colon_Delimited",
			line:        `ERROR: could not bind port 8080`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceDelimited,
		},

		// Tier 3 — Keyword Anywhere in Line
		{
			name:        "Tier3_Keyword_Panic",
			line:        `worker thread encountered unhandled panic in queue processor`,
			expectedLvl: model.LevelFatal,
			expectedSrc: model.LevelSourceKeyword,
		},
		{
			name:        "Tier3_Keyword_Error",
			line:        `job 42 completed with fatal database corruption and minor warning`,
			expectedLvl: model.LevelFatal, // Fatal > Error > Warn
			expectedSrc: model.LevelSourceKeyword,
		},
		{
			name:        "Tier3_Keyword_Debug",
			line:        `subsystem verbose trace debug logging enabled`,
			expectedLvl: model.LevelDebug,
			expectedSrc: model.LevelSourceKeyword,
		},

		// Tier 4 — Heuristic Hints
		{
			name:        "Tier4_HTTP_502_Error",
			line:        `192.168.1.1 - - [16/Oct/2026:12:00:00 +0000] "GET /api/users HTTP/1.1" 502 128`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceHint,
		},
		{
			name:        "Tier4_HTTP_401_Warn",
			line:        `10.0.0.1 - - [16/Oct/2026:12:00:00 +0000] "POST /login HTTP/1.1" 401 42`,
			expectedLvl: model.LevelWarn,
			expectedSrc: model.LevelSourceHint,
		},
		{
			name:        "Tier4_HTTP_200_Info",
			line:        `127.0.0.1 - - [16/Oct/2026:12:00:00 +0000] "GET /health HTTP/1.1" 200 15`,
			expectedLvl: model.LevelInfo,
			expectedSrc: model.LevelSourceHint,
		},
		{
			name:        "Tier4_Exception_Keyword",
			line:        `caught unhandled java.lang.NullPointerException while handling request`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceHint,
		},
		{
			name:        "Tier4_Timeout_Keyword",
			line:        `backend upstream connection timed out after 3000ms`,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceHint,
		},

		// Tier 5 — Stack Trace Continuation Inheritance
		{
			name:        "Tier5_Inherited_StackTrace_Within5s",
			line:        "\tat com.acme.App.process(App.java:123)",
			prevLevel:   model.LevelError,
			prevTSDelta: 2 * time.Second,
			expectedLvl: model.LevelError,
			expectedSrc: model.LevelSourceInherited,
		},
		{
			name:        "Tier5_Expired_After5s_FallbackUnknown",
			line:        "\tat com.acme.App.process(App.java:123)",
			prevLevel:   model.LevelError,
			prevTSDelta: 6 * time.Second,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detector := NewSourceDetector(utc, 4096)
			if tt.prevLevel != model.LevelUnknown {
				detector.lastLevel = tt.prevLevel
				detector.lastLevelTS = refTS - int64(tt.prevTSDelta)
			}
			res := detector.DetectLevel([]byte(tt.line), refTS, false)
			if res.Level != tt.expectedLvl {
				t.Errorf("expected Level=%v, got=%v (line: %q)", tt.expectedLvl, res.Level, tt.line)
			}
			if res.LevelSrc != tt.expectedSrc {
				t.Errorf("expected LevelSrc=%v, got=%v (line: %q)", tt.expectedSrc, res.LevelSrc, tt.line)
			}
		})
	}
}

func TestDetectLevel_FalsePositiveTraps(t *testing.T) {
	utc := time.UTC
	refTS := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	traps := []struct {
		name        string
		line        string
		expectedLvl model.Level
		expectedSrc model.LevelSource
	}{
		{
			name:        "Trap_HTTP_200_Path_Error",
			line:        `"GET /api/error HTTP/1.1" 200 OK`,
			expectedLvl: model.LevelInfo,
			expectedSrc: model.LevelSourceHint, // From HTTP 200, NOT Error from /api/error
		},
		{
			name:        "Trap_Rotated_Error_Log",
			line:        `rotated /var/log/nginx/error.log successfully`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_Zero_Errors_Zero_Failed",
			line:        `completed batch sync: 0 errors, 0 failed`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_Error_Nil",
			line:        `request finished duration=12ms error=nil`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_JSON_Error_Null",
			line:        `{"msg":"healthcheck","errors":0,"error":null}`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_Metric_Error_Count_Zero",
			line:        `metric error_count=0 exported`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_ErrorHandler_Identifier",
			line:        `ErrorHandler initialized for route /api/v1`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_No_Errors_Found",
			line:        `no errors found during verification run`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_Error_Free_Execution",
			line:        `pipeline finished error-free in 234ms`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
		{
			name:        "Trap_Err_Equals_0",
			line:        `task completed with err=0`,
			expectedLvl: model.LevelUnknown,
			expectedSrc: model.LevelSourceNone,
		},
	}

	for _, tt := range traps {
		t.Run(tt.name, func(t *testing.T) {
			detector := NewSourceDetector(utc, 4096)
			res := detector.DetectLevel([]byte(tt.line), refTS, false)
			if res.Level != tt.expectedLvl {
				t.Errorf("%s: expected Level=%v, got=%v (line: %q)", tt.name, tt.expectedLvl, res.Level, tt.line)
			}
			if res.LevelSrc != tt.expectedSrc {
				t.Errorf("%s: expected LevelSrc=%v, got=%v (line: %q)", tt.name, tt.expectedSrc, res.LevelSrc, tt.line)
			}
		})
	}
}

func TestDetectLevel_ConfigToggles(t *testing.T) {
	utc := time.UTC
	refTS := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	keywordLine := []byte("worker processor encountered severe database latency")
	hintLine := []byte(`"GET /api/users HTTP/1.1" 500 128`)

	// 1. KeywordLevels disabled: should skip Tier 3 keyword match
	detectorNoKW := NewSourceDetector(utc, 4096)
	detectorNoKW.SetLevelConfig(LevelConfig{
		MaxScanBytes:  4096,
		KeywordLevels: false,
		Hints:         true,
	})
	resNoKW := detectorNoKW.DetectLevel(keywordLine, refTS, false)
	if resNoKW.Level != model.LevelUnknown {
		t.Errorf("expected LevelUnknown with KeywordLevels=false, got %v", resNoKW.Level)
	}

	// 2. KeywordLevels enabled: should match Error
	detectorKW := NewSourceDetector(utc, 4096)
	resKW := detectorKW.DetectLevel(keywordLine, refTS, false)
	if resKW.Level != model.LevelError || resKW.LevelSrc != model.LevelSourceKeyword {
		t.Errorf("expected LevelError/Keyword, got Level=%v LevelSrc=%v", resKW.Level, resKW.LevelSrc)
	}

	// 3. Hints disabled: should skip Tier 4 HTTP 500 hint
	detectorNoHints := NewSourceDetector(utc, 4096)
	detectorNoHints.SetLevelConfig(LevelConfig{
		MaxScanBytes:  4096,
		KeywordLevels: true,
		Hints:         false,
	})
	resNoHints := detectorNoHints.DetectLevel(hintLine, refTS, false)
	if resNoHints.Level != model.LevelUnknown {
		t.Errorf("expected LevelUnknown with Hints=false, got %v", resNoHints.Level)
	}

	// 4. Hints enabled: should match Error
	detectorHints := NewSourceDetector(utc, 4096)
	resHints := detectorHints.DetectLevel(hintLine, refTS, false)
	if resHints.Level != model.LevelError || resHints.LevelSrc != model.LevelSourceHint {
		t.Errorf("expected LevelError/Hint, got Level=%v LevelSrc=%v", resHints.Level, resHints.LevelSrc)
	}
}

func BenchmarkDetectLevel_ZeroAlloc(b *testing.B) {
	utc := time.UTC
	refTS := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	detector := NewSourceDetector(utc, 4096)

	jsonLine := []byte(`{"time":"2026-10-16T12:00:00Z","level":"error","service":"auth","msg":"database connection failed"}`)
	delimitedLine := []byte("2026-10-16 12:00:00.123456 [WARN] disk space low: /data partition is at 88% capacity")
	plainLine := []byte("worker node-01 encountered unhandled panic in background queue dispatch")

	b.Run("Allocations_JSON_0Allocs", func(b *testing.B) {
		allocs := testing.AllocsPerRun(1000, func() {
			res := detector.DetectLevel(jsonLine, refTS, false)
			if res.Level != model.LevelError {
				b.Fatalf("unexpected level: %v", res.Level)
			}
		})
		if allocs > 0 {
			b.Fatalf("expected 0 allocs/op, got %f", allocs)
		}
	})

	b.Run("Throughput_JSON", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = detector.DetectLevel(jsonLine, refTS, false)
		}
	})

	b.Run("Allocations_Delimited_0Allocs", func(b *testing.B) {
		allocs := testing.AllocsPerRun(1000, func() {
			res := detector.DetectLevel(delimitedLine, refTS, false)
			if res.Level != model.LevelWarn {
				b.Fatalf("unexpected level: %v", res.Level)
			}
		})
		if allocs > 0 {
			b.Fatalf("expected 0 allocs/op, got %f", allocs)
		}
	})

	b.Run("Throughput_Delimited", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = detector.DetectLevel(delimitedLine, refTS, false)
		}
	})

	b.Run("Allocations_PlainKeyword_0Allocs", func(b *testing.B) {
		allocs := testing.AllocsPerRun(1000, func() {
			res := detector.DetectLevel(plainLine, refTS, false)
			if res.Level != model.LevelFatal {
				b.Fatalf("unexpected level: %v", res.Level)
			}
		})
		if allocs > 0 {
			b.Fatalf("expected 0 allocs/op, got %f", allocs)
		}
	})

	b.Run("Throughput_Combined_Timestamp_And_Level", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tsRes := detector.DetectTimestamp(DetectInput{
				Line:       delimitedLine,
				ReadTimeNS: refTS,
			})
			_ = detector.DetectLevel(delimitedLine, tsRes.TS, false)
		}
	})
}
