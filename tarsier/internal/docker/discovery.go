package docker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
				restart_count = MAX(container.restart_count, excluded.restart_count);
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

		// 1. Mark any previously active containers whose IDs are absent from the active list as 'exited'
		activeIDs := make(map[string]bool)
		for _, c := range containers {
			st := strings.ToLower(strings.TrimSpace(c.State))
			if st == "running" || st == "restarting" {
				activeIDs[c.ID] = true
			}
		}

		rows, err := tx.QueryContext(ctx, `SELECT id, name FROM container WHERE last_status IN ('running', 'restarting');`)
		if err == nil {
			var staleIDs []string
			for rows.Next() {
				var sid, sname string
				if err := rows.Scan(&sid, &sname); err == nil {
					if !activeIDs[sid] {
						staleIDs = append(staleIDs, sid)
					}
				}
			}
			rows.Close()

			for _, sid := range staleIDs {
				_, _ = tx.ExecContext(ctx, `UPDATE container SET last_status = 'exited', last_seen = ? WHERE id = ?;`, now, sid)
			}
		}

		// 2. For each container in the current sync list, supersede older records with the same name if active
		stmtSupersede, err := tx.PrepareContext(ctx, `
			UPDATE container SET last_status = 'exited'
			WHERE (name = ? OR name = ('/' || ?) OR TRIM(name, '/') = ?)
			  AND id != ? AND last_status IN ('running', 'restarting');
		`)
		if err != nil {
			return fmt.Errorf("prepare container supersede: %w", err)
		}
		defer stmtSupersede.Close()

		for _, c := range containers {
			c.Name = strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
			c.Image = strings.TrimPrefix(strings.TrimSpace(c.Image), "/")
			state := strings.ToLower(strings.TrimSpace(c.State))
			if (state == "running" || state == "restarting") && c.Name != "" {
				if _, err := stmtSupersede.ExecContext(ctx, c.Name, c.Name, c.Name, c.ID); err != nil {
					return fmt.Errorf("exec container supersede (%s): %w", c.Name, err)
				}
			}

			if _, err := stmtContainer.ExecContext(ctx, c.ID, c.Name, c.Image, now, now, state, c.RestartCount); err != nil {
				return fmt.Errorf("exec container upsert (%s): %w", c.ID, err)
			}

			if _, err := stmtSource.ExecContext(ctx, c.Name, now, now); err != nil {
				return fmt.Errorf("exec source upsert (%s): %w", c.Name, err)
			}
		}

		// 3. Prune exited, dead, or destroyed containers older than 24 hours along with their lifecycle events
		cutoffTS := now - 24*3600
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM container_event
			WHERE container_id IN (
				SELECT id FROM container
				WHERE last_status IN ('exited', 'dead', 'destroyed') AND last_seen < ?
			);
		`, cutoffTS); err != nil {
			return fmt.Errorf("prune expired container events: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM container
			WHERE last_status IN ('exited', 'dead', 'destroyed') AND last_seen < ?;
		`, cutoffTS); err != nil {
			return fmt.Errorf("prune expired containers: %w", err)
		}

		return nil
	})
}
