package logs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

const (
	// IndexRecordSize is the exact byte length of a single .idx record (36 bytes).
	IndexRecordSize = 36

	// DefaultMaxOpenIndexFiles is the maximum number of cached open .idx file handles.
	DefaultMaxOpenIndexFiles = 64
)

var (
	// ErrCorruptIndex is returned when an index record is truncated or malformed.
	ErrCorruptIndex = errors.New("corrupt index record: length less than 36 bytes")
)

// MarshalIndexRecord writes m into dst in little-endian binary format with 0 heap allocations.
// Layout (36 bytes):
//   [0:8]   MinTS (int64, little-endian)
//   [8:16]  MaxTS (int64, little-endian)
//   [16]    MinLevel (uint8)
//   [17]    MaxLevel (uint8)
//   [18]    StreamMask (uint8)
//   [19]    Flags (uint8, reserved 0x00)
//   [20:28] FileOffset (int64, little-endian)
//   [28:32] CompressedLen (uint32, little-endian)
//   [32:36] RecordCount (uint32, little-endian)
func MarshalIndexRecord(dst *[IndexRecordSize]byte, m BlockMeta) {
	binary.LittleEndian.PutUint64(dst[0:8], uint64(m.MinTS))
	binary.LittleEndian.PutUint64(dst[8:16], uint64(m.MaxTS))
	dst[16] = uint8(m.MinLevel)
	dst[17] = uint8(m.MaxLevel)
	dst[18] = m.StreamMask
	dst[19] = 0x00
	binary.LittleEndian.PutUint64(dst[20:28], uint64(m.FileOffset))
	binary.LittleEndian.PutUint32(dst[28:32], m.CompressedLen)
	binary.LittleEndian.PutUint32(dst[32:36], m.RecordCount)
}

// UnmarshalIndexRecord decodes a 36-byte index record from src.
func UnmarshalIndexRecord(src []byte) (BlockMeta, error) {
	if len(src) < IndexRecordSize {
		return BlockMeta{}, ErrCorruptIndex
	}
	return BlockMeta{
		MinTS:         int64(binary.LittleEndian.Uint64(src[0:8])),
		MaxTS:         int64(binary.LittleEndian.Uint64(src[8:16])),
		MinLevel:      model.Level(src[16]),
		MaxLevel:      model.Level(src[17]),
		StreamMask:    src[18],
		FileOffset:    int64(binary.LittleEndian.Uint64(src[20:28])),
		CompressedLen: binary.LittleEndian.Uint32(src[28:32]),
		RecordCount:   binary.LittleEndian.Uint32(src[32:36]),
	}, nil
}

type cachedIndexFile struct {
	file       *os.File
	lastAccess time.Time
}

// IndexManager manages append-only .idx companion files with bounded open handles.
type IndexManager struct {
	mu        sync.Mutex
	baseDir   string
	openFiles map[string]*cachedIndexFile
	maxOpen   int
}

// NewIndexManager creates a new IndexManager for baseDir.
func NewIndexManager(baseDir string) *IndexManager {
	if baseDir == "" {
		baseDir = "/data/logs"
	}
	return &IndexManager{
		baseDir:   baseDir,
		openFiles: make(map[string]*cachedIndexFile),
		maxOpen:   DefaultMaxOpenIndexFiles,
	}
}

// AppendBlock appends a 36-byte index entry for meta to <baseDir>/<sourceID>/<date>.idx.
func (m *IndexManager) AppendBlock(meta BlockMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Join(m.baseDir, strconv.FormatInt(meta.SourceID, 10))
	idxPath := filepath.Join(dir, meta.Date+".idx")

	f, err := m.getOrCreateFileLocked(idxPath, dir)
	if err != nil {
		return fmt.Errorf("open idx file %s: %w", idxPath, err)
	}

	var buf [IndexRecordSize]byte
	MarshalIndexRecord(&buf, meta)

	if _, err := f.Write(buf[:]); err != nil {
		return fmt.Errorf("write idx file %s: %w", idxPath, err)
	}

	return nil
}

