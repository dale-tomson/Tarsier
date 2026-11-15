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

		// 1. Mark any previously running containers whose IDs are absent from the active running list as 'exited'
		activeRunningIDs := make(map[string]bool)
		for _, c := range containers {
			if c.State == "running" {
				activeRunningIDs[c.ID] = true
			}
		}

		rows, err := tx.QueryContext(ctx, `SELECT id, name FROM container WHERE last_status = 'running';`)
		if err == nil {
			var staleIDs []string
			for rows.Next() {
				var sid, sname string
				if err := rows.Scan(&sid, &sname); err == nil {
					if !activeRunningIDs[sid] {
						staleIDs = append(staleIDs, sid)
					}
				}
			}
			rows.Close()

			for _, sid := range staleIDs {
				_, _ = tx.ExecContext(ctx, `UPDATE container SET last_status = 'exited', last_seen = ? WHERE id = ?;`, now, sid)
			}
		}

		// 2. For each container in the current sync list, supersede older records with the same name if running
		stmtSupersede, err := tx.PrepareContext(ctx, `
			UPDATE container SET last_status = 'exited'
			WHERE name = ? AND id != ? AND last_status = 'running';
		`)
		if err != nil {
			return fmt.Errorf("prepare container supersede: %w", err)
		}
		defer stmtSupersede.Close()

		for _, c := range containers {
			if c.State == "running" && c.Name != "" {
				if _, err := stmtSupersede.ExecContext(ctx, c.Name, c.ID); err != nil {
					return fmt.Errorf("exec container supersede (%s): %w", c.Name, err)
				}
			}

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
