package docker

import (
	"context"
	"database/sql"
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

	// Backdate initial timestamps in the database so last_seen advancement is verified deterministically without sleeping
	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx, `UPDATE container SET first_seen = first_seen - 10, last_seen = last_seen - 10 WHERE id = 'c1';`); err != nil {
			return err
		}
		_, err := tx.ExecContext(txCtx, `UPDATE source SET first_seen = first_seen - 10, last_seen = last_seen - 10 WHERE kind = 'container' AND name = 'app-worker';`)
		return err
	})
	if err != nil {
		t.Fatalf("backdate c1 timestamps: %v", err)
	}
	c1FirstSeen -= 10
	c1LastSeen -= 10

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

func TestDiscovery_SyncContainersSupersedesAndReconcilesStale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Initial container c1 running with name "web-server"
	containersV1 := []ContainerInfo{
		{
			ID:           "c1",
			Name:         "web-server",
			Image:        "nginx:alpine",
			State:        "running",
			RestartCount: 0,
		},
		{
			ID:           "c2",
			Name:         "redis-cache",
			Image:        "redis:7",
			State:        "running",
			RestartCount: 0,
		},
	}
	if err := SyncContainers(ctx, store.Config, containersV1, 500); err != nil {
		t.Fatalf("v1 sync failed: %v", err)
	}

	// Now container "web-server" is recreated with new ID "c3".
	// "redis-cache" was deleted/stopped and absent from Docker!
	containersV2 := []ContainerInfo{
		{
			ID:           "c3",
			Name:         "web-server",
			Image:        "nginx:alpine",
			State:        "running",
			RestartCount: 0,
		},
	}
	if err := SyncContainers(ctx, store.Config, containersV2, 500); err != nil {
		t.Fatalf("v2 sync failed: %v", err)
	}

	// 1. Verify c1 (old web-server) status was changed to 'exited'
	var c1Status string
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status FROM container WHERE id = 'c1'`).Scan(&c1Status); err != nil {
		t.Fatalf("query c1 status: %v", err)
	}
	if c1Status != "exited" {
		t.Errorf("expected superseded c1 status 'exited', got %q", c1Status)
	}

	// 2. Verify c2 (absent redis-cache) status was reconciled to 'exited'
	var c2Status string
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status FROM container WHERE id = 'c2'`).Scan(&c2Status); err != nil {
		t.Fatalf("query c2 status: %v", err)
	}
	if c2Status != "exited" {
		t.Errorf("expected absent c2 status 'exited', got %q", c2Status)
	}

	// 3. Verify c3 (new web-server) status is 'running'
	var c3Status string
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status FROM container WHERE id = 'c3'`).Scan(&c3Status); err != nil {
		t.Fatalf("query c3 status: %v", err)
	}
	if c3Status != "running" {
		t.Errorf("expected new c3 status 'running', got %q", c3Status)
	}
}

func TestClient_ListContainersEnrichesFromInspect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		payload := `[
			{
				"Id": "c-restarting",
				"Names": ["/crash-app"],
				"Image": "app:1.0",
				"State": "running",
				"Status": "Restarting (1) 2 seconds ago",
				"Created": 1700000000
			},
			{
				"Id": "c-noinspect",
				"Names": ["/basic-app"],
				"Image": "busybox:latest",
				"State": "running",
				"Status": "Up 1 minute",
				"Created": 1700000010
			}
		]`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	})
	mux.HandleFunc("/containers/c-restarting/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"Id": "c-restarting",
			"Name": "/crash-app",
			"Image": "app:1.0",
			"RestartCount": 4,
			"State": {"Status": "restarting", "Running": false},
			"Config": {"Tty": true},
			"HostConfig": {"Memory": 536870912}
		}`))
	})
	// c-noinspect deliberately returns 404 to verify graceful fallback to basic metadata

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := client.ListContainers(ctx, true)
	if err != nil {
		t.Fatalf("ListContainers failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(list))
	}

	c0 := list[0]
	if c0.RestartCount != 4 {
		t.Errorf("expected RestartCount 4, got %d", c0.RestartCount)
	}
	if c0.State != "restarting" {
		t.Errorf("expected State 'restarting', got %q", c0.State)
	}
	if c0.MemoryLimit != 536870912 {
		t.Errorf("expected MemoryLimit 536870912, got %d", c0.MemoryLimit)
	}
	if !c0.TTY {
		t.Errorf("expected TTY true")
	}

	c1 := list[1]
	if c1.Name != "basic-app" || c1.State != "running" || c1.RestartCount != 0 {
		t.Errorf("unexpected fallback metadata for c-noinspect: %+v", c1)
	}
}

