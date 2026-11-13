package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrQueueFull is returned when a write operation cannot be queued due to channel capacity saturation.
	ErrQueueFull = errors.New("write queue is full")

	// ErrWriterClosed is returned when attempting to queue a write operation on a closed writer.
	ErrWriterClosed = errors.New("writer is closed")
)

// WriteFunc defines an atomic write transaction executed by the single-writer goroutine.
type WriteFunc func(ctx context.Context, tx *sql.Tx) error

type writeJob struct {
	ctx  context.Context
	fn   WriteFunc
	done chan error // buffered with capacity 1 for sync execution; nil for fire-and-forget
}

// WriterStats captures runtime counters for the single-writer worker.
type WriterStats struct {
	Queued   int
	Dropped  uint64
	Executed uint64
}

// Writer serializes all SQLite write operations through a dedicated background goroutine
// pinned to an isolated read-write *sql.DB handle configured with SetMaxOpenConns(1).
type Writer struct {
	rw       *sql.DB
	jobs     chan writeJob
	stop     chan struct{}
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	dropped  atomic.Uint64
	executed atomic.Uint64
}

// NewWriter starts a dedicated single-writer goroutine for rw with the given bounded queue capacity.
func NewWriter(rw *sql.DB, queueSize int) *Writer {
	if queueSize <= 0 {
		queueSize = 256
	}

	w := &Writer{
		rw:   rw,
		jobs: make(chan writeJob, queueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	go w.worker()
	return w
}

// ExecSync enqueues a write job and blocks until the transaction commits, rolls back, or ctx cancels.
func (w *Writer) ExecSync(ctx context.Context, fn WriteFunc) error {
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return ErrWriterClosed
	}

	done := make(chan error, 1)
	job := writeJob{
		ctx:  ctx,
		fn:   fn,
		done: done,
	}

	select {
	case w.jobs <- job:
		w.mu.RUnlock()
	case <-w.stop:
		w.mu.RUnlock()
		return ErrWriterClosed
	case <-ctx.Done():
		w.mu.RUnlock()
		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryEnqueue attempts to enqueue a fire-and-forget write job without blocking.
// If the bounded queue is full, it drops the job, increments Dropped, and returns ErrQueueFull.
func (w *Writer) TryEnqueue(fn WriteFunc) error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if w.closed {
		return ErrWriterClosed
	}

	job := writeJob{
		ctx:  context.Background(),
		fn:   fn,
		done: nil,
	}

	select {
	case w.jobs <- job:
		return nil
	default:
		w.dropped.Add(1)
		return ErrQueueFull
	}
}

// Stats returns a snapshot of queue length, dropped writes, and executed writes.
func (w *Writer) Stats() WriterStats {
	return WriterStats{
		Queued:   len(w.jobs),
		Dropped:  w.dropped.Load(),
		Executed: w.executed.Load(),
	}
}

// Close gracefully closes the input queue, drains all queued jobs, and waits for worker termination
// until ctx expires. Subsequent writes return ErrWriterClosed.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.stop)
	close(w.jobs)
	w.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) worker() {
	defer close(w.done)

	for job := range w.jobs {
		w.executeJob(job)
	}
}

func (w *Writer) executeJob(job writeJob) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in write job: %v", r)
		}
		if job.done != nil {
			job.done <- err
		}
	}()

	if job.ctx.Err() != nil {
		err = job.ctx.Err()
		return
	}

	tx, beginErr := w.rw.BeginTx(job.ctx, nil)
	if beginErr != nil {
		err = fmt.Errorf("begin tx: %w", beginErr)
		return
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if execErr := job.fn(job.ctx, tx); execErr != nil {
		_ = tx.Rollback()
		err = execErr
		return
	}

	if commitErr := tx.Commit(); commitErr != nil {
		err = fmt.Errorf("commit tx: %w", commitErr)
		return
	}

	w.executed.Add(1)
}

// BatchConfig specifies sizing and timing limits for BatchWriter.
type BatchConfig struct {
	MaxBatchSize  int
	FlushInterval time.Duration
	QueueSize     int
	OnError       func(err error)
}

