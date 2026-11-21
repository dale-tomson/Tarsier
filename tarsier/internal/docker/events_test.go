package docker

import (
	"context"
	"fmt"
	"net/http"
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