func (m *IndexManager) getOrCreateFileLocked(idxPath, dir string) (*os.File, error) {
	if entry, exists := m.openFiles[idxPath]; exists {
		entry.lastAccess = time.Now()
		return entry.file, nil
	}

	if len(m.openFiles) >= m.maxOpen {
		var oldestPath string
		var oldestTime time.Time
		first := true
		for p, ent := range m.openFiles {
			if first || ent.lastAccess.Before(oldestTime) {
				oldestPath = p
				oldestTime = ent.lastAccess
				first = false
			}
		}
		if oldestPath != "" {
			old := m.openFiles[oldestPath]
			_ = old.file.Sync()
			_ = old.file.Close()
			delete(m.openFiles, oldestPath)
		}
	}

	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(idxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, err
	}

	m.openFiles[idxPath] = &cachedIndexFile{
		file:       f,
		lastAccess: time.Now(),
	}

	return f, nil
}

// Sync flushes all open index file descriptors.
func (m *IndexManager) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for _, entry := range m.openFiles {
		if err := entry.file.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close syncs and closes all open index files.
func (m *IndexManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for p, entry := range m.openFiles {
		if err := entry.file.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := entry.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.openFiles, p)
	}
	return firstErr
}

// BlockQuery configures candidate block retrieval.
type BlockQuery struct {
	SourceID   int64
	FromTS     int64       // Inclusive UTC nanoseconds (0 = unbounded)
	ToTS       int64       // Inclusive UTC nanoseconds (0 = unbounded)
	MinLevel   model.Level // Filter: skip blocks where MaxLevel < MinLevel (unless MinLevel == 0)
	StreamMask uint8       // Bitmask of allowed streams (0 = all streams allowed)
	Descending bool        // true = newest-first block order, false = oldest-first
}

// CandidateBlock represents a matching block candidate for decompression.
type CandidateBlock struct {
	Date string
	Meta BlockMeta
}

// IndexReader performs fast binary search and block filtering over .idx files.
type IndexReader struct {
	baseDir string
}

// NewIndexReader creates a new IndexReader for baseDir.
func NewIndexReader(baseDir string) *IndexReader {
	if baseDir == "" {
		baseDir = "/data/logs"
	}
	return &IndexReader{baseDir: baseDir}
}

// SearchBlocks evaluates q against all relevant .idx files for q.SourceID in O(log N) time per file.
func (r *IndexReader) SearchBlocks(q BlockQuery) ([]CandidateBlock, error) {
	sourceDir := filepath.Join(r.baseDir, strconv.FormatInt(q.SourceID, 10))
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// 1. Discover and sort .idx files
	var idxFiles []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && filepath.Ext(name) == ".idx" {
			idxFiles = append(idxFiles, name)
		}
	}
	sort.Strings(idxFiles)

	if len(idxFiles) == 0 {
		return nil, nil
	}

	// Reverse file order if descending
	if q.Descending {
		for i, j := 0, len(idxFiles)-1; i < j; i, j = i+1, j-1 {
			idxFiles[i], idxFiles[j] = idxFiles[j], idxFiles[i]
		}
	}

	var results []CandidateBlock

	// 2. Scan relevant .idx files
	for _, fname := range idxFiles {
		dateStr := fname[:len(fname)-4]

		// Date-level pre-filtering (allow 24h margin for clock jitter)
		if tDate, err := time.Parse("2006-01-02", dateStr); err == nil {
			dayStartTS := tDate.UnixNano()
			dayEndTS := tDate.Add(24 * time.Hour).UnixNano()

			const margin = int64(24 * time.Hour)
			if q.FromTS > 0 && dayEndTS+margin < q.FromTS {
				continue
			}
			if q.ToTS > 0 && dayStartTS-margin > q.ToTS {
				continue
			}
		}

		filePath := filepath.Join(sourceDir, fname)
		data, err := os.ReadFile(filePath)
		if err != nil || len(data) < IndexRecordSize {
			continue
		}

		numRecords := len(data) / IndexRecordSize
		if numRecords == 0 {
			continue
		}

		// Binary search for lower bound where MaxTS >= q.FromTS
		var startIdx int
		if q.FromTS > 0 {
			startIdx = sort.Search(numRecords, func(i int) bool {
				offset := i * IndexRecordSize
				recMaxTS := int64(binary.LittleEndian.Uint64(data[offset+8 : offset+16]))
				return recMaxTS >= q.FromTS
			})
		} else {
			startIdx = 0
		}

		var fileCandidates []CandidateBlock

		// Linear scan from startIdx forward
		for i := startIdx; i < numRecords; i++ {
			offset := i * IndexRecordSize
			meta, decErr := UnmarshalIndexRecord(data[offset : offset+IndexRecordSize])
			if decErr != nil {
				continue
			}
			meta.SourceID = q.SourceID
			meta.Date = dateStr

			// If block start is well past query window, stop scanning this file
			if q.ToTS > 0 && meta.MinTS > q.ToTS {
				break
			}

			// Time window filter
			if q.FromTS > 0 && meta.MaxTS < q.FromTS {
				continue
			}
			if q.ToTS > 0 && meta.MinTS > q.ToTS {
				continue
			}

			// Level pruning
			if q.MinLevel > 0 && meta.MaxLevel < q.MinLevel {
				continue
			}

			// Stream mask pruning
			if q.StreamMask != 0 && (meta.StreamMask&q.StreamMask) == 0 {
				continue
			}

			fileCandidates = append(fileCandidates, CandidateBlock{
				Date: dateStr,
				Meta: meta,
			})
		}

		if q.Descending {
			// Reverse within this file's candidates to maintain newest-first
			for i, j := 0, len(fileCandidates)-1; i < j; i, j = i+1, j-1 {
				fileCandidates[i], fileCandidates[j] = fileCandidates[j], fileCandidates[i]
			}
		}

		results = append(results, fileCandidates...)
	}

	return results, nil
}

