package db_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

func TestStore_OpenAndIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	}()

	// 1. Verify config.db and metrics.db exist on disk
	configPath := filepath.Join(tmpDir, "config.db")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("expected config.db to exist at %s", configPath)
	}

	metricsPath := filepath.Join(tmpDir, "metrics.db")
	if _, err := os.Stat(metricsPath); os.IsNotExist(err) {
		t.Fatalf("expected metrics.db to exist at %s", metricsPath)
	}

	// 2. Verify logs.db does NOT exist
	logsPath := filepath.Join(tmpDir, "logs.db")
	if _, err := os.Stat(logsPath); !os.IsNotExist(err) {
		t.Fatalf("logs.db must NOT exist, but found at %s", logsPath)
	}

	// 3. Verify config.db tables
	expectedConfigTables := []string{
		"source",
		"source_offset",
		"container",
		"container_event",
		"alert_rule",
		"alert_rule_state",
		"alert_event",
		"cleanup_history",
		"session",
		"kv",
	}
	for _, tbl := range expectedConfigTables {
		assertTableExists(t, store.Config.RO, tbl)
	}

	// 4. Verify metrics.db tables
	expectedMetricsTables := []string{
		"series",
		"sample_raw",
		"sample_1m",
		"sample_5m",
		"sample_1d",
	}
	for _, tbl := range expectedMetricsTables {
		assertTableExists(t, store.Metrics.RO, tbl)
	}

	// 5. Verify no FTS tables were created in either DB
	assertNoTableMatches(t, store.Config.RO, "%fts%")
	assertNoTableMatches(t, store.Metrics.RO, "%fts%")
}

func TestStore_PragmasEnforced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	testCases := []struct {
		name          string
		handle        *sql.DB
		expectedCache int64
	}{
		{"Config.RW", store.Config.RW, db.ConfigCacheSizeKiB},
		{"Config.RO", store.Config.RO, db.ConfigCacheSizeKiB},
		{"Metrics.RW", store.Metrics.RW, db.MetricsCacheSizeKiB},
		{"Metrics.RO", store.Metrics.RO, db.MetricsCacheSizeKiB},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pragmas, err := db.VerifyPragmas(ctx, tc.handle)
			if err != nil {
				t.Fatalf("VerifyPragmas failed: %v", err)
			}

			if pragmas.JournalMode != "wal" {
				t.Errorf("expected journal_mode 'wal', got %q", pragmas.JournalMode)
			}
			if pragmas.Synchronous != 1 { // 1 = NORMAL
				t.Errorf("expected synchronous 1 (NORMAL), got %d", pragmas.Synchronous)
			}
			if pragmas.BusyTimeout != db.BusyTimeoutMS {
				t.Errorf("expected busy_timeout %d, got %d", db.BusyTimeoutMS, pragmas.BusyTimeout)
			}
			if pragmas.ForeignKeys != 1 { // 1 = ON
				t.Errorf("expected foreign_keys 1 (ON), got %d", pragmas.ForeignKeys)
			}
			if pragmas.TempStore != 2 { // 2 = MEMORY
				t.Errorf("expected temp_store 2 (MEMORY), got %d", pragmas.TempStore)
			}
			if pragmas.MmapSize != 0 {
				t.Errorf("expected mmap_size 0, got %d", pragmas.MmapSize)
			}
			if pragmas.AutoVacuum != 2 { // 2 = INCREMENTAL
				t.Errorf("expected auto_vacuum 2 (INCREMENTAL), got %d", pragmas.AutoVacuum)
			}
			if pragmas.CacheSize != tc.expectedCache {
				t.Errorf("expected cache_size %d, got %d", tc.expectedCache, pragmas.CacheSize)
			}
		})
	}
}

