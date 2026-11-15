package logs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

var (
	// ErrInvalidQuery is returned when a query pattern exceeds 512 bytes or regex compilation fails.
	ErrInvalidQuery = errors.New("invalid query: pattern exceeds 512 bytes or regex is malformed")

	// ErrRecordNotFound is returned when a requested record ID cannot be found for context lookup.
	ErrRecordNotFound = errors.New("log record not found")
)

// LogQuery defines search parameters for querying logs from TailCache and disk.
type LogQuery struct {
	SourceID   int64
	Query      string       // Optional substring or regex pattern
	Regex      bool         // If true, compile with regexp.Compile (<= 512 chars)
	IgnoreCase bool         // Default true for substring/regex search
	MinLevel   model.Level  // 0 = all levels, 1..5 = minimum severity
	Stream     model.Stream // 0 = all streams, 1 = stdout, 2 = stderr
	FromTS     int64        // Inclusive UTC nanoseconds (0 = unbounded)
	ToTS       int64        // Inclusive UTC nanoseconds (0 = unbounded)
	Cursor     string       // Opaque keyset cursor from a previous query result
	CursorTS   int64        // Explicit cursor TS (if Cursor string not used)
	CursorTie  uint64       // Explicit tie-breaker
	Limit      int          // Default 200, clamped to 1..1000
}

// LogRecord represents a single matching log record returned by the query engine.
type LogRecord struct {
	ID       string                // e.g., "<date>:<blockOffset>:<recIdx>" or "mem:<seq>"
	SourceID int64
	TS       int64                 // UTC nanoseconds
	TSSrc    model.TimestampSource // 0..3
	Level    model.Level           // 0..5
	LevelSrc model.LevelSource     // 0..5
	Stream   model.Stream          // 0..2
	Line     string
}

// LogQueryResult encapsulates the list of matching entries and pagination state.
type LogQueryResult struct {
	Entries       []LogRecord
	NextCursor    string // Opaque keyset cursor for next older page; empty if no more results
	HasMore       bool
	ScannedBlocks int
	FromCache     bool
}

// LogQueryEngineConfig configures the LogQueryEngine.
type LogQueryEngineConfig struct {
	BaseDir     string
	TailCache   *TailCache
	IndexReader *IndexReader
}

// LogQueryEngine unifies hot in-memory tail reading and disk .idx/.chunk streaming traversal.
type LogQueryEngine struct {
	baseDir     string
	tailCache   *TailCache
	indexReader *IndexReader
	dec         *zstd.Decoder
	rawPool     *sync.Pool
	compPool    *sync.Pool
}

// NewLogQueryEngine creates and initializes a LogQueryEngine.
func NewLogQueryEngine(cfg LogQueryEngineConfig) (*LogQueryEngine, error) {
	if cfg.BaseDir == "" {
		cfg.BaseDir = "/data/logs"
	}
	if cfg.IndexReader == nil {
		cfg.IndexReader = NewIndexReader(cfg.BaseDir)
	}

	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}

	return &LogQueryEngine{
		baseDir:     cfg.BaseDir,
		tailCache:   cfg.TailCache,
		indexReader: cfg.IndexReader,
		dec:         dec,
		rawPool: &sync.Pool{
			New: func() any {
				b := make([]byte, 68*1024)
				return &b
			},
		},
		compPool: &sync.Pool{
			New: func() any {
				b := make([]byte, 70*1024)
				return &b
			},
		},
	}, nil
}