// RepairIfNeeded fixes torn index tails and recovers unindexed .chunk blocks after an unclean shutdown.
func RepairIfNeeded(chunkPath, idxPath string) error {
	chunkInfo, chunkErr := os.Stat(chunkPath)
	if chunkErr != nil {
		if os.IsNotExist(chunkErr) {
			return nil
		}
		return chunkErr
	}
	chunkSize := chunkInfo.Size()

	idxInfo, idxErr := os.Stat(idxPath)
	var idxSize int64
	if idxErr != nil {
		if !os.IsNotExist(idxErr) {
			return idxErr
		}
		idxSize = 0
	} else {
		idxSize = idxInfo.Size()
	}

	// 1. Truncate torn partial index entry if idxSize % 36 != 0
	if idxSize%IndexRecordSize != 0 {
		validSize := (idxSize / IndexRecordSize) * IndexRecordSize
		if err := os.Truncate(idxPath, validSize); err != nil {
			return fmt.Errorf("truncate torn idx: %w", err)
		}
		idxSize = validSize
	}

	// 2. Inspect last valid index entry
	var indexedChunkEnd int64
	if idxSize >= IndexRecordSize {
		fIdx, err := os.OpenFile(idxPath, os.O_RDWR, 0640)
		if err != nil {
			return err
		}

		for idxSize >= IndexRecordSize {
			var recBuf [IndexRecordSize]byte
			if _, err := fIdx.ReadAt(recBuf[:], idxSize-IndexRecordSize); err != nil {
				_ = fIdx.Close()
				return err
			}
			lastMeta, err := UnmarshalIndexRecord(recBuf[:])
			if err != nil {
				_ = fIdx.Close()
				return err
			}
			blockEnd := lastMeta.FileOffset + int64(lastMeta.CompressedLen)
			if blockEnd <= chunkSize {
				indexedChunkEnd = blockEnd
				break
			}
			// Index points past chunk EOF, discard record
			idxSize -= IndexRecordSize
			if err := fIdx.Truncate(idxSize); err != nil {
				_ = fIdx.Close()
				return err
			}
		}
		_ = fIdx.Close()
	}

	// 3. Recover unindexed tail chunk blocks
	if chunkSize > indexedChunkEnd {
		chunkBytes, err := os.ReadFile(chunkPath)
		if err != nil {
			return fmt.Errorf("read chunk file: %w", err)
		}

		dec, err := zstd.NewReader(nil)
		if err != nil {
			return fmt.Errorf("create zstd reader: %w", err)
		}
		defer dec.Close()

		dir := filepath.Dir(idxPath)
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}

		fIdx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
		if err != nil {
			return fmt.Errorf("open idx for append: %w", err)
		}
		defer fIdx.Close()

		currOffset := indexedChunkEnd
		var validChunkEnd = indexedChunkEnd

		for currOffset < int64(len(chunkBytes)) {
			frameData := chunkBytes[currOffset:]
			frameLen, fErr := zstdFrameSize(frameData)
			if fErr != nil || currOffset+int64(frameLen) > int64(len(chunkBytes)) {
				break
			}

			compressedFrame := frameData[:frameLen]
			rawBytes, decErr := dec.DecodeAll(compressedFrame, nil)
			if decErr != nil {
				break
			}

			recordCount, uncompressedLen, hErr := DecodeBlockHeader(rawBytes)
			if hErr != nil || uint32(len(rawBytes)) < uncompressedLen {
				break
			}

			entries, bErr := DecodeBlock(rawBytes, "")
			if bErr != nil || uint32(len(entries)) != recordCount {
				break
			}

			var minTS, maxTS int64
			var minLevel, maxLevel model.Level
			var streamMask uint8

			for i, ent := range entries {
				ts := ent.Timestamp.UnixNano()
				if i == 0 {
					minTS = ts
					maxTS = ts
					minLevel = ent.Level
					maxLevel = ent.Level
				} else {
					if ts < minTS {
						minTS = ts
					}
					if ts > maxTS {
						maxTS = ts
					}
					if ent.Level < minLevel {
						minLevel = ent.Level
					}
					if ent.Level > maxLevel {
						maxLevel = ent.Level
					}
				}
				streamMask |= 1 << uint8(ent.Stream)
			}

			meta := BlockMeta{
				MinTS:           minTS,
				MaxTS:           maxTS,
				MinLevel:        minLevel,
				MaxLevel:        maxLevel,
				StreamMask:      streamMask,
				FileOffset:      currOffset,
				CompressedLen:   uint32(frameLen),
				UncompressedLen: uncompressedLen,
				RecordCount:     recordCount,
			}

			var idxBuf [IndexRecordSize]byte
			MarshalIndexRecord(&idxBuf, meta)
			if _, wErr := fIdx.Write(idxBuf[:]); wErr != nil {
				return fmt.Errorf("write recovered idx record: %w", wErr)
			}

			currOffset += int64(frameLen)
			validChunkEnd = currOffset
		}

		// Truncate chunk file if corrupt/incomplete bytes were at EOF
		if validChunkEnd < chunkSize {
			if err := os.Truncate(chunkPath, validChunkEnd); err != nil {
				return fmt.Errorf("truncate torn chunk: %w", err)
			}
		}
	}

	return nil
}

// zstdFrameSize calculates the exact compressed byte length of the leading Zstd frame in data.
func zstdFrameSize(data []byte) (int, error) {
	var h zstd.Header
	if err := h.Decode(data); err != nil {
		return 0, err
	}
	offset := h.HeaderSize
	for {
		if offset+3 > len(data) {
			return 0, fmt.Errorf("truncated block header")
		}
		b0 := data[offset]
		b1 := data[offset+1]
		b2 := data[offset+2]
		last := (b0 & 1) != 0
		blockType := (b0 >> 1) & 0x03
		blockSize := int(b0>>3) | (int(b1) << 5) | (int(b2) << 13)

		payloadSize := blockSize
		if blockType == 1 { // RLE
			payloadSize = 1
		} else if blockType == 3 {
			return 0, fmt.Errorf("reserved block type")
		}

		offset += 3 + payloadSize
		if offset > len(data) {
			return 0, fmt.Errorf("truncated block payload")
		}
		if last {
			break
		}
	}
	if h.HasCheckSum {
		offset += 4
		if offset > len(data) {
			return 0, fmt.Errorf("truncated checksum")
		}
	}
	return offset, nil
}
