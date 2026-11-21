package collector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestFileTailer_InodeRotationNoDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "app.log")

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Create app.log and write initial 5 lines
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	for i := 1; i <= 5; i++ {
		_, _ = fmt.Fprintf(f, "2026-11-13T10:00:%02dZ [INFO] initial line %d\n", i, i)
	}

	logChan := make(chan model.LogEntry, 100)
	mgr := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "app", Path: logPath},
		},
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		PollInterval:  50 * time.Millisecond,
		FlushInterval: 100 * time.Millisecond,
	})
	mgr.Start(ctx)
	defer mgr.Stop()

	// Collect first 5 lines
	var collected []model.LogEntry
	deadline := time.After(2 * time.Second)
	for len(collected) < 5 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadline:
			t.Fatalf("timed out waiting for 5 lines, got %d", len(collected))
		}
	}

	// 2. Simulate logrotate: rename app.log -> app.log.1
	rotatedPath := filepath.Join(tmpDir, "app.log.1")
	if err := os.Rename(logPath, rotatedPath); err != nil {
		t.Fatalf("rename failed: %v", err)
	}

	// Write 2 final lines to the old file descriptor (simulating application still writing to old file)
	for i := 6; i <= 7; i++ {
		_, _ = fmt.Fprintf(f, "2026-11-13T10:00:%02dZ [WARN] old file line %d\n", i, i)
	}
	_ = f.Close()

	// Create brand-new app.log (new inode) and write 5 new lines (8..12)
	newF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("create new app.log: %v", err)
	}
	defer newF.Close()

	for i := 8; i <= 12; i++ {
		_, _ = fmt.Fprintf(newF, "2026-11-13T10:00:%02dZ [INFO] new file line %d\n", i, i)
	}

	// Wait to collect all 12 total lines
	deadlineTotal := time.After(3 * time.Second)
	for len(collected) < 12 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadlineTotal:
			t.Fatalf("timed out waiting for 12 lines, got %d", len(collected))
		}
	}

	if len(collected) != 12 {
		t.Fatalf("expected exactly 12 lines, got %d", len(collected))
	}

	// Verify sequential line numbers 1..12 without duplicates
	for i, entry := range collected {
		expectedLineNum := i + 1
		msg := string(entry.Message)
		expectedSub := fmt.Sprintf("line %d", expectedLineNum)
		if !bytes.Contains(entry.Message, []byte(expectedSub)) {
			t.Errorf("entry %d: expected %q in message %q", i, expectedSub, msg)
		}
	}

	// Verify source_offset was updated to new file's inode and offset
	newInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat new app.log: %v", err)
	}
	expectedInode := getInode(newInfo)

	time.Sleep(200 * time.Millisecond) // allow flush
	inode, offset, _, err := GetFileSourceOffset(ctx, store.Config, 1)
	if err != nil {
		t.Fatalf("GetFileSourceOffset failed: %v", err)
	}
	if inode != expectedInode {
		t.Errorf("expected source_offset inode=%d, got %d", expectedInode, inode)
	}
	if offset != newInfo.Size() {
		t.Errorf("expected source_offset offset=%d, got %d", newInfo.Size(), offset)
	}
}