// Search executes a unified query across TailCache and disk .idx/.chunk blocks in reverse chronological order.
func (e *LogQueryEngine) Search(ctx context.Context, q LogQuery) (LogQueryResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultTailLimit
	} else if limit > MaxTailLimit {
		limit = MaxTailLimit
	}

	// 1. Parse and validate matcher
	matcher, err := compileMatcher(q.Query, q.Regex, q.IgnoreCase)
	if err != nil {
		return LogQueryResult{}, err
	}

	// 2. Parse cursor
	cursorTS := q.CursorTS
	cursorTie := q.CursorTie
	isMemCursor := false

	if q.Cursor != "" {
		cTS, cTie, isMem, cErr := DecodeCursor(q.Cursor)
		if cErr == nil {
			cursorTS = cTS
			cursorTie = cTie
			isMemCursor = isMem
		}
	}

	var results []LogRecord

	// 3. Tier 1 — Hot TailCache Fast Path
	// Eligible when query has no text filter and cursor is either empty or in-memory
	if !matcher.hasQuery && e.tailCache != nil && (q.Cursor == "" || isMemCursor) {
		tailRes := e.tailCache.QueryTail(TailQuery{
			SourceID:  q.SourceID,
			MinLevel:  q.MinLevel,
			Stream:    q.Stream,
			FromTS:    q.FromTS,
			ToTS:      q.ToTS,
			BeforeTS:  cursorTS,
			BeforeSeq: cursorTie,
			Limit:     limit + 1, // Read 1 extra to check HasMore
		})

		for _, te := range tailRes.Entries {
			results = append(results, LogRecord{
				ID:       fmt.Sprintf("mem:%d", te.Seq),
				SourceID: te.SourceID,
				TS:       te.TS,
				TSSrc:    te.TSSrc,
				Level:    te.Level,
				LevelSrc: te.LevelSrc,
				Stream:   te.Stream,
				Line:     te.Line,
			})
		}

		// If TailCache has enough entries to satisfy limit + 1
		if len(results) > limit {
			trimmed := results[:limit]
			last := trimmed[len(trimmed)-1]
			seq, _ := strconv.ParseUint(last.ID[4:], 10, 64)
			return LogQueryResult{
				Entries:       trimmed,
				NextCursor:    EncodeCursor(last.TS, seq, true),
				HasMore:       true,
				ScannedBlocks: 0,
				FromCache:     true,
			}, nil
		}

		// If TailCache returned fewer than limit, and does not have older entries on disk
		if !tailRes.HasMoreOlder {
			return LogQueryResult{
				Entries:       results,
				NextCursor:    "",
				HasMore:       false,
				ScannedBlocks: 0,
				FromCache:     true,
			}, nil
		}

		// Otherwise, TailCache was partially exhausted (or older entries exist on disk).
		// We keep whatever entries we got from TailCache, and continue reading disk.
	}

	// 4. Tier 2 — Sparse .idx Pruning + Disk Block Streaming
	neededFromDisk := limit + 1 - len(results)
	if neededFromDisk <= 0 {
		trimmed := results[:limit]
		last := trimmed[len(trimmed)-1]
		return LogQueryResult{
			Entries:       trimmed,
			NextCursor:    encodeRecordCursor(last),
			HasMore:       true,
			ScannedBlocks: 0,
			FromCache:     len(results) > 0,
		}, nil
	}

	// Effective ToTS for disk query
	effectiveToTS := q.ToTS
	if len(results) > 0 {
		lastMem := results[len(results)-1]
		if effectiveToTS == 0 || lastMem.TS < effectiveToTS {
			effectiveToTS = lastMem.TS
		}
		cursorTS = lastMem.TS
		cursorTie = 0
		isMemCursor = true
	}

	var streamMask uint8
	if q.Stream != model.StreamNA {
		streamMask = 1 << uint8(q.Stream)
	}

	candidates, err := e.indexReader.SearchBlocks(BlockQuery{
		SourceID:   q.SourceID,
		FromTS:     q.FromTS,
		ToTS:       effectiveToTS,
		MinLevel:   q.MinLevel,
		StreamMask: streamMask,
		Descending: true, // newest block first
	})
	if err != nil {
		return LogQueryResult{}, fmt.Errorf("search blocks: %w", err)
	}

	scannedBlocks := 0
	var currentChunkFile *os.File
	var currentChunkDate string
	defer func() {
		if currentChunkFile != nil {
			_ = currentChunkFile.Close()
		}
	}()

	rawPtr := e.rawPool.Get().(*[]byte)
	rawBuf := *rawPtr
	defer e.rawPool.Put(rawPtr)

	compPtr := e.compPool.Get().(*[]byte)
	compBuf := *compPtr
	defer e.compPool.Put(compPtr)

	for _, cand := range candidates {
		if ctx.Err() != nil {
			return LogQueryResult{}, ctx.Err()
		}

		scannedBlocks++

		// Switch chunk file when date changes
		if currentChunkFile == nil || currentChunkDate != cand.Date {
			if currentChunkFile != nil {
				_ = currentChunkFile.Close()
				currentChunkFile = nil
			}
			chunkPath := filepath.Join(e.baseDir, strconv.FormatInt(q.SourceID, 10), cand.Date+".chunk")
			f, oErr := os.Open(chunkPath)
			if oErr != nil {
				continue
			}
			currentChunkFile = f
			currentChunkDate = cand.Date
		}

		// Read compressed block frame
		compLen := int(cand.Meta.CompressedLen)
		if cap(compBuf) < compLen {
			compBuf = make([]byte, compLen+4096)
			*compPtr = compBuf
		}
		frameBytes := compBuf[:compLen]

		if _, rErr := currentChunkFile.ReadAt(frameBytes, cand.Meta.FileOffset); rErr != nil {
			continue
		}

		// Decompress block
		decompBytes, decErr := e.dec.DecodeAll(frameBytes, rawBuf[:0])
		if decErr != nil {
			continue
		}

		// Parse records into slice of lightweight views
		views, pErr := parseBlockRecordViews(decompBytes)
		if pErr != nil {
			continue
		}

		// Traverse records in reverse chronological order (newest -> oldest)
		for i := len(views) - 1; i >= 0; i-- {
			rec := views[i]

			// Keyset boundary check
			recTie := (uint64(cand.Meta.FileOffset) << 20) | uint64(rec.recIdx)
			if cursorTS > 0 || cursorTie > 0 {
				if isMemCursor {
					if rec.ts >= cursorTS {
						continue
					}
				} else {
					if rec.ts > cursorTS {
						continue
					}
					if rec.ts == cursorTS && recTie >= cursorTie {
						continue
					}
				}
			}

			// Time window check
			if q.FromTS > 0 && rec.ts < q.FromTS {
				continue
			}
			if q.ToTS > 0 && rec.ts > q.ToTS {
				continue
			}

			// Level filter
			if q.MinLevel > 0 && rec.level < q.MinLevel {
				continue
			}

			// Stream filter
			if q.Stream != model.StreamNA && rec.stream != q.Stream {
				continue
			}

			// Query matcher
			if !matcher.Match(rec.lineBytes) {
				continue
			}

			// Record passes all filters
			recordID := fmt.Sprintf("%s:%d:%d", cand.Date, cand.Meta.FileOffset, rec.recIdx)
			results = append(results, LogRecord{
				ID:       recordID,
				SourceID: q.SourceID,
				TS:       rec.ts,
				TSSrc:    rec.tsSrc,
				Level:    rec.level,
				LevelSrc: rec.levelSrc,
				Stream:   rec.stream,
				Line:     string(rec.lineBytes),
			})

			if len(results) == limit+1 {
				break
			}
		}

		if len(results) == limit+1 {
			break
		}
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	var nextCursor string
	if hasMore && len(results) > 0 {
		last := results[len(results)-1]
		nextCursor = encodeRecordCursor(last)
	}

	return LogQueryResult{
		Entries:       results,
		NextCursor:    nextCursor,
		HasMore:       hasMore,
		ScannedBlocks: scannedBlocks,
		FromCache:     false,
	}, nil
}