func TestDiscovery_SyncContainersPreservesRestartCount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Initial sync with RestartCount 5 and state "restarting"
	if err := SyncContainers(ctx, store.Config, []ContainerInfo{
		{
			ID:           "c1",
			Name:         "worker",
			Image:        "worker:v1",
			State:        "restarting",
			RestartCount: 5,
		},
	}, 500); err != nil {
		t.Fatalf("initial SyncContainers failed: %v", err)
	}

	var status string
	var restarts int
	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status, restart_count FROM container WHERE id = 'c1'`).Scan(&status, &restarts); err != nil {
		t.Fatalf("query c1: %v", err)
	}
	if status != "restarting" || restarts != 5 {
		t.Fatalf("expected status='restarting', restart_count=5; got status=%q, restart_count=%d", status, restarts)
	}

	// Subsequent sync where RestartCount is 0 (e.g. fallback list without inspect):
	// restart_count MUST NOT be wiped out to 0!
	if err := SyncContainers(ctx, store.Config, []ContainerInfo{
		{
			ID:           "c1",
			Name:         "worker",
			Image:        "worker:v1",
			State:        "running",
			RestartCount: 0,
		},
	}, 500); err != nil {
		t.Fatalf("second SyncContainers failed: %v", err)
	}

	if err := store.Config.RO.QueryRowContext(ctx, `SELECT last_status, restart_count FROM container WHERE id = 'c1'`).Scan(&status, &restarts); err != nil {
		t.Fatalf("query c1 after second sync: %v", err)
	}
	if status != "running" {
		t.Errorf("expected status 'running', got %q", status)
	}
	if restarts != 5 {
		t.Errorf("expected preserved restart_count 5, got %d", restarts)
	}
}

func TestDiscovery_SyncContainersPrunesExpiredExited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().Unix()

	// Seed expired exited/dead containers (> 24h ago) and a recently exited one (< 24h ago)
	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
			VALUES
				('old-runner-1', 'ephemeral-1', 'alpine:3.18', ?, ?, 'exited', 0),
				('old-runner-2', 'ephemeral-2', 'alpine:3.18', ?, ?, 'dead', 0),
				('recent-exited', 'recent-job', 'alpine:3.18', ?, ?, 'exited', 0);

			INSERT INTO container_event (container_id, ts, type, detail)
			VALUES
				('old-runner-1', ?, 'die', 'exit_code=0'),
				('old-runner-2', ?, 'die', 'exit_code=1'),
				('recent-exited', ?, 'die', 'exit_code=0');
		`,
			now-26*3600, now-25*3600,
			now-28*3600, now-26*3600,
			now-3*3600, now-2*3600,
			now-25*3600,
			now-26*3600,
			now-2*3600,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed expired containers failed: %v", err)
	}

	activeContainers := []ContainerInfo{
		{
			ID:           "c-active",
			Name:         "web-app",
			Image:        "nginx:alpine",
			State:        "running",
			RestartCount: 0,
		},
	}

	if err := SyncContainers(ctx, store.Config, activeContainers, 500); err != nil {
		t.Fatalf("SyncContainers failed: %v", err)
	}

	// Expired containers and their events should be deleted
	for _, expiredID := range []string{"old-runner-1", "old-runner-2"} {
		var cCount, evCount int
		_ = store.Config.RO.QueryRowContext(ctx, `SELECT COUNT(*) FROM container WHERE id = ?;`, expiredID).Scan(&cCount)
		_ = store.Config.RO.QueryRowContext(ctx, `SELECT COUNT(*) FROM container_event WHERE container_id = ?;`, expiredID).Scan(&evCount)
		if cCount != 0 {
			t.Errorf("expected expired container %s to be pruned, count=%d", expiredID, cCount)
		}
		if evCount != 0 {
			t.Errorf("expected expired container_event for %s to be pruned, count=%d", expiredID, evCount)
		}
	}

	// Recently exited and active containers should remain
	for _, keptID := range []string{"recent-exited", "c-active"} {
		var cCount int
		_ = store.Config.RO.QueryRowContext(ctx, `SELECT COUNT(*) FROM container WHERE id = ?;`, keptID).Scan(&cCount)
		if cCount != 1 {
			t.Errorf("expected container %s to remain, count=%d", keptID, cCount)
		}
	}
}
