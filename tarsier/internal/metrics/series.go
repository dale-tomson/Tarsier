package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/dale-tomson/Tarsier/internal/db"
)

var (
	// ErrMaxSeriesExceeded is returned when the series registry reaches its configured maximum capacity.
	ErrMaxSeriesExceeded = errors.New("maximum series limit exceeded")
)

// SeriesKey uniquely identifies a telemetry time series by entity and metric name.
type SeriesKey struct {
	Entity string
	Metric string
}

// SeriesRegistry provides a thread-safe in-memory cache and SQLite persistence layer for series IDs.
type SeriesRegistry struct {
	handle    *db.Handle
	mu        sync.RWMutex
	series    map[SeriesKey]int64
	maxSeries int
	dropped   atomic.Uint64
}

// NewSeriesRegistry constructs a SeriesRegistry backed by the given metrics database handle.
func NewSeriesRegistry(handle *db.Handle, maxSeries int) *SeriesRegistry {
	if maxSeries <= 0 {
		maxSeries = 10000
	}
	return &SeriesRegistry{
		handle:    handle,
		series:    make(map[SeriesKey]int64),
		maxSeries: maxSeries,
	}
}

// LoadSeries warms the in-memory cache at startup from the SQLite series table using the read-only connection.
func (r *SeriesRegistry) LoadSeries(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rows, err := r.handle.RO.QueryContext(ctx, "SELECT id, entity, metric FROM series;")
	if err != nil {
		return fmt.Errorf("load series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var entity, metric string
		if err := rows.Scan(&id, &entity, &metric); err != nil {
			return fmt.Errorf("scan series: %w", err)
		}
		r.series[SeriesKey{Entity: entity, Metric: metric}] = id
	}

	return rows.Err()
}

// Lookup returns the series ID if already cached without acquiring a write lock or touching SQLite.
func (r *SeriesRegistry) Lookup(entity, metric string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.series[SeriesKey{Entity: entity, Metric: metric}]
	return id, ok
}

// GetOrCreate returns the series ID from cache or creates a new row in metrics.db inside a single-writer transaction.
func (r *SeriesRegistry) GetOrCreate(ctx context.Context, entity, metric string) (int64, error) {
	key := SeriesKey{Entity: entity, Metric: metric}

	// Fast path: RLock
	r.mu.RLock()
	if id, ok := r.series[key]; ok {
		r.mu.RUnlock()
		return id, nil
	}
	r.mu.RUnlock()

	var newID int64
	err := r.handle.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		var txErr error
		newID, txErr = r.GetOrCreateWithTx(txCtx, tx, entity, metric)
		return txErr
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// GetOrCreateWithTx resolves or creates a series ID within an existing SQLite transaction.
func (r *SeriesRegistry) GetOrCreateWithTx(ctx context.Context, tx *sql.Tx, entity, metric string) (int64, error) {
	key := SeriesKey{Entity: entity, Metric: metric}

	// Fast path: RLock
	r.mu.RLock()
	if id, ok := r.series[key]; ok {
		r.mu.RUnlock()
		return id, nil
	}
	r.mu.RUnlock()

	// Slow path: Lock + double-check + insert using existing tx
	r.mu.Lock()
	defer r.mu.Unlock()

	if id, ok := r.series[key]; ok {
		return id, nil
	}

	if len(r.series) >= r.maxSeries {
		r.dropped.Add(1)
		return 0, ErrMaxSeriesExceeded
	}

	var newID int64
	query := `INSERT INTO series (entity, metric) VALUES (?, ?)
ON CONFLICT(entity, metric) DO UPDATE SET entity = excluded.entity
RETURNING id;`
	if err := tx.QueryRowContext(ctx, query, entity, metric).Scan(&newID); err != nil {
		return 0, fmt.Errorf("insert series (%s, %s): %w", entity, metric, err)
	}

	r.series[key] = newID
	return newID, nil
}

// Count returns the number of active series cached in memory.
func (r *SeriesRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.series)
}

// Dropped returns the total number of series rejected due to hitting maxSeries capacity.
func (r *SeriesRegistry) Dropped() uint64 {
	return r.dropped.Load()
}
