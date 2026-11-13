package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const (
	// ConfigWriterQueueSize is the bounded queue capacity for config.db write jobs.
	ConfigWriterQueueSize = 256

	// MetricsWriterQueueSize is the bounded queue capacity for metrics.db write jobs.
	MetricsWriterQueueSize = 2048
)

// Handle wraps the isolated read-write and read-only connection handles and single-writer worker
// for a single SQLite database.
type Handle struct {
	Name   string
	Path   string
	RW     *sql.DB // Dedicated single-writer handle (SetMaxOpenConns(1))
	RO     *sql.DB // Read-only concurrent query pool (SetMaxOpenConns(2))
	Writer *Writer // Dedicated single-writer queue worker
}

// ExecWrite executes a write transaction synchronously through the dedicated single-writer queue.
func (h *Handle) ExecWrite(ctx context.Context, fn WriteFunc) error {
	if h.Writer == nil {
		return errors.New("writer not initialized")
	}
	return h.Writer.ExecSync(ctx, fn)
}

// QueryRO returns the isolated read-only connection pool.
func (h *Handle) QueryRO() *sql.DB {
	return h.RO
}

// Checkpoint triggers a passive WAL checkpoint on the RW connection.
func (h *Handle) Checkpoint(ctx context.Context) error {
	if h.RW == nil {
		return nil
	}
	if _, err := h.RW.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);"); err != nil {
		return fmt.Errorf("wal checkpoint %s: %w", h.Name, err)
	}
	return nil
}

// Close gracefully drains the Writer, checkpoints WAL, and cleanly closes both RO and RW handles.
func (h *Handle) Close(ctx ...context.Context) error {
	c := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		c = ctx[0]
	}

	var errs []error

	// Close and drain Writer first before checkpointing and closing sql.DB handles
	if h.Writer != nil {
		if err := h.Writer.Close(c); err != nil {
			errs = append(errs, fmt.Errorf("close %s writer: %w", h.Name, err))
		}
	}

	// Passive WAL checkpoint on best-effort basis before closing
	if h.RW != nil {
		_ = h.Checkpoint(c)
	}

	if h.RO != nil {
		if err := h.RO.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s RO: %w", h.Name, err))
		}
	}

	if h.RW != nil {
		if err := h.RW.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s RW: %w", h.Name, err))
		}
	}

	return errors.Join(errs...)
}

// Store encapsulates the two isolated SQLite databases required by Tarsier: config.db and metrics.db.
type Store struct {
	Config  *Handle // <storage.path>/config.db
	Metrics *Handle // <storage.path>/metrics.db
}

// Close drains all writers, checkpoints WAL, and cleanly closes all RO and RW handles across both databases.
func (s *Store) Close(ctx ...context.Context) error {
	var errs []error
	if s.Config != nil {
		if err := s.Config.Close(ctx...); err != nil {
			errs = append(errs, err)
		}
	}
	if s.Metrics != nil {
		if err := s.Metrics.Close(ctx...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Open initializes and configures the two SQLite databases (config.db and metrics.db) inside dataDir.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory %q: %w", dataDir, err)
	}

	configHandle, err := openHandle(ctx, filepath.Join(dataDir, "config.db"), "config", ConfigCacheSizeKiB, ConfigWriterQueueSize, ConfigSchemaDDL)
	if err != nil {
		return nil, fmt.Errorf("open config.db: %w", err)
	}

	metricsHandle, err := openHandle(ctx, filepath.Join(dataDir, "metrics.db"), "metrics", MetricsCacheSizeKiB, MetricsWriterQueueSize, MetricsSchemaDDL)
	if err != nil {
		_ = configHandle.Close()
		return nil, fmt.Errorf("open metrics.db: %w", err)
	}

	return &Store{
		Config:  configHandle,
		Metrics: metricsHandle,
	}, nil
}

func openHandle(ctx context.Context, dbPath string, name string, cacheSizeKiB int64, queueSize int, schema []string) (*Handle, error) {
	rwDSN := buildDSN(dbPath, false, cacheSizeKiB)
	rw, err := sql.Open("sqlite", rwDSN)
	if err != nil {
		return nil, fmt.Errorf("open RW connection: %w", err)
	}

	// Single dedicated writer connection
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	rw.SetConnMaxLifetime(0)

	// Ensure incremental vacuum is permanently set in database header before table creation
	var currentAutoVacuum int
	if err := rw.QueryRowContext(ctx, "PRAGMA auto_vacuum;").Scan(&currentAutoVacuum); err == nil && currentAutoVacuum != 2 {
		if _, err := rw.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL; VACUUM;"); err != nil {
			_ = rw.Close()
			return nil, fmt.Errorf("set auto_vacuum: %w", err)
		}
	}

	// Run idempotent migrations via RW connection
	if err := migrate(ctx, rw, schema); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	// Open read-only pool
	roDSN := buildDSN(dbPath, true, cacheSizeKiB)
	ro, err := sql.Open("sqlite", roDSN)
	if err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("open RO connection: %w", err)
	}

	// Bounded concurrent read pool
	ro.SetMaxOpenConns(2)
	ro.SetMaxIdleConns(2)
	ro.SetConnMaxLifetime(0)

	// Dedicated single-writer queue worker pinned to rw handle
	writer := NewWriter(rw, queueSize)

	return &Handle{
		Name:   name,
		Path:   dbPath,
		RW:     rw,
		RO:     ro,
		Writer: writer,
	}, nil
}
