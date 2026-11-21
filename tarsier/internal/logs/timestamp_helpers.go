package logs

import (
	"time"
)

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
