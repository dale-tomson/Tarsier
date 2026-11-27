package docker

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func TestEventWatcher_StreamsAndPersistsLifecycleEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Filter: only allow containers starting with "api-" or "worker"
	filter, err := NewContainerFilter([]string{"api-*", "worker*"}, nil)
	if err != nil {
		t.Fatalf("NewContainerFilter failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		frames := []string{
			// 1. Valid tracked lifecycle event: start
			`{"Type":"container","Action":"start","time":1700000001,"Actor":{"ID":"c1","Attributes":{"name":"api-auth","image":"auth:1.0"}}}`,
			// 2. Ignored noisy event: exec_start
			`{"Type":"container","Action":"exec_start: /bin/sh","time":1700000002,"Actor":{"ID":"c1","Attributes":{"name":"api-auth","image":"auth:1.0"}}}`,
			// 3. Ignored event: excluded container name (not in filter)
			`{"Type":"container","Action":"start","time":1700000003,"Actor":{"ID":"c_excluded","Attributes":{"name":"excluded-service","image":"nginx"}}}`,
			// 4. Valid tracked event: oom
			`{"Type":"container","Action":"oom","time":1700000004,"Actor":{"ID":"c1","Attributes":{"name":"api-auth","image":"auth:1.0"}}}`,
			// 5. Valid tracked event: die (exitCode 137, oomKilled)
			`{"Type":"container","Action":"die","time":1700000005,"Actor":{"ID":"c1","Attributes":{"name":"api-auth","image":"auth:1.0","exitCode":"137","oomKilled":"true"}}}`,
			// 6. Valid tracked event: restart
			`{"Type":"container","Action":"restart","time":1700000006,"Actor":{"ID":"c1","Attributes":{"name":"api-auth","image":"auth:1.0"}}}`,
			// 7. Valid tracked event: stop for worker
			`{"Type":"container","Action":"stop","time":1700000007,"Actor":{"ID":"c2","Attributes":{"name":"worker-queue","image":"worker:latest"}}}`,
		}

		for _, f := range frames {
			select {
			case <-r.Context().Done():
				return
			default:
				_, _ = fmt.Fprintf(w, "%s\n", f)
				flusher.Flush()
				time.Sleep(15 * time.Millisecond)
			}
		}

		// Keep connection open until context cancelled
		<-r.Context().Done()
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	var mu sync.Mutex
	receivedEvents := make([]ContainerEvent, 0)
	watcherOpts := EventWatcherOptions{
		InitialCursorTS: 1700000000,
		OnEvent: func(ev ContainerEvent) {
			mu.Lock()
			receivedEvents = append(receivedEvents, ev)
			mu.Unlock()
		},
	}

	watcher := NewEventWatcher(client, filter, store.Config, watcherOpts)

	runCtx, runCancel := context.WithCancel(ctx)
	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Run(runCtx)
	}()

	// Wait for 5 expected events (c1: start, oom, die, restart; c2: stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(receivedEvents)
		mu.Unlock()
		if count >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	runCancel()
	<-errChan

	mu.Lock()
	finalCount := len(receivedEvents)
	mu.Unlock()

	if finalCount != 5 {
		t.Fatalf("expected 5 lifecycle events, got %d: %+v", finalCount, receivedEvents)
	}

	// Verify excluded container was NOT persisted to container table
	var excludedCount int
	err = store.Config.RO.QueryRowContext(ctx, "SELECT count(*) FROM container WHERE id = 'c_excluded'").Scan(&excludedCount)
	if err != nil {
		t.Fatalf("query excluded: %v", err)
	}
	if excludedCount != 0 {
		t.Errorf("expected excluded container count 0, got %d", excludedCount)
	}

	// Verify events in container_event table
	var eventCount int
	err = store.Config.RO.QueryRowContext(ctx, "SELECT count(*) FROM container_event").Scan(&eventCount)
	if err != nil {
		t.Fatalf("query container_event count: %v", err)
	}
	if eventCount != 5 {
		t.Errorf("expected 5 rows in container_event, got %d", eventCount)
	}

	// Verify die event detail
	var dieDetail string
	err = store.Config.RO.QueryRowContext(ctx, "SELECT detail FROM container_event WHERE type = 'die' AND container_id = 'c1'").Scan(&dieDetail)
	if err != nil {
		t.Fatalf("query die detail: %v", err)
	}
	if dieDetail != "exit_code=137 oom=true" {
		t.Errorf("expected 'exit_code=137 oom=true', got %q", dieDetail)
	}

	// Verify container table updated state for c1 (last action was restart -> status = 'running', restart_count = 1)
	var c1Status string
	var c1Restarts int
	err = store.Config.RO.QueryRowContext(ctx, "SELECT last_status, restart_count FROM container WHERE id = 'c1'").Scan(&c1Status, &c1Restarts)
	if err != nil {
		t.Fatalf("query c1 state: %v", err)
	}
	if c1Status != "running" {
		t.Errorf("expected c1 status 'running', got %q", c1Status)
	}
	if c1Restarts != 1 {
		t.Errorf("expected c1 restarts 1, got %d", c1Restarts)
	}

	// Verify c2 state (stop -> status = 'exited')
	var c2Status string
	err = store.Config.RO.QueryRowContext(ctx, "SELECT last_status FROM container WHERE id = 'c2'").Scan(&c2Status)
	if err != nil {
		t.Fatalf("query c2 state: %v", err)
	}
	if c2Status != "exited" {
		t.Errorf("expected c2 status 'exited', got %q", c2Status)
	}
}