func TestFileTailer_CopyTruncate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "service.log")

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Write initial 4 lines
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("create service.log: %v", err)
	}
	defer f.Close()

	for i := 1; i <= 4; i++ {
		_, _ = fmt.Fprintf(f, "2026-11-13T10:00:%02dZ line %d\n", i, i)
	}

	logChan := make(chan model.LogEntry, 100)
	mgr := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "service", Path: logPath},
		},
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		PollInterval:  50 * time.Millisecond,
		FlushInterval: 100 * time.Millisecond,
	})
	mgr.Start(ctx)
	defer mgr.Stop()

	// Collect first 4 lines
	var collected []model.LogEntry
	deadline := time.After(2 * time.Second)
	for len(collected) < 4 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadline:
			t.Fatalf("timed out waiting for 4 lines, got %d", len(collected))
		}
	}

	// 2. Perform copytruncate: truncate file to 0 bytes
	if err := os.Truncate(logPath, 0); err != nil {
		t.Fatalf("truncate failed: %v", err)
	}
	_, _ = f.Seek(0, io.SeekStart)

	// Write 3 new lines (5..7)
	for i := 5; i <= 7; i++ {
		_, _ = fmt.Fprintf(f, "2026-11-13T10:00:%02dZ line %d\n", i, i)
	}

	// Wait for all 7 lines
	deadlineTotal := time.After(3 * time.Second)
	for len(collected) < 7 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadlineTotal:
			t.Fatalf("timed out waiting for 7 lines, got %d", len(collected))
		}
	}

	if len(collected) != 7 {
		t.Fatalf("expected 7 lines, got %d", len(collected))
	}
	for i, entry := range collected {
		expectedLineNum := i + 1
		expectedSub := fmt.Sprintf("line %d", expectedLineNum)
		if !bytes.Contains(entry.Message, []byte(expectedSub)) {
			t.Errorf("entry %d: expected %q in message %q", i, expectedSub, string(entry.Message))
		}
	}
}

func TestFileTailer_GlobDiscoveryAndRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	globPattern := filepath.Join(tmpDir, "*.log")

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Create a.log
	aPath := filepath.Join(tmpDir, "a.log")
	_ = os.WriteFile(aPath, []byte("2026-11-13T10:00:01Z a log line 1\n"), 0644)

	logChan := make(chan model.LogEntry, 100)
	mgr := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "app", Path: globPattern},
		},
		ConfigDB:       store.Config,
		Sink:           model.LogChanSink(logChan),
		PollInterval:   50 * time.Millisecond,
		RescanInterval: 100 * time.Millisecond,
	})
	mgr.Start(ctx)
	defer mgr.Stop()

	// Receive from a.log
	select {
	case e := <-logChan:
		if !bytes.Contains(e.Message, []byte("a log line 1")) {
			t.Fatalf("unexpected message: %s", string(e.Message))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for a.log")
	}

	// 2. Dynamically create b.log and trigger Rescan
	bPath := filepath.Join(tmpDir, "b.log")
	_ = os.WriteFile(bPath, []byte("2026-11-13T10:00:02Z b log line 1\n"), 0644)
	mgr.Rescan()

	select {
	case e := <-logChan:
		if !bytes.Contains(e.Message, []byte("b log line 1")) {
			t.Fatalf("unexpected message: %s", string(e.Message))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for b.log")
	}

	// 3. Remove a.log
	_ = os.Remove(aPath)
	mgr.Rescan()

	mgr.mu.RLock()
	_, aExists := mgr.tailers[aPath]
	_, bExists := mgr.tailers[bPath]
	mgr.mu.RUnlock()

	if aExists {
		t.Errorf("expected a.log tailer to be removed")
	}
	if !bExists {
		t.Errorf("expected b.log tailer to remain active")
	}
}

func TestFileTailer_RestartResumeAndPartialLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "resume.log")

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Write 3 complete lines + 1 partial line (without '\n')
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}

	_, _ = fmt.Fprint(f, "2026-11-13T10:00:01Z line 1\n")
	_, _ = fmt.Fprint(f, "2026-11-13T10:00:02Z line 2\n")
	_, _ = fmt.Fprint(f, "2026-11-13T10:00:03Z line 3\n")
	_, _ = fmt.Fprint(f, "2026-11-13T10:00:04Z partial line 4") // no \n yet!

	logChan := make(chan model.LogEntry, 100)
	mgr := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "resume", Path: logPath},
		},
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		PollInterval:  50 * time.Millisecond,
		FlushInterval: 50 * time.Millisecond,
	})
	mgr.Start(ctx)

	// Collect first 3 lines
	var collected []model.LogEntry
	deadline := time.After(2 * time.Second)
	for len(collected) < 3 {
		select {
		case e := <-logChan:
			collected = append(collected, e)
		case <-deadline:
			t.Fatalf("timed out waiting for 3 lines, got %d", len(collected))
		}
	}

	// Stop manager (flushes offset to config.db)
	mgr.Stop()

	// Partial line should NOT have advanced offset
	// Complete line 4 by writing remaining text + '\n', plus 2 new lines (5 and 6)
	_, _ = fmt.Fprint(f, " completed\n")
	_, _ = fmt.Fprint(f, "2026-11-13T10:00:05Z line 5\n")
	_, _ = fmt.Fprint(f, "2026-11-13T10:00:06Z line 6\n")
	_ = f.Close()

	// 2. Start brand-new manager against the same config.db
	mgr2 := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "resume", Path: logPath},
		},
		ConfigDB:      store.Config,
		Sink:          model.LogChanSink(logChan),
		PollInterval:  50 * time.Millisecond,
		FlushInterval: 50 * time.Millisecond,
	})
	mgr2.Start(ctx)
	defer mgr2.Stop()

	// Should receive lines 4, 5, 6 with ZERO duplicate replay of lines 1..3
	var collected2 []model.LogEntry
	deadline2 := time.After(2 * time.Second)
	for len(collected2) < 3 {
		select {
		case e := <-logChan:
			collected2 = append(collected2, e)
		case <-deadline2:
			t.Fatalf("timed out waiting for 3 lines on restart, got %d", len(collected2))
		}
	}

	if len(collected2) != 3 {
		t.Fatalf("expected 3 lines on restart, got %d", len(collected2))
	}
	expectedLine4 := "2026-11-13T10:00:04Z partial line 4 completed"
	if string(collected2[0].Message) != expectedLine4 {
		t.Errorf("line 4: expected %q, got %q", expectedLine4, string(collected2[0].Message))
	}
	if !bytes.Contains(collected2[1].Message, []byte("line 5")) {
		t.Errorf("line 5 unexpected: %s", string(collected2[1].Message))
	}
	if !bytes.Contains(collected2[2].Message, []byte("line 6")) {
		t.Errorf("line 6 unexpected: %s", string(collected2[2].Message))
	}
}

