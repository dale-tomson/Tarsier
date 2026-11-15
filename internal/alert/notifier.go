package alert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
)

// Notification represents an alert delivery payload for outbound notifiers.
type Notification struct {
	EventID  int64             // Optional alert_event.id for updating notify_status in config.db
	Rule     string
	Target   string
	Severity string            // "info" | "warning" | "critical"
	State    string            // "firing" | "resolved"
	Summary  string
	Details  map[string]string
	At       time.Time
}

// Notifier defines the common interface for notification delivery targets.
type Notifier interface {
	Name() string
	Send(ctx context.Context, n Notification) error
}

// PermanentError indicates a non-retryable error (e.g. bad credentials, 401/403/404).
type PermanentError struct {
	Err error
}

func (p PermanentError) Error() string {
	return p.Err.Error()
}

func (p PermanentError) Unwrap() error {
	return p.Err
}

// RateLimitError indicates an HTTP 429 or provider rate limit with a suggested delay.
type RateLimitError struct {
	RetryAfter time.Duration
	Err        error
}

func (r RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (retry after %v): %v", r.RetryAfter, r.Err)
}

func (r RateLimitError) Unwrap() error {
	return r.Err
}

// DispatcherOption allows customizing Dispatcher behavior (e.g. in tests).
type DispatcherOption func(*Dispatcher)

// WithRetryBackoffs overrides default retry backoff delays.
func WithRetryBackoffs(backoffs []time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if len(backoffs) > 0 {
			d.backoffs = backoffs
		}
	}
}

// WithAttemptTimeout overrides the default per-attempt timeout (15s).
func WithAttemptTimeout(timeout time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if timeout > 0 {
			d.attemptTimeout = timeout
		}
	}
}

// WithNotifier registers or replaces a named notifier.
func WithNotifier(name string, n Notifier) DispatcherOption {
	return func(d *Dispatcher) {
		d.customNotifiers[name] = n
	}
}

type dispatchJob struct {
	notification Notification
	completion   func(channel string, err error)
}

type channelWorker struct {
	name     string
	notifier Notifier
	queue    chan dispatchJob
	dropped  atomic.Uint64
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

// Dispatcher manages asynchronous bounded fan-out delivery across configured channels.
type Dispatcher struct {
	channels        map[string]*channelWorker
	customNotifiers map[string]Notifier
	backoffs        []time.Duration
	attemptTimeout  time.Duration
	store           *db.Store
	mu              sync.RWMutex
	closed          bool
}

// NewDispatcher initializes notifiers and per-channel worker queues from configuration.
func NewDispatcher(cfg config.NotifiersConfig, store *db.Store, opts ...DispatcherOption) (*Dispatcher, error) {
	d := &Dispatcher{
		channels:        make(map[string]*channelWorker),
		customNotifiers: make(map[string]Notifier),
		backoffs:        []time.Duration{1 * time.Second, 4 * time.Second, 15 * time.Second},
		attemptTimeout:  15 * time.Second,
		store:           store,
	}

	for _, opt := range opts {
		opt(d)
	}

	// Register Discord if enabled or customized
	if dn, ok := d.customNotifiers["discord"]; ok {
		d.registerChannelWorker("discord", dn)
	} else if cfg.Discord.Enabled && strings.TrimSpace(cfg.Discord.WebhookURL) != "" {
		dn := NewDiscordNotifier(cfg.Discord)
		d.registerChannelWorker("discord", dn)
	}

	// Register Email if enabled or customized
	if en, ok := d.customNotifiers["email"]; ok {
		d.registerChannelWorker("email", en)
	} else if cfg.Email.Enabled && strings.TrimSpace(cfg.Email.SMTPHost) != "" {
		en := NewSMTPNotifier(cfg.Email)
		d.registerChannelWorker("email", en)
	}

	// Register any remaining custom notifiers
	for name, n := range d.customNotifiers {
		if _, exists := d.channels[name]; !exists {
			d.registerChannelWorker(name, n)
		}
	}

	return d, nil
}

func (d *Dispatcher) registerChannelWorker(name string, n Notifier) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &channelWorker{
		name:     name,
		notifier: n,
		queue:    make(chan dispatchJob, 128),
		ctx:      ctx,
		cancel:   cancel,
	}

	worker.wg.Add(1)
	go d.runWorker(worker)
	d.channels[name] = worker
}

func (d *Dispatcher) runWorker(w *channelWorker) {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			// Drain remaining jobs before exiting
			for {
				select {
				case job, ok := <-w.queue:
					if !ok {
						return
					}
					err := d.sendWithRetries(w.ctx, w.notifier, job.notification)
					if job.completion != nil {
						job.completion(w.name, err)
					}
				default:
					return
				}
			}
		case job, ok := <-w.queue:
			if !ok {
				return
			}
			err := d.sendWithRetries(w.ctx, w.notifier, job.notification)
			if job.completion != nil {
				job.completion(w.name, err)
			}
		}
	}
}

