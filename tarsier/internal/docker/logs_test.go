package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// Helper to construct an 8-byte Docker log frame header + payload.
func makeFrame(streamType byte, payload []byte) []byte {
	var header [8]byte
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header[:], payload...)
}

func TestDockerLogFollower_NonTTYMultiplexedFrames(t *testing.T) {
	// Prepare frames:
	// Frame 1: stdout, contains 2 full lines (one with inner JSON timestamp, one plain)
	// Frame 2: stderr, contains 1 error line
	// Frame 3: stdout, first half of a split line
	// Frame 4: stderr, an error line arriving in-between
	// Frame 5: stdout, second half of the split line
	var frameData bytes.Buffer

	// Line 1: stdout with inner JSON timestamp
	f1Line1 := "2026-11-13T11:20:00.000000000Z {\"time\":\"2026-11-13T11:19:59.000000000Z\",\"level\":\"info\",\"msg\":\"user login\"}\n"
	// Line 2: stdout without inner timestamp (inherits Docker prefix TS)
	f1Line2 := "2026-11-13T11:20:01.123456789Z [WARN] cache nearly full\n"
	frameData.Write(makeFrame(1, []byte(f1Line1+f1Line2)))

	// Frame 2: stderr
	f2Line := "2026-11-13T11:20:02.000000000Z [ERROR] redis connection dropped\n"
	frameData.Write(makeFrame(2, []byte(f2Line)))

	// Frame 3: stdout part 1 of split line
	f3Part1 := "2026-11-13T11:20:03.000000000Z starting batch processing with "
	frameData.Write(makeFrame(1, []byte(f3Part1)))

	// Frame 4: stderr arriving mid-line
	f4Stderr := "2026-11-13T11:20:03.500000000Z [WARN] slow disk io detected\n"
	frameData.Write(makeFrame(2, []byte(f4Stderr)))

	// Frame 5: stdout part 2 of split line
	f5Part2 := "1000 items completed successfully\n"
	frameData.Write(makeFrame(1, []byte(f5Part2)))

	detector := logs.NewSourceDetector(time.UTC, 4096)
	var entries []model.LogEntry

	err := DemuxStream(&frameData, false, DefaultMaxLineBytes, func(stream model.Stream, rawLine []byte) error {
		// Process rawLine like processLine does
		var dockerTS int64
		appLine := rawLine

		spaceIdx := bytes.IndexByte(rawLine, ' ')
		if spaceIdx > 15 && spaceIdx < 40 {
			tsPrefix := rawLine[:spaceIdx]
			parsed, err := time.Parse(time.RFC3339Nano, string(tsPrefix))
			if err == nil {
				dockerTS = parsed.UnixNano()
				appLine = rawLine[spaceIdx+1:]
			}
		}

		appLine = stripCR(appLine)
		nowNS := time.Now().UTC().UnixNano()

		tsRes := detector.DetectTimestamp(logs.DetectInput{
			Line:       appLine,
			ReadTimeNS: nowNS,
			DockerTSNS: dockerTS,
			IsDocker:   true,
		})
		lvlRes := detector.DetectLevel(appLine, tsRes.TS, false)

		msgCopy := make([]byte, len(appLine))
		copy(msgCopy, appLine)

		entries = append(entries, model.LogEntry{
			Timestamp:       time.Unix(0, tsRes.TS).UTC(),
			TimestampSource: tsRes.TSSrc,
			SourceID:        "test-container",
			Level:           lvlRes.Level,
			LevelSource:     lvlRes.LevelSrc,
			Stream:          stream,
			Message:         msgCopy,
			Raw:             msgCopy,
		})
		return nil
	})

	if err != nil {
		t.Fatalf("DemuxStream failed: %v", err)
	}

	// We expect 5 total lines:
	// 1: stdout - user login (inner TS = 11:19:59, ts_src = 1, lvl = info, lvl_src = 1)
	// 2: stdout - cache nearly full (docker TS = 11:20:01.123456789, ts_src = 3, lvl = warn, lvl_src = 2)
	// 3: stderr - redis connection dropped (docker TS = 11:20:02, ts_src = 3, lvl = error, lvl_src = 2)
	// 4: stderr - slow disk io detected (docker TS = 11:20:03.5, ts_src = 3, lvl = warn, lvl_src = 2)
	// 5: stdout - reassembled split line (docker TS = 11:20:03, ts_src = 3, lvl = info/unknown)
	if len(entries) != 5 {
		t.Fatalf("expected 5 log entries, got %d", len(entries))
	}

	// Assert Line 1:
	e1 := entries[0]
	if e1.Stream != model.StreamStdout {
		t.Errorf("e1: expected StreamStdout, got %v", e1.Stream)
	}
	if e1.TimestampSource != model.TimestampSourceLine { // 1 = line
		t.Errorf("e1: expected TimestampSourceLine, got %v", e1.TimestampSource)
	}
	if e1.Level != model.LevelInfo || e1.LevelSource != model.LevelSourceStructured {
		t.Errorf("e1: expected Info/Structured, got %v/%v", e1.Level, e1.LevelSource)
	}
	expectedE1TS := time.Date(2026, 11, 13, 11, 19, 59, 0, time.UTC)
	if !e1.Timestamp.Equal(expectedE1TS) {
		t.Errorf("e1: expected timestamp %v, got %v", expectedE1TS, e1.Timestamp)
	}

	// Assert Line 2:
	e2 := entries[1]
	if e2.Stream != model.StreamStdout {
		t.Errorf("e2: expected StreamStdout, got %v", e2.Stream)
	}
	if e2.TimestampSource != model.TimestampSourceDocker { // 3 = docker
		t.Errorf("e2: expected TimestampSourceDocker, got %v", e2.TimestampSource)
	}
	if e2.Level != model.LevelWarn || e2.LevelSource != model.LevelSourceDelimited {
		t.Errorf("e2: expected Warn/Delimited, got %v/%v", e2.Level, e2.LevelSource)
	}
	expectedE2TS := time.Date(2026, 11, 13, 11, 20, 1, 123456789, time.UTC)
	if !e2.Timestamp.Equal(expectedE2TS) {
		t.Errorf("e2: expected timestamp %v, got %v", expectedE2TS, e2.Timestamp)
	}
	if string(e2.Message) != "[WARN] cache nearly full" {
		t.Errorf("e2: expected message stripped of docker timestamp, got %q", string(e2.Message))
	}

	// Assert Line 3:
	e3 := entries[2]
	if e3.Stream != model.StreamStderr {
		t.Errorf("e3: expected StreamStderr, got %v", e3.Stream)
	}
	if e3.Level != model.LevelError || e3.LevelSource != model.LevelSourceDelimited {
		t.Errorf("e3: expected Error/Delimited, got %v/%v", e3.Level, e3.LevelSource)
	}

	// Assert Line 5 (Reassembled split line):
	e5 := entries[4]
	if e5.Stream != model.StreamStdout {
		t.Errorf("e5: expected StreamStdout, got %v", e5.Stream)
	}
	expectedMsg := "starting batch processing with 1000 items completed successfully"
	if string(e5.Message) != expectedMsg {
		t.Errorf("e5: expected split line reassembled to %q, got %q", expectedMsg, string(e5.Message))
	}
}

