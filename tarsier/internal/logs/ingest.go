package logs

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// DefaultLogQueueCap is the default maximum bounded capacity of the log ingestion channel.
const DefaultLogQueueCap = 8192

// LogEvaluatorTap defines the interface for tapping log entries for sliding-window pattern alert evaluation.
type LogEvaluatorTap interface {
	Enqueue(entry model.LogEntry)
}

// LogIngestorConfig configures the LogIngestor.
type LogIngestorConfig struct {
	TailCache   *TailCache
	ChunkWriter *ChunkWriterManager
	Evaluator   LogEvaluatorTap
	QueueCap    int
}

// LogIngestor implements model.LogSink by queuing incoming log entries from Docker followers
// and host file tailers into a bounded channel, and multiplexing them to TailCache (memory),
// ChunkWriterManager (compressed disk chunks), and LogEvaluatorTap (alerts).
type LogIngestor struct {
	tailCache   *TailCache
	chunkWriter *ChunkWriterManager
	evaluator   LogEvaluatorTap
	queue       chan model.LogEntry

	dropped  atomic.Uint64
	ingested atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewLogIngestor creates and starts a LogIngestor worker.
func NewLogIngestor(cfg LogIngestorConfig) *LogIngestor {
	qCap := cfg.QueueCap
	if qCap <= 0 {
		qCap = DefaultLogQueueCap
	}

	ctx, cancel := context.WithCancel(context.Background())
	ing := &LogIngestor{
		tailCache:   cfg.TailCache,
		chunkWriter: cfg.ChunkWriter,
		evaluator:   cfg.Evaluator,
		queue:       make(chan model.LogEntry, qCap),
		ctx:         ctx,
		cancel:      cancel,
	}

	ing.wg.Add(1)
	go ing.runWorker()

	return ing
}

// PushLog implements model.LogSink. It pushes an entry without blocking.
// Returns false and increments dropped count if the queue is full.
func (ing *LogIngestor) PushLog(entry model.LogEntry) bool {
	select {
	case ing.queue <- entry:
		ing.ingested.Add(1)
		return true
	default:
		ing.dropped.Add(1)
		return false
	}
}

// DroppedLines returns the total number of lines dropped due to a saturated queue.
func (ing *LogIngestor) DroppedLines() uint64 {
	return ing.dropped.Load()
}

// IngestedLines returns the total number of lines successfully accepted into the queue.
func (ing *LogIngestor) IngestedLines() uint64 {
	return ing.ingested.Load()
}

// runWorker drains incoming log entries and distributes them to downstream components.
func (ing *LogIngestor) runWorker() {
	defer ing.wg.Done()

	for {
		select {
		case <-ing.ctx.Done():
			// Drain remaining queued entries on shutdown
			for {
				select {
				case entry := <-ing.queue:
					ing.dispatch(entry)
				default:
					return
				}
			}
		case entry, ok := <-ing.queue:
			if !ok {
				return
			}
			ing.dispatch(entry)
		}
	}
}

func (ing *LogIngestor) dispatch(entry model.LogEntry) {
	if ing.tailCache != nil {
		ing.tailCache.Push(entry)
	}
	if ing.chunkWriter != nil {
		_ = ing.chunkWriter.Write(entry)
	}
	if ing.evaluator != nil {
		ing.evaluator.Enqueue(entry)
	}
}

// Close gracefully terminates the worker, draining any queued entries before returning.
func (ing *LogIngestor) Close(ctx context.Context) error {
	ing.cancel()

	done := make(chan struct{})
	go func() {
		ing.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
