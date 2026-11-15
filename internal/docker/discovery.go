package docker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// SyncContainers persists discovered container metadata and source mappings atomically into config.db.
// It enforces the maxSources upper bound to prevent ephemeral containers from exhausting resources.
func SyncContainers(ctx context.Context, configDB *db.Handle, containers []ContainerInfo, maxSources int) error {
	if configDB == nil {
		return fmt.Errorf("config db handle is required")
	}

	if maxSources <= 0 {
		maxSources = 500
	}
	if len(containers) > maxSources {
		containers = containers[:maxSources]
	}

	now := time.Now().Unix()

	return configDB.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stmtContainer, err := tx.PrepareContext(ctx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status, restart_count)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name          = excluded.name,
				image         = excluded.image,
				last_seen     = excluded.last_seen,
				last_status   = excluded.last_status,
				restart_count = excluded.restart_count;
		`)
		if err != nil {
			return fmt.Errorf("prepare container upsert: %w", err)
		}
		defer stmtContainer.Close()

		stmtSource, err := tx.PrepareContext(ctx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES ('container', ?, 1, ?, ?)
			ON CONFLICT(kind, name) DO UPDATE SET
				last_seen = excluded.last_seen;
		`)
		if err != nil {
			return fmt.Errorf("prepare source upsert: %w", err)
		}
		defer stmtSource.Close()

		for _, c := range containers {
			if _, err := stmtContainer.ExecContext(ctx, c.ID, c.Name, c.Image, now, now, c.State, c.RestartCount); err != nil {
				return fmt.Errorf("exec container upsert (%s): %w", c.ID, err)
			}

			if _, err := stmtSource.ExecContext(ctx, c.Name, now, now); err != nil {
				return fmt.Errorf("exec source upsert (%s): %w", c.Name, err)
			}
		}

		return nil
	})
}
