package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// IngestOptions configures buffering, batching, and caching thresholds for the metrics ingestion pipeline.
type IngestOptions struct {
	MaxBatchSize  int
	FlushInterval time.Duration
	QueueCapacity int
	MaxSeries     int
	RingCapacity  int
}

// IngestStats provides a snapshot of throughput and queue counters.
type IngestStats struct {
	Accepted    uint64
	Flushed     uint64
	Dropped     uint64
	SeriesCount int
}

// Ingestor coordinates the series registry, in-memory hot cache, and single-writer batch writer.
type Ingestor struct {
	handle      *db.Handle
	registry    *SeriesRegistry
	cache       *HotCache
	batchWriter *db.BatchWriter[model.MetricSample]
	accepted    atomic.Uint64
}

// NewIngestor creates an Ingestor backed by the given metrics database handle.
func NewIngestor(handle *db.Handle, opts IngestOptions) (*Ingestor, error) {
	if opts.MaxBatchSize <= 0 {
		opts.MaxBatchSize = 500
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 500 * time.Millisecond
	}
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = 4096
	}
	if opts.MaxSeries <= 0 {
		opts.MaxSeries = 10000
	}
	if opts.RingCapacity <= 0 {
		opts.RingCapacity = 180
	}

	registry := NewSeriesRegistry(handle, opts.MaxSeries)
	cache := NewHotCache(opts.MaxSeries, opts.RingCapacity)

	ing := &Ingestor{
		handle:   handle,
		registry: registry,
		cache:    cache,
	}

	batchCfg := db.BatchConfig{
		MaxBatchSize:  opts.MaxBatchSize,
		FlushInterval: opts.FlushInterval,
		QueueSize:     opts.QueueCapacity,
	}

	ing.batchWriter = db.NewBatchWriter(handle.Writer, batchCfg, ing.flushBatch)
	return ing, nil
}

// Registry returns the series registry.
func (ing *Ingestor) Registry() *SeriesRegistry {
	return ing.registry
}

// Cache returns the in-memory hot metric cache.
func (ing *Ingestor) Cache() *HotCache {
	return ing.cache
}

// Push records metric samples into the in-memory ring buffer immediately and enqueues them for SQLite persistence.
// Returns the number of successfully accepted samples.
func (ing *Ingestor) Push(samples ...model.MetricSample) int {
	if len(samples) == 0 {
		return 0
	}

	// 1. Immediate in-memory cache update
	ing.cache.Record(samples...)

	// 2. Enqueue into bounded BatchWriter queue
	accepted := 0
	for _, s := range samples {
		if err := ing.batchWriter.TryEnqueue(s); err == nil {
			accepted++
			ing.accepted.Add(1)
		}
	}
	return accepted
}

// flushBatch is invoked by BatchWriter within a single-writer transaction.
func (ing *Ingestor) flushBatch(ctx context.Context, tx *sql.Tx, items []model.MetricSample) error {
	if len(items) == 0 {
		return nil
	}

	// 1. Resolve series IDs for all samples
	resolved := make([]resolvedItem, 0, len(items))
	for _, item := range items {
		// Use fast registry lookup or create using existing tx
		seriesID, err := ing.registry.GetOrCreateWithTx(ctx, tx, item.Entity, item.Metric)
		if err != nil {
			// Skip samples exceeding maxSeries limit rather than aborting entire batch
			continue
		}
		resolved = append(resolved, resolvedItem{
			seriesID: seriesID,
			ts:       item.TS,
			value:    item.Value,
		})
	}

	if len(resolved) == 0 {
		return nil
	}

	// 2. Insert into sample_raw in chunks of up to 250 rows per statement
	const maxChunkRows = 250
	for i := 0; i < len(resolved); i += maxChunkRows {
		end := i + maxChunkRows
		if end > len(resolved) {
			end = len(resolved)
		}
		chunk := resolved[i:end]

		query, args := buildSampleRawInsert(chunk)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert sample_raw chunk (%d rows): %w", len(chunk), err)
		}
	}

	return nil
}

// Close gracefully closes the batch queue, flushes buffered samples to SQLite, and waits for completion.
func (ing *Ingestor) Close(ctx context.Context) error {
	return ing.batchWriter.Close(ctx)
}

// Stats returns a snapshot of ingestion throughput, batch flushes, and drops.
func (ing *Ingestor) Stats() IngestStats {
	bwStats := ing.batchWriter.Stats()
	return IngestStats{
		Accepted:    ing.accepted.Load(),
		Flushed:     bwStats.Flushed,
		Dropped:     bwStats.Dropped,
		SeriesCount: ing.registry.Count(),
	}
}

func buildSampleRawInsert(chunk []resolvedItem) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT OR REPLACE INTO sample_raw (series_id, ts, v) VALUES ")

	args := make([]any, 0, len(chunk)*3)
	for i, item := range chunk {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?)")
		args = append(args, item.seriesID, item.ts, item.value)
	}
	b.WriteString(";")
	return b.String(), args
}

type resolvedItem struct {
	seriesID int64
	ts       int64
	value    float64
}