func TestEventWatcher_ReconnectsWithSinceCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	var connectionAttempts atomic.Int32
	var sinceParam atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		attempt := connectionAttempts.Add(1)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		if attempt == 1 {
			// First attempt sends 2 events then abruptly closes the connection
			_, _ = fmt.Fprintf(w, `{"Type":"container","Action":"start","time":1700000010,"Actor":{"ID":"c1","Attributes":{"name":"svc-1","image":"alpine"}}}`+"\n")
			flusher.Flush()
			_, _ = fmt.Fprintf(w, `{"Type":"container","Action":"stop","time":1700000020,"Actor":{"ID":"c1","Attributes":{"name":"svc-1","image":"alpine"}}}`+"\n")
			flusher.Flush()
			// Close stream
			return
		}

		// Second attempt: record 'since' query parameter
		var sinceVal int64
		_, _ = fmt.Sscanf(r.URL.Query().Get("since"), "%d", &sinceVal)
		sinceParam.Store(sinceVal)

		// Send 3rd event
		_, _ = fmt.Fprintf(w, `{"Type":"container","Action":"start","time":1700000030,"Actor":{"ID":"c1","Attributes":{"name":"svc-1","image":"alpine"}}}`+"\n")
		flusher.Flush()

		<-r.Context().Done()
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	eventsReceived := atomic.Int32{}
	watcher := NewEventWatcher(client, nil, store.Config, EventWatcherOptions{
		InitialCursorTS: 1700000000,
		OnEvent: func(ev ContainerEvent) {
			eventsReceived.Add(1)
		},
	})

	runCtx, runCancel := context.WithCancel(ctx)
	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Run(runCtx)
	}()

	// Wait until at least 3 events received across reconnections
	deadline := time.Now().Add(6 * time.Second)
	for {
		if eventsReceived.Load() >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	runCancel()
	<-errChan

	if attempts := connectionAttempts.Load(); attempts < 2 {
		t.Fatalf("expected at least 2 connection attempts, got %d", attempts)
	}

	// Verify reconnect cursor used the latest event timestamp (1700000020)
	if s := sinceParam.Load(); s != 1700000020 {
		t.Errorf("expected reconnect with since=1700000020, got %d", s)
	}

	// Verify all 3 events are stored in DB
	var count int
	if err := store.Config.RO.QueryRowContext(ctx, "SELECT count(*) FROM container_event").Scan(&count); err != nil {
		t.Fatalf("query container_event count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 container events, got %d", count)
	}
}

func TestEventWatcher_CleanShutdownOnContextCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
		}
		// Idle stream
		<-r.Context().Done()
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	watcher := NewEventWatcher(client, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	start := time.Now()
	go func() {
		errChan <- watcher.Run(ctx)
	}()

	// Wait briefly for watcher to connect
	time.Sleep(50 * time.Millisecond)

	// Cancel context while blocked on idle stream
	cancel()

	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("expected clean nil error on shutdown, got: %v", err)
		}
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			t.Errorf("shutdown took too long: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher failed to stop within 2 seconds")
	}
}