func TestFileTailer_OversizedLineCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "oversized.log")

	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 1. Create empty file first
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to create log: %v", err)
	}
	f.Close()

	logChan := make(chan model.LogEntry, 100)
	mgr := NewFileTailerManager(FileTailerConfig{
		Files: []config.LogFileConfig{
			{Name: "oversized", Path: logPath},
		},
		ConfigDB:     store.Config,
		Sink:         model.LogChanSink(logChan),
		PollInterval: 50 * time.Millisecond,
		MaxLineBytes: 65536,
	})
	mgr.Start(ctx)
	defer mgr.Stop()

	// Wait briefly for manager to discover and open the empty file at offset 0
	time.Sleep(100 * time.Millisecond)

	// 2. Write 140 KiB line without '\n', then '\n'
	oversizedBytes := make([]byte, 140*1024)
	for i := range oversizedBytes {
		oversizedBytes[i] = 'X'
	}
	oversizedBytes = append(oversizedBytes, '\n')

	if err := os.WriteFile(logPath, oversizedBytes, 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// 140 KiB splits into:
	// Chunk 1: 64 KiB (65536)
	// Chunk 2: 64 KiB (65536)
	// Chunk 3: 12 KiB (12288)
	var chunks []model.LogEntry
	deadline := time.After(2 * time.Second)
	for len(chunks) < 3 {
		select {
		case e := <-logChan:
			chunks = append(chunks, e)
		case <-deadline:
			t.Fatalf("timed out waiting for 3 chunks, got %d", len(chunks))
		}
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if len(chunks[0].Message) != 65536 {
		t.Errorf("chunk 0: expected 65536, got %d", len(chunks[0].Message))
	}
	if len(chunks[1].Message) != 65536 {
		t.Errorf("chunk 1: expected 65536, got %d", len(chunks[1].Message))
	}
	if len(chunks[2].Message) != 12288 {
		t.Errorf("chunk 2: expected 12288, got %d", len(chunks[2].Message))
	}
}
