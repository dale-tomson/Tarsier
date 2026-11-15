package cleanup

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	// reTimestamp matches ISO-8601 / RFC-3339 timestamps embedded in log messages.
	reTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)

	// reUUID matches standard 8-4-4-4-12 hex UUID format.
	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// reIPv6 matches bracketed IPv6 with optional port or standard multi-colon IPv6 literals.
	reIPv6 = regexp.MustCompile(`\[[0-9a-fA-F:]+\](?::\d+)?|\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}\b`)

	// reIPv4 matches dotted-quad IPv4 addresses with optional port.
	reIPv4 = regexp.MustCompile(`\b(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(?::\d+)?\b`)

	// reDoubleQuote matches double-quoted literals respecting escaped quotes.
	reDoubleQuote = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

	// reSingleQuote matches single-quoted literals respecting escaped quotes.
	reSingleQuote = regexp.MustCompile(`'(?:\\.|[^'\\])*'`)

	// reHexLiteral matches 0x-prefixed hex literals.
	reHex0x = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)

	// reHexWord matches standalone tokens of 8-64 hex chars.
	reHexWord = regexp.MustCompile(`\b[0-9a-fA-F]{8,64}\b`)

	// rePath matches Unix or Windows file paths with 2+ segments.
	rePath = regexp.MustCompile(`(?:\b[a-zA-Z]:\\(?:[\w.-]+\\)+[\w.-]+|/(?:var|data|etc|usr|opt|home|tmp|root|proc|sys|dev|lib|bin|sbin|private|Volumes|Users)/(?:[\w.-]+/)*[\w.-]+|/(?:[\w.-]+/){2,}[\w.-]+)`)

	// reDurationOrPercent matches numbers followed by units or percentage.
	reDurationOrPercent = regexp.MustCompile(`\b\d+(?:\.\d+)?(ms|µs|us|ns|s|m|h|%|kb|mb|gb|b)\b|\b\d+(?:\.\d+)?%`)

	// reSignedNumber matches standalone integers and decimals, including negative numbers.
	reSignedNumber = regexp.MustCompile(`(?:\b|-\b?)\d+(?:\.\d+)?\b`)

	// reWhitespace matches consecutive spaces and tabs.
	reWhitespace = regexp.MustCompile(`[ \t\r\n]+`)
)

// NormalizeMessage collapses dynamic variables (timestamps, UUIDs, IPs, quotes, hex hashes,
// paths, and numbers) into canonical placeholders to group recurring log error patterns.
func NormalizeMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}

	// 1. Length Guard: cap input inspection at 1,024 bytes
	if len(raw) > 1024 {
		raw = raw[:1024] + "..."
	}

	msg := raw

	// 2. Embedded Timestamps ({ts}) - quick pre-check for '-' and ':'
	if strings.Contains(msg, "-") && strings.Contains(msg, ":") {
		msg = reTimestamp.ReplaceAllString(msg, "{ts}")
	}

	// 3. UUIDs ({uuid}) - quick pre-check for '-'
	if strings.Contains(msg, "-") {
		msg = reUUID.ReplaceAllString(msg, "{uuid}")
	}

	// 4. IPv6 & IPv4 addresses ({ip} or {ip}:{num})
	if strings.Contains(msg, ":") || strings.Contains(msg, "[") {
		msg = reIPv6.ReplaceAllStringFunc(msg, func(m string) string {
			if strings.HasPrefix(m, "[") && strings.Contains(m, "]:") {
				return "{ip}:{num}"
			}
			return "{ip}"
		})
	}

	if strings.Contains(msg, ".") {
		msg = reIPv4.ReplaceAllStringFunc(msg, func(m string) string {
			if strings.Contains(m, ":") {
				return "{ip}:{num}"
			}
			return "{ip}"
		})
	}

	// 5. Quoted Literals ("{str}" and '{str}')
	if strings.IndexByte(msg, '"') != -1 {
		msg = reDoubleQuote.ReplaceAllString(msg, `"{str}"`)
	}
	if strings.IndexByte(msg, '\'') != -1 {
		msg = reSingleQuote.ReplaceAllString(msg, `'{str}'`)
	}

	// 6. Hex Strings & Object IDs ({hex})
	if strings.Contains(msg, "0x") {
		msg = reHex0x.ReplaceAllString(msg, "{hex}")
	}
	msg = reHexWord.ReplaceAllStringFunc(msg, func(m string) string {
		// Standalone hex tokens >= 8 chars containing at least one a-f/A-F letter
		hasLetter := false
		for i := 0; i < len(m); i++ {
			c := m[i]
			if (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				hasLetter = true
				break
			}
		}
		if hasLetter {
			return "{hex}"
		}
		return m
	})

	// 7. Unix / Windows File Paths ({path})
	if strings.IndexByte(msg, '/') != -1 || strings.IndexByte(msg, '\\') != -1 {
		msg = rePath.ReplaceAllString(msg, "{path}")
	}

	// 8. Durations, Percentages & Numbers ({num}ms, {num}%, {num})
	msg = reDurationOrPercent.ReplaceAllStringFunc(msg, func(m string) string {
		if strings.HasSuffix(m, "%") {
			return "{num}%"
		}
		// Extract unit suffix
		for _, unit := range []string{"ms", "µs", "us", "ns", "kb", "mb", "gb", "b", "s", "m", "h"} {
			if strings.HasSuffix(strings.ToLower(m), unit) {
				return "{num}" + unit
			}
		}
		return "{num}"
	})

	msg = reSignedNumber.ReplaceAllString(msg, "{num}")

	// 9. Whitespace Normalization & Final Output Cap (256 bytes)
	msg = reWhitespace.ReplaceAllString(msg, " ")
	msg = strings.TrimSpace(msg)

	if len(msg) > 256 {
		msg = msg[:256]
	}

	return msg
}