// BatchFlushFunc defines the batch write transaction executed by BatchWriter.
type BatchFlushFunc[T any] func(ctx context.Context, tx *sql.Tx, items []T) error

// BatchStats captures runtime metrics for BatchWriter.
type BatchStats struct {
	Queued  int
	Dropped uint64
	Flushed uint64
}

// BatchWriter buffers individual items of type T and flushes them in batches to the single-writer goroutine
// when either MaxBatchSize items accumulate or FlushInterval elapses.
type BatchWriter[T any] struct {
	writer        *Writer
	flushFn       BatchFlushFunc[T]
	items         chan T
	done          chan struct{}
	maxBatchSize  int
	flushInterval time.Duration
	onError       func(err error)
	mu            sync.RWMutex
	closed        bool
	dropped       atomic.Uint64
	flushed       atomic.Uint64
}

// NewBatchWriter constructs and starts a BatchWriter feeding w.
func NewBatchWriter[T any](w *Writer, cfg BatchConfig, flushFn BatchFlushFunc[T]) *BatchWriter[T] {
	maxBatch := cfg.MaxBatchSize
	if maxBatch <= 0 {
		maxBatch = 500
	}

	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = maxBatch * 2
	}

	bw := &BatchWriter[T]{
		writer:        w,
		flushFn:       flushFn,
		items:         make(chan T, queueSize),
		done:          make(chan struct{}),
		maxBatchSize:  maxBatch,
		flushInterval: interval,
		onError:       cfg.OnError,
	}

	go bw.run()
	return bw
}

// TryEnqueue pushes an item without blocking. If the queue is saturated, it increments Dropped
// and returns ErrQueueFull.
func (bw *BatchWriter[T]) TryEnqueue(item T) error {
	bw.mu.RLock()
	defer bw.mu.RUnlock()

	if bw.closed {
		return ErrWriterClosed
	}

	select {
	case bw.items <- item:
		return nil
	default:
		bw.dropped.Add(1)
		return ErrQueueFull
	}
}

// Enqueue pushes an item with context cancellation support if backpressure is desired.
func (bw *BatchWriter[T]) Enqueue(ctx context.Context, item T) error {
	bw.mu.RLock()
	if bw.closed {
		bw.mu.RUnlock()
		return ErrWriterClosed
	}

	select {
	case bw.items <- item:
		bw.mu.RUnlock()
		return nil
	case <-ctx.Done():
		bw.mu.RUnlock()
		return ctx.Err()
	}
}

// Stats returns a snapshot of queued items, dropped items, and successfully flushed items.
func (bw *BatchWriter[T]) Stats() BatchStats {
	return BatchStats{
		Queued:  len(bw.items),
		Dropped: bw.dropped.Load(),
		Flushed: bw.flushed.Load(),
	}
}

// Close closes the incoming item queue, drains and flushes all buffered items, and waits for completion.
func (bw *BatchWriter[T]) Close(ctx context.Context) error {
	bw.mu.Lock()
	if bw.closed {
		bw.mu.Unlock()
		return nil
	}
	bw.closed = true
	close(bw.items)
	bw.mu.Unlock()

	select {
	case <-bw.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (bw *BatchWriter[T]) run() {
	defer close(bw.done)

	batch := make([]T, 0, bw.maxBatchSize)
	ticker := time.NewTicker(bw.flushInterval)
	defer ticker.Stop()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		itemsToFlush := batch
		batch = make([]T, 0, bw.maxBatchSize)

		err := bw.writer.ExecSync(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			return bw.flushFn(ctx, tx, itemsToFlush)
		})
		if err != nil {
			if bw.onError != nil {
				bw.onError(err)
			}
			return err
		}
		bw.flushed.Add(uint64(len(itemsToFlush)))
		return nil
	}

	for {
		select {
		case item, ok := <-bw.items:
			if !ok {
				_ = flush()
				return
			}
			batch = append(batch, item)
			if len(batch) >= bw.maxBatchSize {
				_ = flush()
				ticker.Reset(bw.flushInterval)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				_ = flush()
			}
		}
	}
}