// ReadContext retrieves surrounding lines (up to `before` preceding and `after` succeeding) for recordID.
func (e *LogQueryEngine) ReadContext(ctx context.Context, sourceID int64, recordID string, before, after int) ([]LogRecord, int, error) {
	if before < 0 {
		before = 20
	} else if before > 100 {
		before = 100
	}
	if after < 0 {
		after = 20
	} else if after > 100 {
		after = 100
	}

	// 1. If in-memory locator "mem:<seq>"
	if strings.HasPrefix(recordID, "mem:") {
		seqStr := recordID[4:]
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err == nil && e.tailCache != nil {
			tailEntries, found := e.tailCache.QueryContext(sourceID, 0, seq, before, after)
			if found {
				records := make([]LogRecord, len(tailEntries))
				targetIdx := -1
				for i, te := range tailEntries {
					records[i] = LogRecord{
						ID:       fmt.Sprintf("mem:%d", te.Seq),
						SourceID: te.SourceID,
						TS:       te.TS,
						TSSrc:    te.TSSrc,
						Level:    te.Level,
						LevelSrc: te.LevelSrc,
						Stream:   te.Stream,
						Line:     te.Line,
					}
					if te.Seq == seq {
						targetIdx = i
					}
				}
				if targetIdx >= 0 {
					return records, targetIdx, nil
				}
			}
		}
	}

	// 2. Disk locator "<date>:<blockOffset>:<recIdx>"
	parts := strings.Split(recordID, ":")
	if len(parts) != 3 {
		return nil, 0, ErrRecordNotFound
	}

	targetDate := parts[0]
	targetOffset, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, 0, ErrRecordNotFound
	}
	targetRecIdx, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, 0, ErrRecordNotFound
	}

	sourceDir := filepath.Join(e.baseDir, strconv.FormatInt(sourceID, 10))
	idxPath := filepath.Join(sourceDir, targetDate+".idx")
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, 0, ErrRecordNotFound
	}

	numBlocks := len(idxData) / IndexRecordSize
	var blocks []BlockMeta
	targetBlockIdx := -1

	for i := 0; i < numBlocks; i++ {
		b, bErr := UnmarshalIndexRecord(idxData[i*IndexRecordSize : (i+1)*IndexRecordSize])
		if bErr != nil {
			continue
		}
		b.Date = targetDate
		b.SourceID = sourceID
		if b.FileOffset == targetOffset {
			targetBlockIdx = len(blocks)
		}
		blocks = append(blocks, b)
	}

	if targetBlockIdx == -1 {
		return nil, 0, ErrRecordNotFound
	}

	// Determine start and end block indices to cover requested before/after window
	startBlockIdx := targetBlockIdx
	if targetRecIdx < before && targetBlockIdx > 0 {
		startBlockIdx = targetBlockIdx - 1
	}

	targetBlock := blocks[targetBlockIdx]
	endBlockIdx := targetBlockIdx
	if targetRecIdx+after >= int(targetBlock.RecordCount) && targetBlockIdx+1 < len(blocks) {
		endBlockIdx = targetBlockIdx + 1
	}

	chunkPath := filepath.Join(sourceDir, targetDate+".chunk")
	fChunk, err := os.Open(chunkPath)
	if err != nil {
		return nil, 0, err
	}
	defer fChunk.Close()

	var allRecords []LogRecord
	targetAbsIdx := -1

	for bIdx := startBlockIdx; bIdx <= endBlockIdx; bIdx++ {
		blk := blocks[bIdx]
		frameBytes := make([]byte, blk.CompressedLen)
		if _, rErr := fChunk.ReadAt(frameBytes, blk.FileOffset); rErr != nil {
			return nil, 0, rErr
		}

		rawBytes, decErr := e.dec.DecodeAll(frameBytes, nil)
		if decErr != nil {
			return nil, 0, decErr
		}

		entries, dErr := DecodeBlock(rawBytes, strconv.FormatInt(sourceID, 10))
		if dErr != nil {
			return nil, 0, dErr
		}

		for rIdx, ent := range entries {
			isTarget := (bIdx == targetBlockIdx && rIdx == targetRecIdx)
			if isTarget {
				targetAbsIdx = len(allRecords)
			}
			allRecords = append(allRecords, LogRecord{
				ID:       fmt.Sprintf("%s:%d:%d", targetDate, blk.FileOffset, rIdx),
				SourceID: sourceID,
				TS:       ent.Timestamp.UnixNano(),
				TSSrc:    ent.TimestampSource,
				Level:    ent.Level,
				LevelSrc: ent.LevelSource,
				Stream:   ent.Stream,
				Line:     string(ent.Message),
			})
		}
	}

	if targetAbsIdx == -1 {
		return nil, 0, ErrRecordNotFound
	}

	start := max(0, targetAbsIdx-before)
	end := min(len(allRecords), targetAbsIdx+after+1)

	sub := allRecords[start:end]
	targetIndexInSlice := targetAbsIdx - start

	return sub, targetIndexInSlice, nil
}