func TestEventWatcher_CrashLoopDieToStartYieldsRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Track inspect responses per phase for container "c-loop":
	// phase 0: initial start (running, RestartCount 0)
	// phase 1: after die #1 (restarting, RestartCount 0)
	// phase 2: after start #1 (running, RestartCount 1)
	// phase 3: after die #2 (restarting, RestartCount 1)
	// phase 4: after start #2 (running, RestartCount 2)
	var inspectPhase atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/containers/c-loop/json", func(w http.ResponseWriter, r *http.Request) {
		phase := inspectPhase.Load()
		var status string
		var restarts int
		switch phase {
		case 0:
			status, restarts = "running", 0
		case 1:
			status, restarts = "restarting", 0
		case 2:
			status, restarts = "running", 1
		case 3:
			status, restarts = "restarting", 1
		default:
			status, restarts = "running", 2
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"Id":"c-loop","Name":"/crashy","Image":"app:v1","RestartCount":%d,"State":{"Status":%q}}`, restarts, status)
	})

	// Container "c-fallback" has no inspect endpoint (404), testing DB-state fallback.
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		send := func(phase int32, frame string) {
			if phase >= 0 {
				inspectPhase.Store(phase)
			}
			_, _ = fmt.Fprintf(w, "%s\n", frame)
			flusher.Flush()
			time.Sleep(25 * time.Millisecond)
		}

		// c-loop crash loop
		send(0, `{"Type":"container","Action":"start","time":1700000100,"Actor":{"ID":"c-loop","Attributes":{"name":"crashy","image":"app:v1"}}}`)
		send(1, `{"Type":"container","Action":"die","time":1700000101,"Actor":{"ID":"c-loop","Attributes":{"name":"crashy","image":"app:v1","exitCode":"1"}}}`)
		send(2, `{"Type":"container","Action":"start","time":1700000102,"Actor":{"ID":"c-loop","Attributes":{"name":"crashy","image":"app:v1"}}}`)
		send(3, `{"Type":"container","Action":"die","time":1700000103,"Actor":{"ID":"c-loop","Attributes":{"name":"crashy","image":"app:v1","exitCode":"1"}}}`)
		send(4, `{"Type":"container","Action":"start","time":1700000104,"Actor":{"ID":"c-loop","Attributes":{"name":"crashy","image":"app:v1"}}}`)

		// c-fallback crash loop without inspect + kill event
		send(-1, `{"Type":"container","Action":"start","time":1700000105,"Actor":{"ID":"c-fallback","Attributes":{"name":"fallback-svc","image":"busybox"}}}`)
		send(-1, `{"Type":"container","Action":"kill","time":1700000106,"Actor":{"ID":"c-fallback","Attributes":{"name":"fallback-svc","image":"busybox","signal":"9"}}}`)
		send(-1, `{"Type":"container","Action":"die","time":1700000107,"Actor":{"ID":"c-fallback","Attributes":{"name":"fallback-svc","image":"busybox","exitCode":"137"}}}`)
		send(-1, `{"Type":"container","Action":"start","time":1700000108,"Actor":{"ID":"c-fallback","Attributes":{"name":"fallback-svc","image":"busybox"}}}`)

		<-r.Context().Done()
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	var mu sync.Mutex
	var received []ContainerEvent
	watcher := NewEventWatcher(client, nil, store.Config, EventWatcherOptions{
		InitialCursorTS: 1700000099,
		OnEvent: func(ev ContainerEvent) {
			mu.Lock()
			received = append(received, ev)
			mu.Unlock()
		},
	})

	runCtx, runCancel := context.WithCancel(ctx)
	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Run(runCtx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 9 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	runCancel()
	<-errChan

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 9 {
		t.Fatalf("expected 9 events, got %d: %+v", len(received), received)
	}

	// Verify c-loop dispatched actions: start, die, restart, die, restart
	expectedLoopActions := []string{"start", "die", "restart", "die", "restart"}
	for i, exp := range expectedLoopActions {
		if received[i].Action != exp {
			t.Errorf("c-loop event %d: expected action %q, got %q", i, exp, received[i].Action)
		}
	}
	if received[2].Detail != "restarted (count=1)" {
		t.Errorf("expected detail 'restarted (count=1)', got %q", received[2].Detail)
	}
	if received[4].Detail != "restarted (count=2)" {
		t.Errorf("expected detail 'restarted (count=2)', got %q", received[4].Detail)
	}

	// Verify c-fallback dispatched actions: start, kill, die, restart
	expectedFallbackActions := []string{"start", "kill", "die", "restart"}
	for i, exp := range expectedFallbackActions {
		got := received[5+i]
		if got.Action != exp {
			t.Errorf("c-fallback event %d: expected action %q, got %q", i, exp, got.Action)
		}
	}
	if received[6].Detail != "signal=9" {
		t.Errorf("expected kill detail 'signal=9', got %q", received[6].Detail)
	}
	if received[8].Detail != "restarted (count=1)" {
		t.Errorf("expected fallback restart detail 'restarted (count=1)', got %q", received[8].Detail)
	}

	// Verify DB restart_count and last_status for both containers
	var loopStatus string
	var loopRestarts int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status, restart_count FROM container WHERE id = 'c-loop'`).Scan(&loopStatus, &loopRestarts); err != nil {
		t.Fatalf("query c-loop: %v", err)
	}
	if loopStatus != "running" || loopRestarts != 2 {
		t.Errorf("expected c-loop status='running', restart_count=2; got status=%q, restart_count=%d", loopStatus, loopRestarts)
	}

	var fbStatus string
	var fbRestarts int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status, restart_count FROM container WHERE id = 'c-fallback'`).Scan(&fbStatus, &fbRestarts); err != nil {
		t.Fatalf("query c-fallback: %v", err)
	}
	if fbStatus != "running" || fbRestarts != 1 {
		t.Errorf("expected c-fallback status='running', restart_count=1; got status=%q, restart_count=%d", fbStatus, fbRestarts)
	}

	// Verify container_event table recorded normalized restart rows
	var restartEventCount int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT count(*) FROM container_event WHERE type = 'restart'`).Scan(&restartEventCount); err != nil {
		t.Fatalf("query restart events: %v", err)
	}
	if restartEventCount != 3 {
		t.Errorf("expected 3 restart events in container_event, got %d", restartEventCount)
	}
}

