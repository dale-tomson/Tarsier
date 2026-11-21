package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// getInode extracts the underlying filesystem inode from os.FileInfo on Unix systems.
func getInode(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	sys := info.Sys()
	if sys == nil {
		return 0
	}
	if stat, ok := sys.(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}
	return 0
}

// EnsureSource registers or retrieves the integer source ID for a file from config.db.
func EnsureSource(ctx context.Context, configDB *db.Handle, kind, name string) (int64, error) {
	if configDB == nil {
		return 0, errors.New("configDB handle is required")
	}
	now := time.Now().Unix()
	var id int64

	// 1. Fast read from RO pool
	err := configDB.RO.QueryRowContext(ctx, "SELECT id FROM source WHERE kind = ? AND name = ?", kind, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("query source id: %w", err)
	}

	// 2. Insert into source table via SingleWriter
	err = configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, writeErr := tx.ExecContext(ctx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES (?, ?, 1, ?, ?)
			ON CONFLICT(kind, name) DO UPDATE SET last_seen = excluded.last_seen;
		`, kind, name, now, now)
		return writeErr
	})
	if err != nil {
		return 0, fmt.Errorf("insert source: %w", err)
	}

	// 3. Read back assigned ID
	err = configDB.RO.QueryRowContext(ctx, "SELECT id FROM source WHERE kind = ? AND name = ?", kind, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("readback source id: %w", err)
	}
	return id, nil
}

// GetFileSourceOffset loads the persisted (inode, offset, last_ts) cursor for a source from config.db.
func GetFileSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64) (inode uint64, offset int64, lastTS int64, err error) {
	if configDB == nil {
		return 0, 0, 0, errors.New("configDB handle is required")
	}
	var inVal, offVal, tsVal int64
	qErr := configDB.RO.QueryRowContext(ctx, "SELECT inode, offset, last_ts FROM source_offset WHERE source_id = ?", sourceID).Scan(&inVal, &offVal, &tsVal)
	if errors.Is(qErr, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if qErr != nil {
		return 0, 0, 0, fmt.Errorf("query file source offset: %w", qErr)
	}
	return uint64(inVal), offVal, tsVal, nil
}

// SaveFileSourceOffset persists the (inode, offset, last_ts) cursor for a source in config.db.
func SaveFileSourceOffset(ctx context.Context, configDB *db.Handle, sourceID int64, inode uint64, offset int64, lastTS int64) error {
	if configDB == nil {
		return errors.New("configDB handle is required")
	}
	return configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO source_offset (source_id, inode, offset, last_ts)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(source_id) DO UPDATE SET
				inode   = excluded.inode,
				offset  = excluded.offset,
				last_ts = excluded.last_ts;
		`, sourceID, int64(inode), offset, lastTS)
		return err
	})
}