type recordView struct {
	ts        int64
	level     model.Level
	levelSrc  model.LevelSource
	tsSrc     model.TimestampSource
	stream    model.Stream
	lineBytes []byte
	recIdx    int
}

// parseBlockRecordViews parses record headers from an uncompressed block into lightweight subslice views.
func parseBlockRecordViews(raw []byte) ([]recordView, error) {
	recordCount, uncompressedLen, err := DecodeBlockHeader(raw)
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) < uncompressedLen {
		return nil, ErrCorruptBlock
	}

	views := make([]recordView, 0, recordCount)
	offset := BlockHeaderSize
	limit := int(uncompressedLen)

	for i := uint32(0); i < recordCount; i++ {
		if offset+RecordHeaderSize > limit {
			return nil, ErrCorruptBlock
		}
		hdr := raw[offset : offset+RecordHeaderSize]
		tsNano := int64(binaryLittleEndianUint64(hdr[0:8]))
		lvl := model.Level(hdr[8])
		lvlSrc := model.LevelSource(hdr[9])
		tsSrc := model.TimestampSource(hdr[10])
		stream := model.Stream(hdr[11])
		lineLen := int(binaryLittleEndianUint32(hdr[12:16]))

		totalSize := RecordHeaderSize + lineLen
		if offset+totalSize > limit {
			return nil, ErrCorruptBlock
		}

		lineBytes := raw[offset+RecordHeaderSize : offset+totalSize]
		views = append(views, recordView{
			ts:        tsNano,
			level:     lvl,
			levelSrc:  lvlSrc,
			tsSrc:     tsSrc,
			stream:    stream,
			lineBytes: lineBytes,
			recIdx:    int(i),
		})

		offset += totalSize
	}

	return views, nil
}

