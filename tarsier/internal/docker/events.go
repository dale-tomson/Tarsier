package docker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// ContainerEvent represents a normalized container lifecycle transition.
type ContainerEvent struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"` // Normalized without leading '/'
	Image         string `json:"image"`
	Action        string `json:"action"` // "start", "stop", "die", "oom", "restart", "destroy", "kill"
	TS            int64  `json:"ts"`     // Unix epoch seconds
	ExitCode      int    `json:"exit_code,omitempty"`
	Detail        string `json:"detail"` // e.g. "exit_code=137 oom=true"
}

type dockerEventFrame struct {
	Type     string `json:"Type"`   // "container"
	Action   string `json:"Action"` // "create", "start", "stop", "die", "oom", "restart", "destroy"
	Time     int64  `json:"time"`   // Unix epoch seconds
	TimeNano int64  `json:"timeNano"`
	Actor    struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// EventWatcher connects to Docker's streaming /events API, monitors container lifecycle changes,
// filters events according to ContainerFilter, persists transitions into config.db,
// and publishes events to downstream listeners.
type EventWatcher struct {
	client     *Client
	filter     *ContainerFilter
	configDB   *db.Handle
	eventsChan chan ContainerEvent
	onEvent    func(ContainerEvent)

	mu        sync.RWMutex
	cursorTS  int64
	connected bool
}

// EventWatcherOptions configures an EventWatcher.
type EventWatcherOptions struct {
	InitialCursorTS int64
	OnEvent         func(ContainerEvent)
}

// ResolveInitialCursor resolves the starting event stream timestamp from config.db.
// If container_event has recorded events, it returns MAX(ts); otherwise it falls back
// to now minus lookback (defaulting to 24h when lookback <= 0).
func ResolveInitialCursor(ctx context.Context, configDB *db.Handle, lookback time.Duration) int64 {
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	if configDB != nil && configDB.RO != nil {
		var maxTS sql.NullInt64
		err := configDB.RO.QueryRowContext(ctx, `SELECT MAX(ts) FROM container_event;`).Scan(&maxTS)
		if err == nil && maxTS.Valid && maxTS.Int64 > 0 {
			return maxTS.Int64
		}
	}
	return time.Now().Add(-lookback).Unix()
}

// NewEventWatcher initializes an EventWatcher.
func NewEventWatcher(client *Client, filter *ContainerFilter, configDB *db.Handle, opts ...EventWatcherOptions) *EventWatcher {
	var opt EventWatcherOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	startTS := opt.InitialCursorTS
	if startTS <= 0 {
		startTS = time.Now().Unix()
	}

	return &EventWatcher{
		client:     client,
		filter:     filter,
		configDB:   configDB,
		eventsChan: make(chan ContainerEvent, 256),
		onEvent:    opt.OnEvent,
		cursorTS:   startTS,
	}
}

// Events returns the read-only channel delivering normalized container lifecycle events.
func (w *EventWatcher) Events() <-chan ContainerEvent {
	return w.eventsChan
}

// Connected returns whether the watcher currently has an active connection to Docker's event stream.
func (w *EventWatcher) Connected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.connected
}

// CursorTS returns the current stream cursor timestamp.
func (w *EventWatcher) CursorTS() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cursorTS
}

func (w *EventWatcher) setConnected(c bool) {
	w.mu.Lock()
	w.connected = c
	w.mu.Unlock()
}

func (w *EventWatcher) updateCursor(ts int64) {
	w.mu.Lock()
	if ts > w.cursorTS {
		w.cursorTS = ts
	}
	w.mu.Unlock()
}

// Run begins streaming events from the Docker daemon. It loops with exponential backoff
// on disconnect until ctx is cancelled.
func (w *EventWatcher) Run(ctx context.Context) error {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			w.setConnected(false)
			return nil
		default:
		}

		err := w.stream(ctx)
		w.setConnected(false)

		if ctx.Err() != nil {
			return nil
		}

		if err != nil {
			log.Printf("Docker event stream disconnected: %v; reconnecting in %v...", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = 1 * time.Second
		}
	}
}

func (w *EventWatcher) stream(ctx context.Context) error {
	w.mu.RLock()
	since := w.cursorTS
	w.mu.RUnlock()

	filtersJSON := `{"type":["container"]}`
	queryURL := fmt.Sprintf("http://localhost/events?since=%d&filters=%s",
		since, url.QueryEscape(filtersJSON))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
	if err != nil {
		return err
	}

	httpClient := w.client.StreamHTTPClient()
	if httpClient == nil {
		httpClient = w.client.httpClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		lr := io.LimitReader(resp.Body, 1024)
		body, _ := io.ReadAll(lr)
		return fmt.Errorf("docker /events returned unexpected status %d: %s", resp.StatusCode, string(body))
	}

	w.setConnected(true)

	// Unbounded line reader so containers with large labels/annotations (>64 KiB) do not fail decoding.
	reader := bufio.NewReaderSize(resp.Body, 16<<10)

	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var frame dockerEventFrame
			if jsonErr := json.Unmarshal(trimmed, &frame); jsonErr == nil {
				w.updateCursor(frame.Time)

				if ev, ok := w.processFrame(frame); ok {
					if pErr := w.persistAndDispatch(ctx, ev); pErr != nil {
						log.Printf("Warning: failed to persist container event %s (%s): %v", ev.ContainerName, ev.Action, pErr)
					}
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			return readErr
		}
	}
}