func TestDockerLogFollower_TTYRawStream(t *testing.T) {
	rawLogs := []byte("2026-11-13T12:00:00Z line one\n2026-11-13T12:00:01Z [ERROR] line two\n2026-11-13T12:00:02Z line three\n")

	var entries []string
	var streams []model.Stream

	err := DemuxStream(bytes.NewReader(rawLogs), true, DefaultMaxLineBytes, func(stream model.Stream, line []byte) error {
		streams = append(streams, stream)
		entries = append(entries, string(line))
		return nil
	})

	if err != nil {
		t.Fatalf("DemuxStream TTY failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	for i, s := range streams {
		if s != model.StreamStdout {
			t.Errorf("entry %d: expected StreamStdout for TTY, got %v", i, s)
		}
	}
	if entries[1] != "2026-11-13T12:00:01Z [ERROR] line two" {
		t.Errorf("entry 1 unexpected: %q", entries[1])
	}
}

func TestDockerLogFollower_OversizedLineCap(t *testing.T) {
	// Construct a 150 KiB line without newline
	oversizedLen := 150 * 1024
	longData := make([]byte, oversizedLen)
	for i := range longData {
		longData[i] = 'A'
	}

	frame := makeFrame(1, longData)

	var receivedChunks [][]byte
	err := DemuxStream(bytes.NewReader(frame), false, 65536, func(stream model.Stream, line []byte) error {
		chunkCopy := make([]byte, len(line))
		copy(chunkCopy, line)
		receivedChunks = append(receivedChunks, chunkCopy)
		return nil
	})

	if err != nil {
		t.Fatalf("DemuxStream failed on oversized line: %v", err)
	}

	// 150 KiB should split into:
	// Chunk 1: 64 KiB (65536 bytes)
	// Chunk 2: 64 KiB (65536 bytes)
	// Chunk 3: 22 KiB (22528 bytes)
	if len(receivedChunks) != 3 {
		t.Fatalf("expected 3 chunks for 150 KiB line, got %d", len(receivedChunks))
	}
	if len(receivedChunks[0]) != 65536 {
		t.Errorf("chunk 0: expected 65536 bytes, got %d", len(receivedChunks[0]))
	}
	if len(receivedChunks[1]) != 65536 {
		t.Errorf("chunk 1: expected 65536 bytes, got %d", len(receivedChunks[1]))
	}
	if len(receivedChunks[2]) != 22528 {
		t.Errorf("chunk 2: expected 22528 bytes, got %d", len(receivedChunks[2]))
	}
}

func TestDockerLogFollower_CursorPersistenceAndDeduplication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Ensure source exists in config.db
	sourceID, err := EnsureSource(ctx, store.Config, "container", "api-gateway")
	if err != nil {
		t.Fatalf("EnsureSource failed: %v", err)
	}

	// Mock server that records ?since= parameter and replays lines
	var lastSince atomic.Int64
	var connectionCount atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/containers/c1/logs", func(w http.ResponseWriter, r *http.Request) {
		connectionCount.Add(1)
		sinceStr := r.URL.Query().Get("since")
		var sVal int64
		if sinceStr != "" {
			_, _ = fmt.Sscanf(sinceStr, "%d", &sVal)
		}
		lastSince.Store(sVal)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)

		if sVal == 0 {
			// First connection: emit 5 lines with timestamps 10..14
			for i := 10; i < 15; i++ {
				line := fmt.Sprintf("2026-11-13T11:20:%02d.000000000Z message %d\n", i, i)
				_, _ = w.Write(makeFrame(1, []byte(line)))
			}
		} else {
			// Reconnection after restart: mock server replays lines 13 and 14 (overlap) + emits 15, 16, 17
			for i := 13; i <= 17; i++ {
				line := fmt.Sprintf("2026-11-13T11:20:%02d.000000000Z message %d\n", i, i)
				_, _ = w.Write(makeFrame(1, []byte(line)))
			}
		}
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	logChan := make(chan model.LogEntry, 100)
	mgr := NewLogManager(LogManagerConfig{
		Client:        client,
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		FlushInterval: 100 * time.Millisecond,
	})

	containerInfo := ContainerInfo{
		ID:   "c1",
		Name: "api-gateway",
	}

	// Start follower
	mgr.StartContainer(ctx, containerInfo)

	// Collect first 5 lines
	var collected []model.LogEntry
	deadline := time.After(2 * time.Second)