func binaryLittleEndianUint64(b []byte) uint64 {
	_ = b[7]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func binaryLittleEndianUint32(b []byte) uint32 {
	_ = b[3]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

type lineMatcher struct {
	hasQuery   bool
	isRegex    bool
	re         *regexp.Regexp
	needle     []byte
	ignoreCase bool
}

func compileMatcher(pattern string, isRegex, ignoreCase bool) (lineMatcher, error) {
	if pattern == "" {
		return lineMatcher{hasQuery: false}, nil
	}
	if len(pattern) > 512 {
		return lineMatcher{}, ErrInvalidQuery
	}

	if isRegex {
		rePattern := pattern
		if ignoreCase {
			rePattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(rePattern)
		if err != nil {
			return lineMatcher{}, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
		}
		return lineMatcher{
			hasQuery: true,
			isRegex:  true,
			re:       re,
		}, nil
	}

	needle := []byte(pattern)
	if ignoreCase {
		needle = toLowerASCII(needle)
	}

	return lineMatcher{
		hasQuery:   true,
		isRegex:    false,
		needle:     needle,
		ignoreCase: ignoreCase,
	}, nil
}

func (m *lineMatcher) Match(line []byte) bool {
	if !m.hasQuery {
		return true
	}
	if m.isRegex {
		return m.re.Match(line)
	}
	if !m.ignoreCase {
		return containsSubslice(line, m.needle)
	}
	return caseFoldContains(line, m.needle)
}

func containsSubslice(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	first := needle[0]
	maxIdx := len(haystack) - len(needle)
	for i := 0; i <= maxIdx; i++ {
		if haystack[i] == first {
			match := true
			for j := 1; j < len(needle); j++ {
				if haystack[i+j] != needle[j] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func caseFoldContains(haystack, needleLower []byte) bool {
	if len(needleLower) == 0 {
		return true
	}
	if len(haystack) < len(needleLower) {
		return false
	}
	first := needleLower[0]
	maxIdx := len(haystack) - len(needleLower)
	for i := 0; i <= maxIdx; i++ {
		c := haystack[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == first {
			match := true
			for j := 1; j < len(needleLower); j++ {
				hc := haystack[i+j]
				if hc >= 'A' && hc <= 'Z' {
					hc += 'a' - 'A'
				}
				if hc != needleLower[j] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func toLowerASCII(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func encodeRecordCursor(rec LogRecord) string {
	if strings.HasPrefix(rec.ID, "mem:") {
		seq, _ := strconv.ParseUint(rec.ID[4:], 10, 64)
		return EncodeCursor(rec.TS, seq, true)
	}
	parts := strings.Split(rec.ID, ":")
	if len(parts) == 3 {
		offset, _ := strconv.ParseInt(parts[1], 10, 64)
		recIdx, _ := strconv.ParseUint(parts[2], 10, 32)
		tie := (uint64(offset) << 20) | recIdx
		return EncodeCursor(rec.TS, tie, false)
	}
	return EncodeCursor(rec.TS, 0, false)
}

// EncodeCursor produces a compact URL-safe base64 string representing a pagination cursor.
func EncodeCursor(ts int64, tie uint64, isMem bool) string {
	tag := "d"
	if isMem {
		tag = "m"
	}
	raw := fmt.Sprintf("%s:%d:%d", tag, ts, tie)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor decodes a cursor string generated by EncodeCursor.
func DecodeCursor(s string) (ts int64, tie uint64, isMem bool, err error) {
	if s == "" {
		return 0, 0, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, 0, false, err
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 {
		return 0, 0, false, errors.New("invalid cursor format")
	}
	isMem = (parts[0] == "m")
	ts, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false, err
	}
	tie, err = strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return 0, 0, false, err
	}
	return ts, tie, isMem, nil
}
