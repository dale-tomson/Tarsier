package logs

import (
	"fmt"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestDetectTimestamp_GoldenCorpus(t *testing.T) {
	utc := time.UTC
	refReadTime := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	type testCase struct {
		name       string
		line       string
		readTime   int64
		dockerTS   int64
		isDocker   bool
		tz         *time.Location
		ambiguity  int8
		expectedTS int64
		expectedSrc model.TimestampSource
	}

	// Helper to make UTC nanoseconds from string RFC3339 for verification
	mustRFC3339 := func(s string) int64 {
		parsed, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			panic(fmt.Sprintf("invalid rfc3339: %s: %v", s, err))
		}
		return parsed.UnixNano()
	}

	var cases []testCase

	// 1. ISO 8601 & RFC 3339 Variants (Cases 1 - 25)
	isoCases := []struct {
		line string
		rfc  string
	}{
		{"2026-10-16T03:47:12Z msg", "2026-10-16T03:47:12Z"},
		{"2026-10-16T03:47:12.123Z test log", "2026-10-16T03:47:12.123Z"},
		{"2026-10-16T03:47:12.123456Z log line", "2026-10-16T03:47:12.123456Z"},
		{"2026-10-16T03:47:12.123456789Z full nano", "2026-10-16T03:47:12.123456789Z"},
		{"2026-10-16 03:47:12Z space separator", "2026-10-16T03:47:12Z"},
		{"2026-10-16 03:47:12.500Z space + frac", "2026-10-16T03:47:12.5Z"},
		{"2026-10-16 03:47:12,123Z comma decimal", "2026-10-16T03:47:12.123Z"},
		{"2026-10-16T03:47:12+05:30 indian offset", "2026-10-16T03:47:12+05:30"},
		{"2026-10-16T03:47:12.500+05:30 offset+frac", "2026-10-16T03:47:12.5+05:30"},
		{"2026-10-16T03:47:12-04:00 us eastern", "2026-10-16T03:47:12-04:00"},
		{"2026-10-16T03:47:12+0530 no colon tz", "2026-10-16T03:47:12+05:30"},
		{"2026-10-16T03:47:12-0700 no colon tz", "2026-10-16T03:47:12-07:00"},
		{"2026-10-16T03:47:12+02 2-digit hour tz", "2026-10-16T03:47:12+02:00"},
		{"2026-10-16T03:47:12-05 2-digit hour tz", "2026-10-16T03:47:12-05:00"},
		{"[2026-10-16T03:47:12Z] bracketed", "2026-10-16T03:47:12Z"},
		{"(2026-10-16T03:47:12Z) parenthesized", "2026-10-16T03:47:12Z"},
		{"20261016T034712Z compact iso", "2026-10-16T03:47:12Z"},
		{"2026-01-01T00:00:00Z new year", "2026-01-01T00:00:00Z"},
		{"2026-12-31T23:59:59Z year end", "2026-12-31T23:59:59Z"},
		{"2026-02-28T12:00:00Z feb normal", "2026-02-28T12:00:00Z"},
		{"2024-02-29T12:00:00Z feb leap", "2024-02-29T12:00:00Z"},
		{"2026-10-16 03:47:12 no zone (utc default)", "2026-10-16T03:47:12Z"},
		{"2026-10-16 03:47:12.25 no zone frac", "2026-10-16T03:47:12.25Z"},
		{"host-01 app: 2026-10-16T03:47:12Z anchored prefix", "2026-10-16T03:47:12Z"},
		{"ip-10-0-1-5 web: 2026-10-16T03:47:12Z anchored prefix 2", "2026-10-16T03:47:12Z"},
	}
	for i, c := range isoCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("ISO8601_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 2. Slash / Go / Python Style (Cases 26 - 40)
	slashCases := []struct {
		line string
		rfc  string
	}{
		{"2026/10/16 03:47:12 web request", "2026-10-16T03:47:12Z"},
		{"2026/10/16 03:47:12.123 with millis", "2026-10-16T03:47:12.123Z"},
		{"2026/10/16 03:47:12.123456 with micros", "2026-10-16T03:47:12.123456Z"},
		{"2026/10/16T03:47:12 slash with T", "2026-10-16T03:47:12Z"},
		{"2026/01/15 08:30:00 jan slash", "2026-01-15T08:30:00Z"},
		{"2026/12/25 18:00:00 xmas slash", "2026-12-25T18:00:00Z"},
		{"2026-10-16 03:47:12.123456 +0530 IST m=+0.01", "2026-10-16T03:47:12.123456+05:30"},
		{"2026-10-16 03:47:12.000000 +0000 UTC m=+12.34", "2026-10-16T03:47:12Z"},
		{"2026-10-16 03:47:12.500 -0400 EDT m=+0.50", "2026-10-16T03:47:12.5-04:00"},
		{"2026-10-16 03:47:12 +0000 UTC", "2026-10-16T03:47:12Z"},
		{"2026/10/16 03:47:12 +0530", "2026-10-16T03:47:12+05:30"},
		{"2026/10/16 03:47:12 -0800", "2026-10-16T03:47:12-08:00"},
		{"[2026/10/16 03:47:12] bracketed slash", "2026-10-16T03:47:12Z"},
		{"(2026/10/16 03:47:12) parens slash", "2026-10-16T03:47:12Z"},
		{"2026/05/20 14:15:30 spring slash", "2026-05-20T14:15:30Z"},
	}
	for i, c := range slashCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("SlashGo_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 3. Syslog RFC 5424 (Cases 41 - 50)
	syslog5424Cases := []struct {
		line string
		rfc  string
	}{
		{"<34>1 2026-10-16T03:47:12.003Z mymachine.example.com su - ID47", "2026-10-16T03:47:12.003Z"},
		{"<165>1 2026-10-16T03:47:12Z host app 1234 - msg", "2026-10-16T03:47:12Z"},
		{"<0>1 2026-10-16T03:47:12.123456Z kern kern 1 - emergency", "2026-10-16T03:47:12.123456Z"},
		{"<13>1 2026-10-16T03:47:12+05:30 host daemon 99 - user notice", "2026-10-16T03:47:12+05:30"},
		{"<191>1 2026-10-16T03:47:12-04:00 host local7 42 - debug msg", "2026-10-16T03:47:12-04:00"},
		{"<85>1 2026-10-16T03:47:12Z mail server 1 - mail sent", "2026-10-16T03:47:12Z"},
		{"<22>1 2026-10-16T03:47:12.999Z auth server - - root login", "2026-10-16T03:47:12.999Z"},
		{"<1>1 2026-10-16T03:47:12Z alert sys - - alert", "2026-10-16T03:47:12Z"},
		{"<30>1 2026-10-16T03:47:12Z daemon proc - - restart", "2026-10-16T03:47:12Z"},
		{"<180>1 2026-10-16T03:47:12Z audit app - - sec event", "2026-10-16T03:47:12Z"},
	}
	for i, c := range syslog5424Cases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("Syslog5424_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 4. Syslog RFC 3164 (Cases 51 - 65)
	syslog3164Cases := []struct {
		line string
		rfc  string
	}{
		{"Oct 16 03:47:12 mymachine su: 'su root' succeeded", "2026-10-16T03:47:12Z"},
		{"Oct  6 03:47:12 single digit day with 2 spaces", "2026-10-06T03:47:12Z"},
		{"<34>Oct 16 03:47:12 with pri prefix", "2026-10-16T03:47:12Z"},
		{"<13>Oct  6 03:47:12 with pri and 2 spaces", "2026-10-06T03:47:12Z"},
		{"Jan  1 00:00:00 jan start", "2026-01-01T00:00:00Z"},
		{"Feb 28 23:59:59 feb end", "2026-02-28T23:59:59Z"},
		{"Mar 15 12:30:45 march mid", "2026-03-15T12:30:45Z"},
		{"Apr 30 18:20:10 apr end", "2026-04-30T18:20:10Z"},
		{"May  5 06:05:04 may day", "2026-05-05T06:05:04Z"},
		{"Jun 21 09:12:15 solstice", "2026-06-21T09:12:15Z"},
		{"Jul  4 16:45:00 fourth of july", "2026-07-04T16:45:00Z"},
		{"Aug 12 11:11:11 aug mid", "2026-08-12T11:11:11Z"},
		{"Sep 30 20:00:00 sep end", "2026-09-30T20:00:00Z"},
		{"Nov 11 11:11:11 veterans day", "2026-11-11T11:11:11Z"},
		{"Dec 25 08:00:00 christmas", "2026-12-25T08:00:00Z"},
	}
	for i, c := range syslog3164Cases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("Syslog3164_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 5. Common Log Format (CLF) (Cases 66 - 75)
	clfCases := []struct {
		line string
		rfc  string
	}{
		{"[16/Oct/2026:03:47:12 +0530] \"GET /index.html HTTP/1.1\" 200 1234", "2026-10-16T03:47:12+05:30"},
		{"16/Oct/2026:03:47:12 +0530 unbracketed clf", "2026-10-16T03:47:12+05:30"},
		{"[16/Oct/2026:03:47:12 +0000] utc clf", "2026-10-16T03:47:12Z"},
		{"[16/Oct/2026:03:47:12 -0400] edt clf", "2026-10-16T03:47:12-04:00"},
		{"[01/Jan/2026:00:00:00 +0000] new year clf", "2026-01-01T00:00:00Z"},
		{"[28/Feb/2026:12:00:00 +0100] feb clf", "2026-02-28T12:00:00+01:00"},
		{"[31/Dec/2026:23:59:59 +0000] year end clf", "2026-12-31T23:59:59Z"},
		{"[04/Jul/2026:18:30:00 -0700] summer clf", "2026-07-04T18:30:00-07:00"},
		{"127.0.0.1 - frank [16/Oct/2026:03:47:12 +0530] \"POST /api\" 201", "2026-10-16T03:47:12+05:30"},
		{"::1 - - [16/Oct/2026:03:47:12 +0000] \"HEAD /health\" 200", "2026-10-16T03:47:12Z"},
	}
	for i, c := range clfCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("CLF_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 6. HTTP-date / RFC 2822 / RFC 1123 (Cases 76 - 85)
	rfc2822Cases := []struct {
		line string
		rfc  string
	}{
		{"Fri, 16 Oct 2026 03:47:12 +0530", "2026-10-16T03:47:12+05:30"},
		{"16 Oct 2026 03:47:12 GMT", "2026-10-16T03:47:12Z"},
		{"Fri, 16 Oct 2026 03:47:12 GMT", "2026-10-16T03:47:12Z"},
		{"Fri, 16 Oct 2026 03:47:12 UTC", "2026-10-16T03:47:12Z"},
		{"Mon, 01 Jan 2026 00:00:00 +0000", "2026-01-01T00:00:00Z"},
		{"Thu, 15 Jan 2026 10:20:30 -0500", "2026-01-15T10:20:30-05:00"},
		{"16 Oct 2026 03:47:12 +0000 without weekday", "2026-10-16T03:47:12Z"},
		{"6 Oct 2026 03:47:12 GMT single digit day", "2026-10-06T03:47:12Z"},
		{"Wed, 25 Dec 2026 12:00:00 +0200", "2026-12-25T12:00:00+02:00"},
		{"Date: Fri, 16 Oct 2026 03:47:12 GMT", "2026-10-16T03:47:12Z"},
	}
	for i, c := range rfc2822Cases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("RFC2822_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 7. ctime / asctime (Cases 86 - 95)
	ctimeCases := []struct {
		line string
		rfc  string
	}{
		{"Fri Oct 16 03:47:12 2026", "2026-10-16T03:47:12Z"},
		{"Fri Oct  6 03:47:12 2026", "2026-10-06T03:47:12Z"},
		{"Thu Jan  1 00:00:00 2026", "2026-01-01T00:00:00Z"},
		{"Sat Feb 28 23:59:59 2026", "2026-02-28T23:59:59Z"},
		{"Sun Mar 15 12:00:00 2026", "2026-03-15T12:00:00Z"},
		{"Wed Apr 30 18:30:00 2026", "2026-04-30T18:30:00Z"},
		{"Fri May  1 08:00:00 2026", "2026-05-01T08:00:00Z"},
		{"Mon Jun 15 14:20:10 2026", "2026-06-15T14:20:10Z"},
		{"Sat Jul  4 10:00:00 2026", "2026-07-04T10:00:00Z"},
		{"Thu Dec 31 23:59:59 2026", "2026-12-31T23:59:59Z"},
	}
	for i, c := range ctimeCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("CTime_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 8. Month-name Dates (Cases 96 - 105)
	monthNameCases := []struct {
		line string
		rfc  string
	}{
		{"16 Oct 2026 03:47:12", "2026-10-16T03:47:12Z"},
		{"6 Oct 2026 03:47:12 single digit day", "2026-10-06T03:47:12Z"},
		{"Oct 16, 2026 3:47:12 AM", "2026-10-16T03:47:12Z"},
		{"Oct 16, 2026 3:47:12 PM", "2026-10-16T15:47:12Z"},
		{"Oct 16, 2026 12:00:00 AM midnight", "2026-10-16T00:00:00Z"},
		{"Oct 16, 2026 12:00:00 PM noon", "2026-10-16T12:00:00Z"},
		{"Jan 01, 2026 08:15:30 AM", "2026-01-01T08:15:30Z"},
		{"Feb 28, 2026 11:30:00 PM", "2026-02-28T23:30:00Z"},
		{"15 Nov 2026 18:22:44", "2026-11-15T18:22:44Z"},
		{"31 Dec 2026 23:59:59", "2026-12-31T23:59:59Z"},
	}
	for i, c := range monthNameCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("MonthName_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 9. Numeric Day/Month First (Cases 106 - 115)
	numDateCases := []struct {
		line      string
		rfc       string
		ambiguity int8
	}{
		{"16/10/2026 03:47:12 unambiguous day first", "2026-10-16T03:47:12Z", 0},
		{"10/16/2026 03:47:12 unambiguous month first", "2026-10-16T03:47:12Z", 0},
		{"25/12/2026 18:00:00 xmas day first", "2026-12-25T18:00:00Z", 0},
		{"12/25/2026 18:00:00 xmas month first", "2026-12-25T18:00:00Z", 0},
		{"16-10-2026 03:47:12 dash day first", "2026-10-16T03:47:12Z", 0},
		{"10-16-2026 03:47:12 dash month first", "2026-10-16T03:47:12Z", 0},
		{"04/05/2026 12:00:00 settled day first", "2026-05-04T12:00:00Z", 1},
		{"04/05/2026 12:00:00 settled month first", "2026-04-05T12:00:00Z", 2},
		{"01/02/2026 08:00:00 settled day first", "2026-02-01T08:00:00Z", 1},
		{"01/02/2026 08:00:00 settled month first", "2026-01-02T08:00:00Z", 2},
	}
	for i, c := range numDateCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("NumericDate_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			ambiguity:   c.ambiguity,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 10. glog / klog (Cases 116 - 125)
	glogCases := []struct {
		line string
		rfc  string
	}{
		{"E1016 03:47:12.123456 1234 file.go:42] error message", "2026-10-16T03:47:12.123456Z"},
		{"I1016 03:47:12.000001 987 main.go:10] info message", "2026-10-16T03:47:12.000001Z"},
		{"W1016 03:47:12.500000 555 cache.go:88] warn message", "2026-10-16T03:47:12.5Z"},
		{"F1016 03:47:12.999999 1 init.go:5] fatal message", "2026-10-16T03:47:12.999999Z"},
		{"I0101 00:00:00.100000 1 server.go:1] new year", "2026-01-01T00:00:00.1Z"},
		{"I0228 23:59:59.000000 1 server.go:1] feb end", "2026-02-28T23:59:59Z"},
		{"W0315 12:30:00.123000 2 db.go:15] query slow", "2026-03-15T12:30:00.123Z"},
		{"E0704 18:00:00.000000 3 auth.go:99] denied", "2026-07-04T18:00:00Z"},
		{"I1115 08:00:00.250000 4 worker.go:5] start", "2026-11-15T08:00:00.25Z"},
		{"E1231 23:59:59.999000 5 cron.go:10] year rollover", "2026-12-31T23:59:59.999Z"},
	}
	for i, c := range glogCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("GLog_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 11. Epoch Timestamps (Cases 126 - 135)
	epochCases := []struct {
		line string
		ts   int64
	}{
		{"1760579232 10-digit epoch s", 1760579232 * 1e9},
		{"1760579232123 13-digit epoch ms", 1760579232123 * 1e6},
		{"1760579232123456 16-digit epoch us", 1760579232123456 * 1e3},
		{"1760579232123456789 19-digit epoch ns", 1760579232123456789},
		{"1760579232.123 fractional seconds", 1760579232*1e9 + 123000000},
		{"[1760579232] bracketed epoch s", 1760579232 * 1e9},
		{"[1760579232123] bracketed epoch ms", 1760579232123 * 1e6},
		{"(1760579232) parens epoch s", 1760579232 * 1e9},
		{"1760579232.500 fractional epoch", 1760579232*1e9 + 500000000},
		{"1760579232.000001 microsecond float", 1760579232*1e9 + 1000},
	}
	for i, c := range epochCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("Epoch_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  c.ts,
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 12. Time-Only (Cases 136 - 145)
	timeOnlyCases := []struct {
		line string
		rfc  string
	}{
		{"03:47:12 app started", "2026-10-16T03:47:12Z"},
		{"03:47:12.123 with millis", "2026-10-16T03:47:12.123Z"},
		{"03:47:12.123456 with micros", "2026-10-16T03:47:12.123456Z"},
		{"[03:47:12] bracketed time", "2026-10-16T03:47:12Z"},
		{"(03:47:12) parens time", "2026-10-16T03:47:12Z"},
		{"[03:47:12.500] bracketed with frac", "2026-10-16T03:47:12.5Z"},
		{"00:00:00 midnight", "2026-10-16T00:00:00Z"},
		{"12:00:00 noon", "2026-10-16T12:00:00Z"},
		{"08:30:15 morning", "2026-10-16T08:30:15Z"},
		{"11:59:59 before noon", "2026-10-16T11:59:59Z"},
	}
	for i, c := range timeOnlyCases {
		cases = append(cases, testCase{
			name:        fmt.Sprintf("TimeOnly_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  mustRFC3339(c.rfc),
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 13. JSON Top-Level Keys (Cases 146 - 165)
	jsonCases := []struct {
		line string
		rfc  string
		ts   int64
	}{
		{`{"time": "2026-10-16T03:47:12Z", "msg": "hello"}`, "2026-10-16T03:47:12Z", 0},
		{`{"ts": "2026-10-16T03:47:12.123Z", "level": "info"}`, "2026-10-16T03:47:12.123Z", 0},
		{`{"timestamp": "2026-10-16T03:47:12Z", "status": 200}`, "2026-10-16T03:47:12Z", 0},
		{`{"@timestamp": "2026-10-16T03:47:12.500Z", "service": "api"}`, "2026-10-16T03:47:12.5Z", 0},
		{`{"t": "2026-10-16T03:47:12Z", "event": "click"}`, "2026-10-16T03:47:12Z", 0},
		{`{"datetime": "2026-10-16 03:47:12", "user": "admin"}`, "2026-10-16T03:47:12Z", 0},
		{`{"date": "2026-10-16T03:47:12+05:30", "host": "web"}`, "2026-10-16T03:47:12+05:30", 0},
		{`{"eventTime": "2026-10-16T03:47:12Z", "action": "login"}`, "2026-10-16T03:47:12Z", 0},
		{`{"ts": 1760579232, "msg": "epoch s in json"}`, "", 1760579232 * 1e9},
		{`{"time": 1760579232123, "msg": "epoch ms in json"}`, "", 1760579232123 * 1e6},
		{`{"timestamp": 1760579232.123, "msg": "epoch float in json"}`, "", 1760579232*1e9 + 123000000},
		{`{"ts": "1760579232", "msg": "quoted epoch s"}`, "", 1760579232 * 1e9},
		{`{"time": "1760579232123", "msg": "quoted epoch ms"}`, "", 1760579232123 * 1e6},
		{`{"msg": "prefix", "time": "2026-10-16T03:47:12Z"}`, "2026-10-16T03:47:12Z", 0},
		{`{"app": "worker", "ts": "2026-10-16 03:47:12,123Z"}`, "2026-10-16T03:47:12.123Z", 0},
		{`{ "time" : "2026-10-16T03:47:12Z" }`, "2026-10-16T03:47:12Z", 0},
		{`{"time":"2026-10-16T03:47:12Z"}`, "2026-10-16T03:47:12Z", 0},
		{`{"level":"info","time":"2026-10-16T03:47:12Z","msg":"ok"}`, "2026-10-16T03:47:12Z", 0},
		{`{"data":{"nested":true},"time":"2026-10-16T03:47:12Z"}`, "2026-10-16T03:47:12Z", 0},
		{`{"time":"2026-10-16T03:47:12+00:00"}`, "2026-10-16T03:47:12Z", 0},
	}
	for i, c := range jsonCases {
		expected := c.ts
		if c.rfc != "" {
			expected = mustRFC3339(c.rfc)
		}
		cases = append(cases, testCase{
			name:        fmt.Sprintf("JSON_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  expected,
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 14. Logfmt Keys (Cases 166 - 175)
	logfmtCases := []struct {
		line string
		rfc  string
		ts   int64
	}{
		{`level=info ts=2026-10-16T03:47:12Z msg=started`, "2026-10-16T03:47:12Z", 0},
		{`level=warn time="2026-10-16 03:47:12" msg="disk warning"`, "2026-10-16T03:47:12Z", 0},
		{`timestamp=2026-10-16T03:47:12.123Z caller=main.go:12`, "2026-10-16T03:47:12.123Z", 0},
		{`@timestamp=2026-10-16T03:47:12Z app=billing`, "2026-10-16T03:47:12Z", 0},
		{`ts=1760579232 unit=seconds`, "", 1760579232 * 1e9},
		{`time=1760579232123 unit=millis`, "", 1760579232123 * 1e6},
		{`ts="1760579232" quoted=epoch`, "", 1760579232 * 1e9},
		{`app=web ts=2026-10-16T03:47:12+05:30 method=GET`, "2026-10-16T03:47:12+05:30", 0},
		{`[module] ts=2026-10-16T03:47:12Z bracketed module`, "2026-10-16T03:47:12Z", 0},
		{`ts=2026-10-16T03:47:12.999Z duration=42ms`, "2026-10-16T03:47:12.999Z", 0},
	}
	for i, c := range logfmtCases {
		expected := c.ts
		if c.rfc != "" {
			expected = mustRFC3339(c.rfc)
		}
		cases = append(cases, testCase{
			name:        fmt.Sprintf("Logfmt_%02d", i+1),
			line:        c.line,
			readTime:    refReadTime,
			tz:          utc,
			expectedTS:  expected,
			expectedSrc: model.TimestampSourceLine,
		})
	}

	// 15. Fallbacks: Docker TS, Carried TS, Read Time (Cases 176 - 180)
	dockerTS := mustRFC3339("2026-10-16T04:00:00Z")
	cases = append(cases,
		testCase{
			name:        "Fallback_Docker",
			line:        "no timestamp here in docker container stdout",
			readTime:    refReadTime,
			dockerTS:    dockerTS,
			isDocker:    true,
			tz:          utc,
			expectedTS:  dockerTS,
			expectedSrc: model.TimestampSourceDocker,
		},
		testCase{
			name:        "Fallback_ReadTime",
			line:        "just some random plain text log without any time",
			readTime:    refReadTime,
			isDocker:    false,
			tz:          utc,
			expectedTS:  refReadTime,
			expectedSrc: model.TimestampSourceReadTime,
		},
	)

	// Ensure we exceed the 150+ requirement
	if len(cases) < 150 {
		t.Fatalf("expected at least 150 test cases, got %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSourceDetector(tc.tz, 4096)
			if tc.ambiguity != 0 {
				detector.dayFirstAmbiguity = tc.ambiguity
			}
			readTime := tc.readTime
			if tc.expectedTS > 0 && tc.expectedTS > readTime {
				readTime = tc.expectedTS + int64(1*time.Second)
			}
			res := detector.DetectTimestamp(DetectInput{
				Line:       []byte(tc.line),
				ReadTimeNS: readTime,
				DockerTSNS: tc.dockerTS,
				IsDocker:   tc.isDocker,
			})
			if res.TSSrc != tc.expectedSrc {
				t.Errorf("%s: expected TSSrc=%v, got=%v (line: %q)", tc.name, tc.expectedSrc, res.TSSrc, tc.line)
			}
			if res.TS != tc.expectedTS {
				t.Errorf("%s: expected TS=%d, got=%d (delta: %d ns)", tc.name, tc.expectedTS, res.TS, res.TS-tc.expectedTS)
			}
		})
	}
}

func TestDetectTimestamp_FalsePositiveTraps(t *testing.T) {
	utc := time.UTC
	refReadTime := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	t.Run("MidLine13DigitIDs_Ignored", func(t *testing.T) {
		detector := NewSourceDetector(utc, 4096)
		// 13-digit ID mid-line should NOT be classified as epoch
		line := []byte("2026-10-16 order_id=1760579232123 processed successfully")
		res := detector.DetectTimestamp(DetectInput{
			Line:       line,
			ReadTimeNS: refReadTime,
			IsDocker:   false,
		})
		// Should fall back to read time since "2026-10-16" has no time component
		if res.TSSrc != model.TimestampSourceReadTime {
			t.Errorf("expected TSSrc=ReadTime, got %v (TS=%d)", res.TSSrc, res.TS)
		}

		// When Docker timestamp is present, should pick Docker timestamp
		dockerTS := refReadTime - 1000
		resDocker := detector.DetectTimestamp(DetectInput{
			Line:       line,
			ReadTimeNS: refReadTime,
			DockerTSNS: dockerTS,
			IsDocker:   true,
		})
		if resDocker.TSSrc != model.TimestampSourceDocker || resDocker.TS != dockerTS {
			t.Errorf("expected TSSrc=DockerTS, got %v", resDocker.TSSrc)
		}
	})

	t.Run("ImplausibleDates_Rejected", func(t *testing.T) {
		detector := NewSourceDetector(utc, 4096)
		// 1970 date is before MinPlausibleTS (2000-01-01)
		linePast := []byte("1970-01-01T00:00:01Z old boot log")
		resPast := detector.DetectTimestamp(DetectInput{
			Line:       linePast,
			ReadTimeNS: refReadTime,
		})
		if resPast.TSSrc != model.TimestampSourceReadTime {
			t.Errorf("expected 1970 date to be rejected, got %v", resPast.TSSrc)
		}

		// Far future date (> 24h ahead of readTime)
		lineFuture := []byte("2099-05-01T00:00:00Z future log")
		resFuture := detector.DetectTimestamp(DetectInput{
			Line:       lineFuture,
			ReadTimeNS: refReadTime,
		})
		if resFuture.TSSrc != model.TimestampSourceReadTime {
			t.Errorf("expected 2099 date to be rejected, got %v", resFuture.TSSrc)
		}
	})

	t.Run("AmbiguousDateSettlement", func(t *testing.T) {
		detector := NewSourceDetector(utc, 4096)
		ambiguousLine := []byte("04/05/2026 12:00:00 test event")

		// 1. Initial unsettled: should reject ambiguous date and fall back to ReadTime
		res1 := detector.DetectTimestamp(DetectInput{
			Line:       ambiguousLine,
			ReadTimeNS: refReadTime,
		})
		if res1.TSSrc != model.TimestampSourceReadTime {
			t.Errorf("expected ambiguous date to be rejected when unsettled, got TSSrc=%v", res1.TSSrc)
		}

		// 2. Unambiguous line arrives establishing DD/MM format (part1 = 16 > 12)
		unambiguousLine := []byte("16/05/2026 12:00:00 establishing dd/mm")
		res2 := detector.DetectTimestamp(DetectInput{
			Line:       unambiguousLine,
			ReadTimeNS: refReadTime,
		})
		if res2.TSSrc != model.TimestampSourceLine {
			t.Fatalf("expected unambiguous line to parse, got TSSrc=%v", res2.TSSrc)
		}
		if detector.dayFirstAmbiguity != 1 {
			t.Fatalf("expected dayFirstAmbiguity=1 (DD/MM), got %d", detector.dayFirstAmbiguity)
		}

		// 3. Now the earlier ambiguous line "04/05/2026" should parse as 4th of May 2026
		res3 := detector.DetectTimestamp(DetectInput{
			Line:       ambiguousLine,
			ReadTimeNS: refReadTime,
		})
		if res3.TSSrc != model.TimestampSourceLine {
			t.Fatalf("expected previously ambiguous line to parse now, got TSSrc=%v", res3.TSSrc)
		}
		expectedTS := time.Date(2026, 5, 4, 12, 0, 0, 0, utc).UnixNano()
		if res3.TS != expectedTS {
			t.Errorf("expected 2026-05-04, got %v", time.Unix(0, res3.TS).UTC())
		}
	})

	t.Run("StackTraceContinuation_CarriedTS", func(t *testing.T) {
		detector := NewSourceDetector(utc, 4096)
		firstLine := []byte("2026-10-16T03:47:12Z java.lang.NullPointerException: object is null")
		firstReadTime := refReadTime
		res1 := detector.DetectTimestamp(DetectInput{
			Line:       firstLine,
			ReadTimeNS: firstReadTime,
			IsDocker:   false,
		})
		if res1.TSSrc != model.TimestampSourceLine {
			t.Fatalf("expected line 1 to be DetectedInLine, got %v", res1.TSSrc)
		}

		// Continuation arriving within 2 seconds
		stackLine := []byte("\tat com.acme.App.main(App.java:42)")
		res2 := detector.DetectTimestamp(DetectInput{
			Line:       stackLine,
			ReadTimeNS: firstReadTime + int64(2*time.Second),
			IsDocker:   false,
		})
		if res2.TSSrc != model.TimestampSourceCarried {
			t.Errorf("expected stack trace within 2s to inherit CarriedPrevLine, got %v", res2.TSSrc)
		}
		if res2.TS != res1.TS {
			t.Errorf("expected carried TS=%d, got=%d", res1.TS, res2.TS)
		}

		// Continuation arriving 10 seconds later (> 5s threshold)
		lateLine := []byte("\tat com.acme.App.run(App.java:100)")
		lateReadTime := firstReadTime + int64(10*time.Second)
		resLate := detector.DetectTimestamp(DetectInput{
			Line:       lateLine,
			ReadTimeNS: lateReadTime,
			IsDocker:   false,
		})
		if resLate.TSSrc != model.TimestampSourceReadTime {
			t.Errorf("expected late continuation to fall back to ReadTime, got %v", resLate.TSSrc)
		}
		if resLate.TS != lateReadTime {
			t.Errorf("expected read time %d, got %d", lateReadTime, resLate.TS)
		}
	})

	t.Run("StickyFormat_ResetsCleanlyAfterMisses", func(t *testing.T) {
		detector := NewSourceDetector(utc, 4096)
		isoLine := []byte("2026-10-16T03:47:12Z iso log")
		res1 := detector.DetectTimestamp(DetectInput{
			Line:       isoLine,
			ReadTimeNS: refReadTime,
		})
		if res1.TSSrc != model.TimestampSourceLine || detector.stickyFormat != fmtISO8601 {
			t.Fatalf("expected stickyFormat=fmtISO8601, got %v", detector.stickyFormat)
		}

		// Send 25 lines without timestamp to trigger 20-miss reset
		noTimeLine := []byte("just a line with no timestamp at all")
		for i := 0; i < 25; i++ {
			detector.DetectTimestamp(DetectInput{
				Line:       noTimeLine,
				ReadTimeNS: refReadTime + int64((i+10)*int(time.Second)),
			})
		}
		if detector.stickyFormat != fmtNone {
			t.Errorf("expected stickyFormat to be cleared to fmtNone after 20 misses, got %v", detector.stickyFormat)
		}

		// Switch format to JSON: should detect cleanly from full scanner list
		jsonLine := []byte(`{"ts": 1760579232, "msg": "now json format"}`)
		resJSON := detector.DetectTimestamp(DetectInput{
			Line:       jsonLine,
			ReadTimeNS: refReadTime + int64(60*time.Second),
		})
		if resJSON.TSSrc != model.TimestampSourceLine {
			t.Errorf("expected JSON timestamp to be detected cleanly after sticky reset, got %v", resJSON.TSSrc)
		}
	})
}

func BenchmarkDetectTimestamp_FastPath(t *testing.B) {
	utc := time.UTC
	refReadTime := time.Date(2026, 10, 16, 12, 0, 0, 0, utc).UnixNano()

	detector := NewSourceDetector(utc, 4096)
	isoLine := []byte("2026-10-16T03:47:12.123456Z [INFO] worker-01: processed transaction batch id=987123 latency=12ms memory_rss_mb=42")

	// Prime the sticky format cache
	detector.DetectTimestamp(DetectInput{
		Line:       isoLine,
		ReadTimeNS: refReadTime,
	})

	t.Run("Allocations_0B_0Allocs", func(t *testing.B) {
		allocs := testing.AllocsPerRun(1000, func() {
			res := detector.DetectTimestamp(DetectInput{
				Line:       isoLine,
				ReadTimeNS: refReadTime,
			})
			if res.TSSrc != model.TimestampSourceLine {
				t.Fatalf("unexpected TSSrc: %v", res.TSSrc)
			}
		})
		if allocs > 0 {
			t.Fatalf("expected 0 allocs/op on fast path, got %f", allocs)
		}
	})

	t.Run("Throughput_ISO8601", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = detector.DetectTimestamp(DetectInput{
				Line:       isoLine,
				ReadTimeNS: refReadTime,
			})
		}
	})

	jsonLine := []byte(`{"time":"2026-10-16T03:47:12.123456Z","level":"info","source":"worker-01","msg":"processed transaction batch","latency_ms":12}`)
	jsonDetector := NewSourceDetector(utc, 4096)
	jsonDetector.DetectTimestamp(DetectInput{
		Line:       jsonLine,
		ReadTimeNS: refReadTime,
	})

	t.Run("Allocations_JSON_0Allocs", func(t *testing.B) {
		allocs := testing.AllocsPerRun(1000, func() {
			res := jsonDetector.DetectTimestamp(DetectInput{
				Line:       jsonLine,
				ReadTimeNS: refReadTime,
			})
			if res.TSSrc != model.TimestampSourceLine {
				t.Fatalf("unexpected TSSrc: %v", res.TSSrc)
			}
		})
		if allocs > 0 {
			t.Fatalf("expected 0 allocs/op for JSON, got %f", allocs)
		}
	})

	t.Run("Throughput_JSON", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = jsonDetector.DetectTimestamp(DetectInput{
				Line:       jsonLine,
				ReadTimeNS: refReadTime,
			})
		}
	})
}