// PatternSummary holds frequency statistics and metadata for a single normalized pattern.
type PatternSummary struct {
	Pattern    string `json:"pattern"`
	Count      int64  `json:"count"`
	SampleLine string `json:"sample_line"`  // First seen raw line (truncated to 300 bytes)
	SourceName string `json:"source_name"`  // Source where sample occurred
	LastSeenTS int64  `json:"last_seen_ts"` // UTC nanoseconds
}

// PatternCounter tracks normalized pattern frequencies with a hard memory bound.
type PatternCounter struct {
	mu         sync.Mutex
	maxTracked int
	entries    map[string]*PatternSummary
}

// NewPatternCounter initializes a PatternCounter bounded by maxTracked entries (default 4096).
func NewPatternCounter(maxTracked int) *PatternCounter {
	if maxTracked <= 0 {
		maxTracked = 4096
	}
	return &PatternCounter{
		maxTracked: maxTracked,
		entries:    make(map[string]*PatternSummary, maxTracked),
	}
}

// Add normalizes rawLine, updates pattern frequency, and enforces maxTracked capacity.
func (c *PatternCounter) Add(sourceName string, ts int64, rawLine string) {
	pattern := NormalizeMessage(rawLine)
	if pattern == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, exists := c.entries[pattern]; exists {
		entry.Count++
		if ts > entry.LastSeenTS {
			entry.LastSeenTS = ts
		}
		return
	}

	// Enforce memory bound: if capacity reached, evict bottom 25% lowest-count entries
	if len(c.entries) >= c.maxTracked {
		c.evictTailLocked()
	}

	sample := rawLine
	if len(sample) > 300 {
		sample = sample[:300] + "..."
	}

	c.entries[pattern] = &PatternSummary{
		Pattern:    pattern,
		Count:      1,
		SampleLine: sample,
		SourceName: sourceName,
		LastSeenTS: ts,
	}
}

// evictTailLocked removes the lowest-count entries to free up at least 25% capacity.
func (c *PatternCounter) evictTailLocked() {
	if len(c.entries) == 0 {
		return
	}

	type sortItem struct {
		key   string
		count int64
		ts    int64
	}

	items := make([]sortItem, 0, len(c.entries))
	for k, v := range c.entries {
		items = append(items, sortItem{
			key:   k,
			count: v.Count,
			ts:    v.LastSeenTS,
		})
	}

	// Sort ascending: lowest count first, oldest timestamp first
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count < items[j].count
		}
		return items[i].ts < items[j].ts
	})

	toEvict := len(c.entries) / 4
	if toEvict <= 0 {
		toEvict = 1
	}

	for i := 0; i < toEvict && i < len(items); i++ {
		delete(c.entries, items[i].key)
	}
}

// TopN returns the top n patterns sorted by Count DESC, LastSeenTS DESC, Pattern ASC.
func (c *PatternCounter) TopN(n int) []PatternSummary {
	if n <= 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	list := make([]PatternSummary, 0, len(c.entries))
	for _, entry := range c.entries {
		list = append(list, *entry)
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		if list[i].LastSeenTS != list[j].LastSeenTS {
			return list[i].LastSeenTS > list[j].LastSeenTS
		}
		return list[i].Pattern < list[j].Pattern
	})

	if len(list) > n {
		list = list[:n]
	}

	return list
}

// Len returns the current count of tracked patterns.
func (c *PatternCounter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