func (w *EventWatcher) processFrame(frame dockerEventFrame) (ContainerEvent, bool) {
	// Only track container events
	if frame.Type != "" && frame.Type != "container" {
		return ContainerEvent{}, false
	}

	action := strings.ToLower(strings.TrimSpace(frame.Action))
	// Strip sub-actions e.g. "health_status: unhealthy" or exec actions
	if idx := strings.Index(action, ":"); idx != -1 {
		action = strings.TrimSpace(action[:idx])
	}

	// Filter down strictly to lifecycle actions
	switch action {
	case "start", "stop", "die", "oom", "restart", "destroy", "kill":
		// Tracked
	default:
		return ContainerEvent{}, false
	}

	name := strings.TrimPrefix(strings.TrimSpace(frame.Actor.Attributes["name"]), "/")
	if name == "" {
		name = frame.Actor.ID
		if len(name) > 12 {
			name = name[:12]
		}
	}

	// Apply ContainerFilter
	if w.filter != nil && !w.filter.Match(name) {
		return ContainerEvent{}, false
	}

	var exitCode int
	if ecStr, ok := frame.Actor.Attributes["exitCode"]; ok {
		if ec, err := strconv.Atoi(ecStr); err == nil {
			exitCode = ec
		}
	}

	detail := ""
	oomKilled := frame.Actor.Attributes["oomKilled"] == "true" || action == "oom"

	switch action {
	case "die":
		if oomKilled {
			detail = fmt.Sprintf("exit_code=%d oom=true", exitCode)
		} else {
			detail = fmt.Sprintf("exit_code=%d", exitCode)
		}
	case "oom":
		detail = "oom=true"
	case "restart":
		detail = "restarting"
	case "kill":
		if sig := frame.Actor.Attributes["signal"]; sig != "" {
			detail = fmt.Sprintf("signal=%s", sig)
		}
	}

	ev := ContainerEvent{
		ContainerID:   frame.Actor.ID,
		ContainerName: name,
		Image:         strings.TrimPrefix(strings.TrimSpace(frame.Actor.Attributes["image"]), "/"),
		Action:        action,
		TS:            frame.Time,
		ExitCode:      exitCode,
		Detail:        detail,
	}

	return ev, true
}