collectLoop1:
	for len(collected) < 5 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadline:
			t.Fatalf("timeout waiting for 5 lines, got %d", len(collected))
			break collectLoop1
		}
	}

	// Stop follower: this triggers final cursor flush to source_offset in config.db
	mgr.StopContainer("c1")

	// Verify offset was persisted in config.db
	lastTS, err := GetSourceOffset(ctx, store.Config, sourceID)
	if err != nil {
		t.Fatalf("GetSourceOffset failed: %v", err)
	}
	expectedTS := time.Date(2026, 11, 13, 11, 20, 14, 0, time.UTC).UnixNano()
	if lastTS != expectedTS {
		t.Fatalf("expected persisted lastTS=%d, got %d", expectedTS, lastTS)
	}

	// Restart follower: should pass ?since= (11:20:14 - 1s = 11:20:13) and deduplicate lines 13 & 14
	mgrRestart := NewLogManager(LogManagerConfig{
		Client:        client,
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		FlushInterval: 100 * time.Millisecond,
	})
	mgrRestart.StartContainer(ctx, containerInfo)

	// Collect new lines: should only receive lines 15, 16, 17 (lines 13 and 14 deduplicated!)
	var collectedAfterRestart []model.LogEntry
	deadlineRestart := time.After(2 * time.Second)
collectLoop2:
	for len(collectedAfterRestart) < 3 {
		select {
		case e := <-logChan:
			collectedAfterRestart = append(collectedAfterRestart, e)
		case <-deadlineRestart:
			t.Fatalf("timeout waiting for 3 lines after restart, got %d", len(collectedAfterRestart))
			break collectLoop2
		}
	}

	mgrRestart.StopContainer("c1")

	if len(collectedAfterRestart) != 3 {
		t.Fatalf("expected exactly 3 new lines after restart, got %d", len(collectedAfterRestart))
	}
	// Verify timestamps of the 3 new lines are 15, 16, 17
	for idx, expectedSec := range []int{15, 16, 17} {
		expTime := time.Date(2026, 11, 13, 11, 20, expectedSec, 0, time.UTC)
		if !collectedAfterRestart[idx].Timestamp.Equal(expTime) {
			t.Errorf("item %d: expected timestamp %v, got %v", idx, expTime, collectedAfterRestart[idx].Timestamp)
		}
	}
}

func TestDockerLogFollower_CleanEOFAppliesBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var connectionCount atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/c-eof/logs", func(w http.ResponseWriter, r *http.Request) {
		connectionCount.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		// Immediately return empty body (clean EOF).
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	logChan := make(chan model.LogEntry, 16)
	mgr := NewLogManager(LogManagerConfig{
		Client:        client,
		Sink:          model.LogChanSink(logChan),
		FlushInterval: 100 * time.Millisecond,
	})
	defer mgr.Stop()

	mgr.StartContainer(ctx, ContainerInfo{
		ID:   "c-eof",
		Name: "eof-container",
	})

	// Wait long enough that a busy loop would reconnect dozens/hundreds of times,
	// but shorter than the 1s initial backoff.
	time.Sleep(250 * time.Millisecond)
	mgr.StopContainer("c-eof")

	if got := connectionCount.Load(); got != 1 {
		t.Fatalf("expected 1 connection during 250ms window with backoff on clean EOF, got %d", got)
	}
}