func TestStore_ReadOnlyEnforcement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// 1. Attempt write via Config.RO
	_, err = store.Config.RO.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES ('test_key', 'test_val');")
	if err == nil {
		t.Fatalf("expected write to Config.RO to fail, but succeeded")
	}
	if !isReadOnlyError(err) {
		t.Errorf("expected read-only error on Config.RO, got: %v", err)
	}

	// 2. Attempt write via Metrics.RO
	_, err = store.Metrics.RO.ExecContext(ctx, "INSERT INTO series (entity, metric) VALUES ('host', 'cpu_pct');")
	if err == nil {
		t.Fatalf("expected write to Metrics.RO to fail, but succeeded")
	}
	if !isReadOnlyError(err) {
		t.Errorf("expected read-only error on Metrics.RO, got: %v", err)
	}

	// 3. Attempt schema modification via RO
	_, err = store.Config.RO.ExecContext(ctx, "CREATE TABLE ro_trap (id INT);")
	if err == nil {
		t.Fatalf("expected DDL on Config.RO to fail, but succeeded")
	}
	if !isReadOnlyError(err) {
		t.Errorf("expected read-only error on Config.RO DDL, got: %v", err)
	}
}

func TestStore_IdempotentReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()

	// Initial open and write
	store1, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}

	_, err = store1.Config.RW.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES ('version', '1.0');")
	if err != nil {
		t.Fatalf("insert into kv failed: %v", err)
	}

	_, err = store1.Metrics.RW.ExecContext(ctx, "INSERT INTO series (id, entity, metric) VALUES (1, 'host', 'cpu_pct');")
	if err != nil {
		t.Fatalf("insert into series failed: %v", err)
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}

	// Reopen on the same directory
	store2, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer store2.Close()

	// Verify data persisted and readable from both RW and RO
	var val string
	if err := store2.Config.RO.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = 'version';").Scan(&val); err != nil {
		t.Fatalf("read kv from RO failed: %v", err)
	}
	if val != "1.0" {
		t.Errorf("expected '1.0', got %q", val)
	}

	var entity, metric string
	if err := store2.Metrics.RO.QueryRowContext(ctx, "SELECT entity, metric FROM series WHERE id = 1;").Scan(&entity, &metric); err != nil {
		t.Fatalf("read series from RO failed: %v", err)
	}
	if entity != "host" || metric != "cpu_pct" {
		t.Errorf("expected host/cpu_pct, got %s/%s", entity, metric)
	}

	// Verify PRAGMAs remain intact after reopen
	pragmas, err := db.VerifyPragmas(ctx, store2.Metrics.RW)
	if err != nil {
		t.Fatalf("VerifyPragmas after reopen failed: %v", err)
	}
	if pragmas.AutoVacuum != 2 {
		t.Errorf("expected auto_vacuum 2 (INCREMENTAL) after reopen, got %d", pragmas.AutoVacuum)
	}
	if pragmas.JournalMode != "wal" {
		t.Errorf("expected journal_mode wal after reopen, got %q", pragmas.JournalMode)
	}
}

func TestStore_PoolLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Check RW stats: MaxOpenConnections should be 1
	rwStats := store.Config.RW.Stats()
	if rwStats.MaxOpenConnections != 1 {
		t.Errorf("expected Config.RW MaxOpenConnections=1, got %d", rwStats.MaxOpenConnections)
	}

	// Check RO stats: MaxOpenConnections should be 2
	roStats := store.Config.RO.Stats()
	if roStats.MaxOpenConnections != 2 {
		t.Errorf("expected Config.RO MaxOpenConnections=2, got %d", roStats.MaxOpenConnections)
	}
}

// Helpers

func assertTableExists(t *testing.T, db *sql.DB, tableName string) {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?;", tableName).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query sqlite_master for table %s: %v", tableName, err)
	}
	if count != 1 {
		t.Errorf("expected table %q to exist, but not found", tableName)
	}
}

func assertNoTableMatches(t *testing.T, db *sql.DB, pattern string) {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE ?;", pattern).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query sqlite_master for pattern %s: %v", pattern, err)
	}
	if count != 0 {
		t.Errorf("expected 0 tables matching %q, but found %d", pattern, count)
	}
}

func isReadOnlyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "readonly") ||
		strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "attempt to write a readonly database")
}