func (w *EventWatcher) persistAndDispatch(ctx context.Context, ev ContainerEvent) error {
	ev.ContainerName = strings.TrimPrefix(strings.TrimSpace(ev.ContainerName), "/")
	ev.Image = strings.TrimPrefix(strings.TrimSpace(ev.Image), "/")
	var (
		inspect    ContainerInspect
		inspectErr = errors.New("not inspected")
	)

	if (ev.Action == "start" || ev.Action == "die" || ev.Action == "restart") && w.client != nil && ev.ContainerID != "" {
		inspectCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		inspect, inspectErr = w.client.InspectContainer(inspectCtx, ev.ContainerID)
		cancel()
	}

	if w.configDB != nil {
		err := w.configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			wasStart := ev.Action == "start"
			newRestartCount := 0

			if wasStart {
				var prevStatus string
				var prevRestartCount int
				var existsInDB bool
				rowErr := tx.QueryRowContext(ctx, `
					SELECT last_status, restart_count FROM container WHERE id = ?;
				`, ev.ContainerID).Scan(&prevStatus, &prevRestartCount)
				if rowErr == nil {
					existsInDB = true
				}

				inspectRestarts := 0
				if inspectErr == nil {
					inspectRestarts = inspect.RestartCount
				}

				isRestart := inspectRestarts > 0 || (existsInDB && (prevStatus == "exited" || prevStatus == "dead" || prevStatus == "restarting"))
				if isRestart {
					newRestartCount = prevRestartCount
					if inspectErr == nil && inspectRestarts > newRestartCount {
						newRestartCount = inspectRestarts
					} else if existsInDB && (prevStatus == "exited" || prevStatus == "dead" || prevStatus == "restarting") {
						newRestartCount = prevRestartCount + 1
					} else if newRestartCount == 0 {
						newRestartCount = 1
					}

					ev.Action = "restart"
					ev.Detail = fmt.Sprintf("restarted (count=%d)", newRestartCount)
				}
			}

			// 1. Insert into container_event table
			_, err := tx.ExecContext(ctx, `
				INSERT INTO container_event (container_id, ts, type, detail)
				VALUES (?, ?, ?, ?);
			`, ev.ContainerID, ev.TS, ev.Action, ev.Detail)
			if err != nil {
				return fmt.Errorf("insert container_event: %w", err)
			}

			// 2. Update container table state
			switch ev.Action {
			case "start":
				if ev.ContainerName != "" {
					_, _ = tx.ExecContext(ctx, `
						UPDATE container SET last_status = 'exited'
						WHERE name = ? AND id != ? AND last_status IN ('running', 'restarting');
					`, ev.ContainerName, ev.ContainerID)
				}

				_, err = tx.ExecContext(ctx, `
					INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
					VALUES (?, ?, ?, ?, ?, 'running', 0)
					ON CONFLICT(id) DO UPDATE SET
						name = CASE WHEN excluded.name != '' THEN excluded.name ELSE container.name END,
						image = CASE WHEN excluded.image != '' THEN excluded.image ELSE container.image END,
						last_status = 'running',
						last_seen = excluded.last_seen,
						restart_count = MAX(container.restart_count, excluded.restart_count);
				`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS)

			case "restart":
				if ev.ContainerName != "" {
					_, _ = tx.ExecContext(ctx, `
						UPDATE container SET last_status = 'exited'
						WHERE name = ? AND id != ? AND last_status IN ('running', 'restarting');
					`, ev.ContainerName, ev.ContainerID)
				}

				if wasStart {
					_, err = tx.ExecContext(ctx, `
						INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
						VALUES (?, ?, ?, ?, ?, 'running', ?)
						ON CONFLICT(id) DO UPDATE SET
							name = CASE WHEN excluded.name != '' THEN excluded.name ELSE container.name END,
							image = CASE WHEN excluded.image != '' THEN excluded.image ELSE container.image END,
							last_status = 'running',
							last_seen = excluded.last_seen,
							restart_count = MAX(container.restart_count, excluded.restart_count);
					`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS, newRestartCount)
				} else {
					inspectRestarts := 1
					if inspectErr == nil && inspect.RestartCount > inspectRestarts {
						inspectRestarts = inspect.RestartCount
					}
					_, err = tx.ExecContext(ctx, `
						INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
						VALUES (?, ?, ?, ?, ?, 'running', ?)
						ON CONFLICT(id) DO UPDATE SET
							last_status = 'running',
							last_seen = excluded.last_seen,
							restart_count = MAX(container.restart_count + 1, excluded.restart_count);
					`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS, inspectRestarts)
				}

			case "die":
				dieStatus := "exited"
				if inspectErr == nil && strings.EqualFold(strings.TrimSpace(inspect.State.Status), "restarting") {
					dieStatus = "restarting"
				}
				_, err = tx.ExecContext(ctx, `
					INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
					VALUES (?, ?, ?, ?, ?, ?, 0)
					ON CONFLICT(id) DO UPDATE SET
						last_status = excluded.last_status,
						last_seen = excluded.last_seen;
				`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS, dieStatus)

			case "stop":
				_, err = tx.ExecContext(ctx, `
					INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
					VALUES (?, ?, ?, ?, ?, 'exited', 0)
					ON CONFLICT(id) DO UPDATE SET
						last_status = 'exited',
						last_seen = excluded.last_seen;
				`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS)

			case "destroy":
				_, err = tx.ExecContext(ctx, `
					INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
					VALUES (?, ?, ?, ?, ?, 'destroyed', 0)
					ON CONFLICT(id) DO UPDATE SET
						last_status = 'destroyed',
						last_seen = excluded.last_seen;
				`, ev.ContainerID, ev.ContainerName, ev.Image, ev.TS, ev.TS)
			}

			if err != nil {
				return fmt.Errorf("upsert container state (%s): %w", ev.Action, err)
			}

			// 3. Upsert source entry
			_, err = tx.ExecContext(ctx, `
				INSERT INTO source (kind, name, host_id, first_seen, last_seen)
				VALUES ('container', ?, 1, ?, ?)
				ON CONFLICT(kind, name) DO UPDATE SET
					last_seen = excluded.last_seen;
			`, ev.ContainerName, ev.TS, ev.TS)
			if err != nil {
				return fmt.Errorf("upsert source (%s): %w", ev.ContainerName, err)
			}

			return nil
		})

		if err != nil {
			return err
		}
	} else if ev.Action == "start" && inspectErr == nil && inspect.RestartCount > 0 {
		ev.Action = "restart"
		ev.Detail = fmt.Sprintf("restarted (count=%d)", inspect.RestartCount)
	}

	// Dispatch to subscriber channel (non-blocking)
	select {
	case w.eventsChan <- ev:
	default:
		// Queue full; avoid blocking
	}

	if w.onEvent != nil {
		w.onEvent(ev)
	}

	return nil
}