func TestEventWatcher_ResolveInitialCursor(t *testing.T) {
	ctx := context.Background()

	// 1. Nil DB falls back to now - lookback (default 24h)
	before := time.Now().Add(-24 * time.Hour).Unix()
	curNil := ResolveInitialCursor(ctx, nil, 0)
	after := time.Now().Add(-24 * time.Hour).Unix()
	if curNil < before-2 || curNil > after+2 {
		t.Errorf("expected nil DB cursor around %d, got %d", before, curNil)
	}

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// 2. Empty container_event table falls back to now - lookback
	lookback := 6 * time.Hour
	expectedMin := time.Now().Add(-lookback).Unix() - 2
	expectedMax := time.Now().Add(-lookback).Unix() + 2
	curEmpty := ResolveInitialCursor(ctx, store.Config, lookback)
	if curEmpty < expectedMin || curEmpty > expectedMax {
		t.Errorf("expected empty DB cursor in [%d, %d], got %d", expectedMin, expectedMax, curEmpty)
	}

	// 3. Populated container_event table returns MAX(ts)
	if err := store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container_event (container_id, ts, type, detail)
			VALUES ('c1', 1700000100, 'start', ''), ('c1', 1700000500, 'die', 'exit_code=1');
		`)
		return err
	}); err != nil {
		t.Fatalf("seed container_event: %v", err)
	}

	curPopulated := ResolveInitialCursor(ctx, store.Config, lookback)
	if curPopulated != 1700000500 {
		t.Errorf("expected MAX(ts) 1700000500, got %d", curPopulated)
	}
}

func TestEventWatcher_LargeJSONFrameStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Construct a >90 KiB JSON frame exceeding bufio.Scanner's old 64 KiB cap
	hugeLabel := strings.Repeat("x", 96*1024)
	largeFrame := fmt.Sprintf(
		`{"Type":"container","Action":"start","time":1700001000,"Actor":{"ID":"c-huge","Attributes":{"name":"huge-labels","image":"nginx:latest","big_label":%q}}}`,
		hugeLabel,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		_, _ = fmt.Fprintf(w, "%s\n", largeFrame)
		flusher.Flush()

		<-r.Context().Done()
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	receivedCh := make(chan ContainerEvent, 4)
	watcher := NewEventWatcher(client, nil, store.Config, EventWatcherOptions{
		InitialCursorTS: 1700000999,
		OnEvent: func(ev ContainerEvent) {
			receivedCh <- ev
		},
	})

	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(runCtx)
	}()

	select {
	case ev := <-receivedCh:
		if ev.ContainerID != "c-huge" || ev.ContainerName != "huge-labels" || ev.Action != "start" {
			t.Errorf("unexpected decoded event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		runCancel()
		<-errCh
		t.Fatal("timed out waiting for large (>64 KiB) JSON event frame")
	}

	runCancel()
	<-errCh

	var count int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT count(*) FROM container_event WHERE container_id = 'c-huge'`).Scan(&count); err != nil {
		t.Fatalf("query container_event: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 persisted event for large frame, got %d", count)
	}
}

func TestEventWatcher_ProcessFrameNormalizesLeadingSlashes(t *testing.T) {
	watcher := NewEventWatcher(nil, nil, nil)
	frame := dockerEventFrame{
		Type:   "container",
		Action: "start",
		Time:   1700001000,
	}
	frame.Actor.ID = "cid-ev-1"
	frame.Actor.Attributes = map[string]string{
		"name":  " /quilltab ",
		"image": " /quillstone/quilltab:684d2b2e ",
	}

	ev, ok := watcher.processFrame(frame)
	if !ok {
		t.Fatalf("expected processFrame to accept start event")
	}
	if ev.ContainerName != "quilltab" {
		t.Errorf("expected normalized ContainerName 'quilltab', got %q", ev.ContainerName)
	}
	if ev.Image != "quillstone/quilltab:684d2b2e" {
		t.Errorf("expected normalized Image 'quillstone/quilltab:684d2b2e', got %q", ev.Image)
	}
}
