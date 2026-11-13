package db

import (
	"context"
	"database/sql"
	"fmt"
)

// ConfigSchemaDDL contains idempotent DDL statements for config.db.
var ConfigSchemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS source (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		name TEXT NOT NULL,
		host_id INTEGER NOT NULL DEFAULT 1,
		first_seen INTEGER NOT NULL,
		last_seen INTEGER NOT NULL,
		UNIQUE(kind, name)
	);`,

	`CREATE TABLE IF NOT EXISTS source_offset (
		source_id INTEGER PRIMARY KEY,
		inode INTEGER NOT NULL DEFAULT 0,
		offset INTEGER NOT NULL DEFAULT 0,
		last_ts INTEGER NOT NULL DEFAULT 0
	);`,

	`CREATE TABLE IF NOT EXISTS container (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		image TEXT NOT NULL,
		first_seen INTEGER NOT NULL,
		last_seen INTEGER NOT NULL,
		last_status TEXT NOT NULL,
		restart_count INTEGER NOT NULL DEFAULT 0
	);`,

	`CREATE TABLE IF NOT EXISTS container_event (
		id INTEGER PRIMARY KEY,
		container_id TEXT NOT NULL,
		ts INTEGER NOT NULL,
		type TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);`,

	`CREATE INDEX IF NOT EXISTS idx_container_event_ts ON container_event(container_id, ts DESC);`,

	`CREATE TABLE IF NOT EXISTS alert_rule (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		severity TEXT NOT NULL,
		spec_json TEXT NOT NULL,
		channels TEXT NOT NULL,
		cooldown_s INTEGER NOT NULL DEFAULT 300,
		notify_recovery INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS alert_rule_state (
		rule_id INTEGER NOT NULL,
		target TEXT NOT NULL,
		state TEXT NOT NULL,
		since INTEGER NOT NULL,
		last_fired INTEGER NOT NULL DEFAULT 0,
		last_notified INTEGER NOT NULL DEFAULT 0,
		value REAL NOT NULL DEFAULT 0,
		PRIMARY KEY(rule_id, target)
	) WITHOUT ROWID;`,

	`CREATE TABLE IF NOT EXISTS alert_event (
		id INTEGER PRIMARY KEY,
		rule_id INTEGER NOT NULL,
		target TEXT NOT NULL,
		severity TEXT NOT NULL,
		state TEXT NOT NULL,
		started INTEGER NOT NULL,
		resolved INTEGER NOT NULL DEFAULT 0,
		summary TEXT NOT NULL,
		details_json TEXT NOT NULL DEFAULT '{}',
		notify_status TEXT NOT NULL DEFAULT 'pending'
	);`,

	`CREATE INDEX IF NOT EXISTS idx_alert_event_started ON alert_event(started DESC);`,

	`CREATE TABLE IF NOT EXISTS cleanup_history (
		id INTEGER PRIMARY KEY,
		started_at INTEGER NOT NULL,
		finished_at INTEGER NOT NULL,
		dry_run INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		deleted_metrics INTEGER NOT NULL DEFAULT 0,
		deleted_chunks INTEGER NOT NULL DEFAULT 0,
		freed_bytes INTEGER NOT NULL DEFAULT 0,
		report_format TEXT NOT NULL DEFAULT '',
		report_body TEXT NOT NULL DEFAULT ''
	);`,

	`CREATE TABLE IF NOT EXISTS session (
		id TEXT PRIMARY KEY,
		expires INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS kv (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) WITHOUT ROWID;`,
}

// MetricsSchemaDDL contains idempotent DDL statements for metrics.db.
var MetricsSchemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS series (
		id INTEGER PRIMARY KEY,
		entity TEXT NOT NULL,
		metric TEXT NOT NULL,
		UNIQUE(entity, metric)
	);`,

	`CREATE TABLE IF NOT EXISTS sample_raw (
		series_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		v REAL NOT NULL,
		PRIMARY KEY(series_id, ts)
	) WITHOUT ROWID;`,

	`CREATE TABLE IF NOT EXISTS sample_1m (
		series_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		avg REAL NOT NULL,
		min REAL NOT NULL,
		max REAL NOT NULL,
		PRIMARY KEY(series_id, ts)
	) WITHOUT ROWID;`,

	`CREATE TABLE IF NOT EXISTS sample_5m (
		series_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		avg REAL NOT NULL,
		min REAL NOT NULL,
		max REAL NOT NULL,
		PRIMARY KEY(series_id, ts)
	) WITHOUT ROWID;`,

	`CREATE TABLE IF NOT EXISTS sample_1d (
		series_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		avg REAL NOT NULL,
		min REAL NOT NULL,
		max REAL NOT NULL,
		PRIMARY KEY(series_id, ts)
	) WITHOUT ROWID;`,
}

// migrate executes DDL statements sequentially against db.
func migrate(ctx context.Context, db *sql.DB, statements []string) error {
	for i, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("execute migration statement %d: %w", i, err)
		}
	}
	return nil
}
