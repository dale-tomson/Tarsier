package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func newMockDockerServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("td-%d.sock", time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on mock unix socket (%s): %v", socketPath, err)
	}

	server := &http.Server{
		Handler: handler,
	}

	go func() {
		_ = server.Serve(listener)
	}()

	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	return socketPath
}

func TestClient_PingAndListContainers(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})

	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		all := r.URL.Query().Get("all")
		if all != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		payload := `[
			{
				"Id": "c1a2b3d4e5f6",
				"Names": ["/tarsier"],
				"Image": "ghcr.io/dale-tomson/tarsier:latest",
				"State": "running",
				"Status": "Up 2 hours",
				"Created": 1700000000
			},
			{
				"Id": "a9b8c7d6e5f4",
				"Names": ["/api-worker"],
				"Image": "redis:7-alpine",
				"State": "exited",
				"Status": "Exited (0) 10 minutes ago",
				"Created": 1699990000
			}
		]`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test Ping
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	// Test ListContainers(all = true)
	containers, err := client.ListContainers(ctx, true)
	if err != nil {
		t.Fatalf("ListContainers failed: %v", err)
	}

	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}

	// Verify normalization (leading slashes removed)
	if containers[0].Name != "tarsier" {
		t.Errorf("expected container name 'tarsier', got %q", containers[0].Name)
	}
	if containers[0].State != "running" {
		t.Errorf("expected state 'running', got %q", containers[0].State)
	}
	if containers[0].Image != "ghcr.io/dale-tomson/tarsier:latest" {
		t.Errorf("expected image 'ghcr.io/dale-tomson/tarsier:latest', got %q", containers[0].Image)
	}

	if containers[1].Name != "api-worker" {
		t.Errorf("expected container name 'api-worker', got %q", containers[1].Name)
	}
	if containers[1].State != "exited" {
		t.Errorf("expected state 'exited', got %q", containers[1].State)
	}
}

func TestClient_InspectContainer(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/containers/abc123/json", func(w http.ResponseWriter, r *http.Request) {
		payload := `{
			"Id": "abc123456789",
			"Name": "/my-service",
			"Image": "postgres:16",
			"RestartCount": 3,
			"State": {
				"Status": "running",
				"Running": true,
				"OOMKilled": true,
				"ExitCode": 137,
				"StartedAt": "2026-11-15T08:00:00Z"
			},
			"Config": {
				"Tty": false
			},
			"HostConfig": {
				"Memory": 268435456,
				"NanoCpus": 2000000000
			}
		}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	})

	mux.HandleFunc("/containers/unknown/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: unknown"}`))
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Inspect existing container
	inspect, err := client.InspectContainer(ctx, "abc123")
	if err != nil {
		t.Fatalf("InspectContainer failed: %v", err)
	}

	if inspect.ID != "abc123456789" {
		t.Errorf("expected ID 'abc123456789', got %q", inspect.ID)
	}
	if inspect.Name != "my-service" {
		t.Errorf("expected normalized Name 'my-service', got %q", inspect.Name)
	}
	if inspect.RestartCount != 3 {
		t.Errorf("expected RestartCount 3, got %d", inspect.RestartCount)
	}
	if !inspect.State.OOMKilled {
		t.Errorf("expected State.OOMKilled to be true")
	}
	if inspect.HostConfig.Memory != 268435456 {
		t.Errorf("expected Memory 268435456, got %d", inspect.HostConfig.Memory)
	}
	if inspect.HostConfig.NanoCPUs != 2000000000 {
		t.Errorf("expected NanoCpus 2000000000, got %d", inspect.HostConfig.NanoCPUs)
	}

	// Inspect non-existing container -> ErrContainerNotFound
	_, err = client.InspectContainer(ctx, "unknown")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("expected ErrContainerNotFound, got %v", err)
	}
}

