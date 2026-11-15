package logs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

var (
	// BlockMagic is the 4-byte identifier at the start of every uncompressed Tarsier log block ("TRLB").
	BlockMagic = [4]byte{'T', 'R', 'L', 'B'}

	// ErrInvalidMagic is returned when a block header does not start with BlockMagic.
	ErrInvalidMagic = errors.New("invalid block magic bytes: expected TRLB")

	// ErrUnsupportedVersion is returned when a block version is not supported.
	ErrUnsupportedVersion = errors.New("unsupported block version")

	// ErrCorruptBlock is returned when block length or record fields are truncated or invalid.
	ErrCorruptBlock = errors.New("corrupt block: payload truncated or length mismatch")
)

const (
	// BlockVersion is the current block layout version (1).
	BlockVersion uint8 = 0x01

	// BlockHeaderSize is the exact byte length of the uncompressed block header (16 bytes).
	BlockHeaderSize = 16

	// RecordHeaderSize is the exact byte length of each log entry's binary metadata header (16 bytes).
	RecordHeaderSize = 16

	// MaxLineLength is the maximum allowed length of a single log line (64 KiB).
	MaxLineLength = 65536
)

// BlockMeta stores summary metadata for a single compressed block written to disk.
// This is emitted on every block flush and consumed by Task 3.6 to write companion .idx records.
type BlockMeta struct {
	SourceID        int64
	Date            string // "YYYY-MM-DD" (UTC)
	MinTS           int64
	MaxTS           int64
	MinLevel        model.Level
	MaxLevel        model.Level
	StreamMask      uint8  // bit 0: StreamNA, bit 1: StreamStdout, bit 2: StreamStderr
	FileOffset      int64  // Starting byte offset inside <date>.chunk
	CompressedLen   uint32 // Exact byte length of the compressed Zstd frame written to disk
	UncompressedLen uint32
	RecordCount     uint32
}

// EncodeBlockHeader writes the 16-byte uncompressed block header into dst[:16].
// Layout:
//   [0:4]   Magic ("TRLB")
//   [4]     Version (0x01)
//   [5]     Flags (0x00)
//   [6:8]   Reserved (0x0000)
//   [8:12]  RecordCount (uint32, little-endian)
//   [12:16] UncompressedLen (uint32, little-endian)
func EncodeBlockHeader(dst []byte, recordCount, uncompressedLen uint32) {
	_ = dst[15] // bounds check elimination
	copy(dst[0:4], BlockMagic[:])
	dst[4] = BlockVersion
	dst[5] = 0x00
	dst[6] = 0x00
	dst[7] = 0x00
	binary.LittleEndian.PutUint32(dst[8:12], recordCount)
	binary.LittleEndian.PutUint32(dst[12:16], uncompressedLen)
}

// DecodeBlockHeader decodes and validates the 16-byte uncompressed block header from src.
func DecodeBlockHeader(src []byte) (recordCount, uncompressedLen uint32, err error) {
	if len(src) < BlockHeaderSize {
		return 0, 0, ErrCorruptBlock
	}
	if src[0] != BlockMagic[0] || src[1] != BlockMagic[1] || src[2] != BlockMagic[2] || src[3] != BlockMagic[3] {
		return 0, 0, ErrInvalidMagic
	}
	if src[4] != BlockVersion {
		return 0, 0, fmt.Errorf("%w: got %d, expected %d", ErrUnsupportedVersion, src[4], BlockVersion)
	}
	recordCount = binary.LittleEndian.Uint32(src[8:12])
	uncompressedLen = binary.LittleEndian.Uint32(src[12:16])
	return recordCount, uncompressedLen, nil
}

// EncodeRecord appends a single log entry into dst, returning the modified slice.
// Layout:
//   [0:8]   TS (int64, little-endian, UTC nanoseconds)
//   [8]     Level (uint8, 0..5)
//   [9]     LevelSrc (uint8, 0..5)
//   [10]    TSSrc (uint8, 0..3)
//   [11]    Stream (uint8, 0..2)
//   [12:16] LineLen (uint32, little-endian)
//   [16:..] LineBytes (LineLen bytes)
func EncodeRecord(dst []byte, entry model.LogEntry) []byte {
	lineBytes := entry.Message
	if len(lineBytes) == 0 && len(entry.Raw) > 0 {
		lineBytes = entry.Raw
	}
	if len(lineBytes) > MaxLineLength {
		lineBytes = lineBytes[:MaxLineLength]
	}

	lineLen := uint32(len(lineBytes))
	start := len(dst)
	totalNeeded := start + RecordHeaderSize + len(lineBytes)

	// Ensure capacity
	if cap(dst) >= totalNeeded {
		dst = dst[:totalNeeded]
	} else {
		newDst := make([]byte, totalNeeded, totalNeeded+4096)
		copy(newDst, dst)
		dst = newDst
	}

	hdr := dst[start : start+RecordHeaderSize]
	binary.LittleEndian.PutUint64(hdr[0:8], uint64(entry.Timestamp.UnixNano()))
	hdr[8] = uint8(entry.Level)
	hdr[9] = uint8(entry.LevelSource)
	hdr[10] = uint8(entry.TimestampSource)
	hdr[11] = uint8(entry.Stream)
	binary.LittleEndian.PutUint32(hdr[12:16], lineLen)

	copy(dst[start+RecordHeaderSize:], lineBytes)
	return dst
}

// DecodeRecord decodes a single log record from src at byte offset 0.
// Returns the parsed LogEntry, number of bytes consumed, and any error encountered.
func DecodeRecord(src []byte, sourceID string) (entry model.LogEntry, consumed int, err error) {
	if len(src) < RecordHeaderSize {
		return model.LogEntry{}, 0, ErrCorruptBlock
	}

	tsNano := int64(binary.LittleEndian.Uint64(src[0:8]))
	lvl := model.Level(src[8])
	lvlSrc := model.LevelSource(src[9])
	tsSrc := model.TimestampSource(src[10])
	stream := model.Stream(src[11])
	lineLen := int(binary.LittleEndian.Uint32(src[12:16]))

	totalSize := RecordHeaderSize + lineLen
	if len(src) < totalSize {
		return model.LogEntry{}, 0, ErrCorruptBlock
	}

	// Copy line bytes so caller's LogEntry is independent of temporary decode buffers
	lineBytes := make([]byte, lineLen)
	copy(lineBytes, src[RecordHeaderSize:totalSize])

	entry = model.LogEntry{
		Timestamp:       time.Unix(0, tsNano).UTC(),
		TimestampSource: tsSrc,
		SourceID:        sourceID,
		Level:           lvl,
		LevelSource:     lvlSrc,
		Stream:          stream,
		Message:         lineBytes,
		Raw:             lineBytes,
	}

	return entry, totalSize, nil
}

// DecodeBlock decodes all log entries from a complete uncompressed block payload.
func DecodeBlock(raw []byte, sourceID string) ([]model.LogEntry, error) {
	recordCount, uncompressedLen, err := DecodeBlockHeader(raw)
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) < uncompressedLen {
		return nil, ErrCorruptBlock
	}

	entries := make([]model.LogEntry, 0, recordCount)
	offset := BlockHeaderSize
	limit := int(uncompressedLen)

	for i := uint32(0); i < recordCount; i++ {
		if offset >= limit {
			return nil, ErrCorruptBlock
		}
		entry, consumed, decErr := DecodeRecord(raw[offset:limit], sourceID)
		if decErr != nil {
			return nil, decErr
		}
		entries = append(entries, entry)
		offset += consumed
	}

	return entries, nil
}