func (d *Dispatcher) sendWithRetries(ctx context.Context, n Notifier, notif Notification) error {
	var lastErr error
	maxAttempts := len(d.backoffs) + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, d.attemptTimeout)
		err := n.Send(attemptCtx, notif)
		cancel()

		if err == nil {
			return nil
		}
		lastErr = err

		// Permanent errors are not retried
		var permErr PermanentError
		if errors.As(err, &permErr) {
			return lastErr
		}

		if attempt == maxAttempts-1 {
			break
		}

		// Calculate backoff delay
		var delay time.Duration
		var rateErr RateLimitError
		if errors.As(err, &rateErr) && rateErr.RetryAfter > 0 {
			delay = rateErr.RetryAfter
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		} else if attempt < len(d.backoffs) {
			base := d.backoffs[attempt]
			jitter := time.Duration(rand.Int63n(int64(base / 4))) // up to 25% jitter
			delay = base + jitter
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return lastErr
}

// deliveryTracker aggregates multi-channel results for a Notification with EventID > 0.
type deliveryTracker struct {
	eventID  int64
	store    *db.Store
	mu       sync.Mutex
	pending  int
	results  map[string]error
}

func (dt *deliveryTracker) onComplete(channel string, err error) {
	dt.mu.Lock()
	dt.results[channel] = err
	dt.pending--
	done := dt.pending <= 0
	dt.mu.Unlock()

	if done && dt.store != nil && dt.store.Config != nil && dt.eventID > 0 {
		status := dt.formatStatus()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = dt.store.Config.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, execErr := tx.ExecContext(ctx,
				"UPDATE alert_event SET notify_status = ? WHERE id = ?",
				status, dt.eventID)
			return execErr
		})
	}
}

func (dt *deliveryTracker) formatStatus() string {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	var sent []string
	var failed []string

	for ch, err := range dt.results {
		if err == nil {
			sent = append(sent, ch)
		} else {
			failed = append(failed, fmt.Sprintf("%s (%s)", ch, err.Error()))
		}
	}

	if len(failed) == 0 {
		return "sent: " + strings.Join(sent, ", ")
	}
	if len(sent) == 0 {
		return "failed: " + strings.Join(failed, "; ")
	}
	return fmt.Sprintf("sent: %s; failed: %s", strings.Join(sent, ", "), strings.Join(failed, "; "))
}

// Dispatch executes non-blocking fan-out of the notification across requested channels.
func (d *Dispatcher) Dispatch(channels []string, n Notification) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed || len(channels) == 0 {
		return
	}

	var targets []*channelWorker
	for _, name := range channels {
		norm := strings.TrimSpace(strings.ToLower(name))
		if w, ok := d.channels[norm]; ok {
			targets = append(targets, w)
		}
	}

	if len(targets) == 0 {
		if n.EventID > 0 && d.store != nil && d.store.Config != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.store.Config.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					"UPDATE alert_event SET notify_status = ? WHERE id = ?",
					"unconfigured: no enabled channels", n.EventID)
				return err
			})
		}
		return
	}

	var tracker *deliveryTracker
	if n.EventID > 0 && d.store != nil {
		tracker = &deliveryTracker{
			eventID: n.EventID,
			store:   d.store,
			pending: len(targets),
			results: make(map[string]error),
		}
	}

	for _, w := range targets {
		job := dispatchJob{
			notification: n,
		}
		if tracker != nil {
			job.completion = tracker.onComplete
		}

		select {
		case w.queue <- job:
		default:
			// Queue full: non-blocking drop
			w.dropped.Add(1)
			if tracker != nil {
				tracker.onComplete(w.name, errors.New("dropped: queue full"))
			}
		}
	}
}

// SendTest sends an immediate test notification synchronously over the specified channel.
func (d *Dispatcher) SendTest(ctx context.Context, channel string) error {
	d.mu.RLock()
	w, ok := d.channels[strings.TrimSpace(strings.ToLower(channel))]
	d.mu.RUnlock()

	if !ok {
		return fmt.Errorf("channel %q is disabled or not configured", channel)
	}

	testNotif := Notification{
		Rule:     "Test Notification",
		Target:   "system",
		Severity: "info",
		State:    "firing",
		Summary:  "This is a test notification from Tarsier to verify connectivity and credentials.",
		Details: map[string]string{
			"channel": channel,
			"source":  "manual test",
		},
		At: time.Now().UTC(),
	}

	return w.notifier.Send(ctx, testNotif)
}

// DroppedCount returns the count of dropped notifications for a channel.
func (d *Dispatcher) DroppedCount(channel string) uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if w, ok := d.channels[channel]; ok {
		return w.dropped.Load()
	}
	return 0
}

// HasChannel returns true if the channel is registered.
func (d *Dispatcher) HasChannel(channel string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.channels[strings.TrimSpace(strings.ToLower(channel))]
	return ok
}

// Close gracefully closes all worker queues and waits for in-flight deliveries to drain.
func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true

	workers := make([]*channelWorker, 0, len(d.channels))
	for _, w := range d.channels {
		workers = append(workers, w)
		close(w.queue)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for _, w := range workers {
			w.wg.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, w := range workers {
			w.cancel()
		}
		return ctx.Err()
	}
}