func TestClient_SocketMissingGracefulError(t *testing.T) {
	nonExistentSocket := filepath.Join(t.TempDir(), "non-existent.sock")
	client := NewClient(nonExistentSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Ping(ctx)
	if err == nil {
		t.Fatal("expected error for non-existent socket, got nil")
	}

	if !errors.Is(err, ErrSocketUnavailable) {
		t.Fatalf("expected ErrSocketUnavailable, got: %v", err)
	}
}

func TestDiscovery_SyncContainersToConfigDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	containers := []ContainerInfo{
		{
			ID:           "c1",
			Name:         "app-worker",
			Image:        "alpine:latest",
			State:        "running",
			RestartCount: 0,
		},
		{
			ID:           "c2",
			Name:         "db-postgres",
			Image:        "postgres:16",
			State:        "running",
			RestartCount: 1,
		},
	}

	// Initial Sync
	if err := SyncContainers(ctx, store.Config, containers, 500); err != nil {
		t.Fatalf("initial SyncContainers failed: %v", err)
	}

	// Verify container and source rows
	var (
		c1FirstSeen, c1LastSeen int64
		c1Status                string
		c1RestartCount          int
	)
	err = store.Config.RO.QueryRowContext(ctx, `
		SELECT first_seen, last_seen, last_status, restart_count
		FROM container WHERE id = 'c1'
	`).Scan(&c1FirstSeen, &c1LastSeen, &c1Status, &c1RestartCount)
	if err != nil {
		t.Fatalf("query container c1: %v", err)
	}
	if c1Status != "running" || c1RestartCount != 0 {
		t.Errorf("unexpected c1 initial state: status=%s, restarts=%d", c1Status, c1RestartCount)
	}

	var sourceFirstSeen, sourceLastSeen int64
	err = store.Config.RO.QueryRowContext(ctx, `
		SELECT first_seen, last_seen
		FROM source WHERE kind = 'container' AND name = 'app-worker'
	`).Scan(&sourceFirstSeen, &sourceLastSeen)
	if err != nil {
		t.Fatalf("query source app-worker: %v", err)
	}
	if sourceFirstSeen == 0 || sourceLastSeen == 0 {
		t.Errorf("expected non-zero timestamps for source")
	}

	// Wait 1 second and sync updated state
	time.Sleep(1100 * time.Millisecond)

	updatedContainers := []ContainerInfo{
		{
			ID:           "c1",
			Name:         "app-worker",
			Image:        "alpine:latest",
			State:        "exited",
			RestartCount: 2,
		},
		{
			ID:           "c2",
			Name:         "db-postgres",
			Image:        "postgres:16",
			State:        "running",
			RestartCount: 1,
		},
	}

	if err := SyncContainers(ctx, store.Config, updatedContainers, 500); err != nil {
		t.Fatalf("second SyncContainers failed: %v", err)
	}

	var (
		c1UpdatedFirstSeen, c1UpdatedLastSeen int64
		c1UpdatedStatus                       string
		c1UpdatedRestartCount                 int
	)
	err = store.Config.RO.QueryRowContext(ctx, `
		SELECT first_seen, last_seen, last_status, restart_count
		FROM container WHERE id = 'c1'
	`).Scan(&c1UpdatedFirstSeen, &c1UpdatedLastSeen, &c1UpdatedStatus, &c1UpdatedRestartCount)
	if err != nil {
		t.Fatalf("query updated container c1: %v", err)
	}

	// first_seen MUST be preserved, last_seen MUST advance, status & restart_count updated
	if c1UpdatedFirstSeen != c1FirstSeen {
		t.Errorf("first_seen changed: was %d, now %d", c1FirstSeen, c1UpdatedFirstSeen)
	}
	if c1UpdatedLastSeen <= c1LastSeen {
		t.Errorf("last_seen did not advance: was %d, now %d", c1LastSeen, c1UpdatedLastSeen)
	}
	if c1UpdatedStatus != "exited" {
		t.Errorf("expected status 'exited', got %q", c1UpdatedStatus)
	}
	if c1UpdatedRestartCount != 2 {
		t.Errorf("expected restart_count 2, got %d", c1UpdatedRestartCount)
	}

	// Verify maxSources capping
	lotsOfContainers := make([]ContainerInfo, 10)
	for i := 0; i < 10; i++ {
		lotsOfContainers[i] = ContainerInfo{
			ID:    fmt.Sprintf("bulk-%d", i),
			Name:  fmt.Sprintf("bulk-service-%d", i),
			Image: "test:latest",
			State: "running",
		}
	}
	if err := SyncContainers(ctx, store.Config, lotsOfContainers, 3); err != nil {
		t.Fatalf("capped SyncContainers failed: %v", err)
	}

	var count int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT count(*) FROM container WHERE id LIKE 'bulk-%'`).Scan(&count); err != nil {
		t.Fatalf("query bulk count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected exactly 3 bulk containers inserted due to maxSources cap, got %d", count)
	}
}
