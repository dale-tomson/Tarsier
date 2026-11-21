package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	// BusyTimeoutMS is the SQLite busy timeout in milliseconds.
	BusyTimeoutMS = 5000

	// ConfigCacheSizeKiB is the memory cap for config.db page cache (~2 MiB).
	ConfigCacheSizeKiB = -2000

	// MetricsCacheSizeKiB is the memory cap for metrics.db page cache (~4 MiB).
	MetricsCacheSizeKiB = -4000
)

// PragmaSettings holds the runtime PRAGMA values queried from an active database connection.
type PragmaSettings struct {
	JournalMode string
	Synchronous int
	BusyTimeout int
	ForeignKeys int
	TempStore   int
	MmapSize    int64
	AutoVacuum  int
	CacheSize   int64
}

// buildDSN constructs an SQLite URI connection string with mandatory PRAGMAs configured.
func buildDSN(dbPath string, readOnly bool, cacheSizeKiB int64) string {
	q := url.Values{}

	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rwc")
		q.Add("_pragma", "journal_mode=WAL")
	}

	q.Add("_pragma", fmt.Sprintf("busy_timeout=%d", BusyTimeoutMS))
	q.Add("_pragma", "synchronous=NORMAL")
	q.Add("_pragma", "foreign_keys=ON")
	q.Add("_pragma", "temp_store=MEMORY")
	q.Add("_pragma", "mmap_size=0")
	q.Add("_pragma", fmt.Sprintf("cache_size=%d", cacheSizeKiB))

	return fmt.Sprintf("file:%s?%s", dbPath, q.Encode())
}

// VerifyPragmas queries and returns runtime PRAGMA values for the given database handle.
func VerifyPragmas(ctx context.Context, db *sql.DB) (PragmaSettings, error) {
	var settings PragmaSettings

	row := db.QueryRowContext(ctx, "PRAGMA journal_mode;")
	if err := row.Scan(&settings.JournalMode); err != nil {
		return settings, fmt.Errorf("query journal_mode: %w", err)
	}
	settings.JournalMode = strings.ToLower(settings.JournalMode)

	row = db.QueryRowContext(ctx, "PRAGMA synchronous;")
	if err := row.Scan(&settings.Synchronous); err != nil {
		return settings, fmt.Errorf("query synchronous: %w", err)
	}

	row = db.QueryRowContext(ctx, "PRAGMA busy_timeout;")
	if err := row.Scan(&settings.BusyTimeout); err != nil {
		return settings, fmt.Errorf("query busy_timeout: %w", err)
	}

	row = db.QueryRowContext(ctx, "PRAGMA foreign_keys;")
	if err := row.Scan(&settings.ForeignKeys); err != nil {
		return settings, fmt.Errorf("query foreign_keys: %w", err)
	}

	row = db.QueryRowContext(ctx, "PRAGMA temp_store;")
	if err := row.Scan(&settings.TempStore); err != nil {
		return settings, fmt.Errorf("query temp_store: %w", err)
	}

	row = db.QueryRowContext(ctx, "PRAGMA mmap_size;")
	if err := row.Scan(&settings.MmapSize); err != nil {
		return settings, fmt.Errorf("query mmap_size: %w", err)
	}

	row = db.QueryRowContext(ctx, "PRAGMA auto_vacuum;")
	if err := row.Scan(&settings.AutoVacuum); err != nil {
		return settings, fmt.Errorf("query auto_vacuum: %w", err)
	}

	var rawCache interface{}
	row = db.QueryRowContext(ctx, "PRAGMA cache_size;")
	if err := row.Scan(&rawCache); err != nil {
		return settings, fmt.Errorf("query cache_size: %w", err)
	}
	switch v := rawCache.(type) {
	case int64:
		settings.CacheSize = v
	case int32:
		settings.CacheSize = int64(v)
	case int:
		settings.CacheSize = int64(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return settings, fmt.Errorf("parse cache_size string %q: %w", v, err)
		}
		settings.CacheSize = n
	default:
		return settings, fmt.Errorf("unexpected type %T for cache_size: %v", rawCache, rawCache)
	}

	return settings, nil
}
