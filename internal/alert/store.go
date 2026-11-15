package alert

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
)

// ErrRuleNotFound is returned when an alert rule cannot be found by ID.
var ErrRuleNotFound = errors.New("alert rule not found")

// ErrRuleCapExceeded is returned when attempting to exceed the hard limit of 250 alert rules.
var ErrRuleCapExceeded = errors.New("cannot create rule: maximum limit of 250 rules reached")

// AlertEvent represents a recorded historical alert incident in config.db.
type AlertEvent struct {
	ID           int64     `json:"id"`
	RuleID       int64     `json:"rule_id"`
	RuleName     string    `json:"rule_name"`
	Target       string    `json:"target"`
	Severity     string    `json:"severity"`
	State        string    `json:"state"` // "firing" | "resolved"
	Started      int64     `json:"started"`
	Resolved     int64     `json:"resolved"`
	Summary      string    `json:"summary"`
	DetailsJSON  string    `json:"details_json"`
	NotifyStatus string    `json:"notify_status"`
	CreatedAt    time.Time `json:"created_at"`
}

// EventFilter provides query options for listing historical alert events.
type EventFilter struct {
	RuleID   int64
	Severity string
	State    string
	Cursor   int64 // Unix seconds cursor for created_at (< cursor)
	Limit    int   // 1..1000, default 200
}

// RuleStore provides database CRUD and query operations for alert rules and events in config.db.
type RuleStore struct {
	handle *db.Handle
}

// NewRuleStore initializes a RuleStore backed by the config.db handle.
func NewRuleStore(handle *db.Handle) *RuleStore {
	return &RuleStore{
		handle: handle,
	}
}

// ListRules retrieves all alert rules from config.db ordered by name.
func (s *RuleStore) ListRules(ctx context.Context) ([]AlertRule, error) {
	query := `
		SELECT id, name, kind, enabled, severity, target, condition_json, for_sec, cooldown_sec,
		       channels_json, notify_recovery, created_at, updated_at
		FROM alert_rule
		ORDER BY name ASC
	`
	rows, err := s.handle.QueryRO().QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query alert rules: %w", err)
	}
	defer rows.Close()

	var rules []AlertRule
	for rows.Next() {
		var r AlertRule
		var enabledInt, notifyInt int
		var channelsJSON string
		var createdAtSec, updatedAtSec int64

		if err := rows.Scan(
			&r.ID, &r.Name, &r.Kind, &enabledInt, &r.Severity, &r.Target, &r.ConditionJSON,
			&r.ForSec, &r.CooldownSec, &channelsJSON, &notifyInt, &createdAtSec, &updatedAtSec,
		); err != nil {
			return nil, fmt.Errorf("scan alert rule: %w", err)
		}

		r.Enabled = enabledInt == 1
		r.NotifyRecovery = notifyInt == 1
		r.CreatedAt = time.Unix(createdAtSec, 0).UTC()
		r.UpdatedAt = time.Unix(updatedAtSec, 0).UTC()

		if err := json.Unmarshal([]byte(channelsJSON), &r.Channels); err != nil {
			r.Channels = []string{}
		}

		rules = append(rules, r)
	}

	return rules, rows.Err()
}

// GetRule retrieves a single alert rule by ID.
func (s *RuleStore) GetRule(ctx context.Context, id int64) (AlertRule, error) {
	query := `
		SELECT id, name, kind, enabled, severity, target, condition_json, for_sec, cooldown_sec,
		       channels_json, notify_recovery, created_at, updated_at
		FROM alert_rule
		WHERE id = ?
	`
	var r AlertRule
	var enabledInt, notifyInt int
	var channelsJSON string
	var createdAtSec, updatedAtSec int64

	row := s.handle.QueryRO().QueryRowContext(ctx, query, id)
	err := row.Scan(
		&r.ID, &r.Name, &r.Kind, &enabledInt, &r.Severity, &r.Target, &r.ConditionJSON,
		&r.ForSec, &r.CooldownSec, &channelsJSON, &notifyInt, &createdAtSec, &updatedAtSec,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertRule{}, ErrRuleNotFound
	}
	if err != nil {
		return AlertRule{}, fmt.Errorf("get alert rule: %w", err)
	}

	r.Enabled = enabledInt == 1
	r.NotifyRecovery = notifyInt == 1
	r.CreatedAt = time.Unix(createdAtSec, 0).UTC()
	r.UpdatedAt = time.Unix(updatedAtSec, 0).UTC()

	if err := json.Unmarshal([]byte(channelsJSON), &r.Channels); err != nil {
		r.Channels = []string{}
	}

	return r, nil
}

// CreateRule validates and inserts a new alert rule, enforcing the 250 rule cap.
func (s *RuleStore) CreateRule(ctx context.Context, r AlertRule) (AlertRule, error) {
	compiled, err := CompileRule(r)
	if err != nil {
		return AlertRule{}, err
	}

	channelsBytes, err := json.Marshal(compiled.Channels)
	if err != nil {
		return AlertRule{}, fmt.Errorf("marshal channels: %w", err)
	}

	var created AlertRule
	writeErr := s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM alert_rule").Scan(&count); err != nil {
			return fmt.Errorf("count rules: %w", err)
		}
		if count >= MaxRulesCount {
			return ErrRuleCapExceeded
		}

		nowSec := time.Now().UTC().Unix()
		enabledInt := 0
		if compiled.Enabled {
			enabledInt = 1
		}
		notifyInt := 0
		if compiled.NotifyRecovery {
			notifyInt = 1
		}

		query := `
			INSERT INTO alert_rule (
				name, kind, enabled, severity, target, condition_json, for_sec, cooldown_sec,
				channels_json, notify_recovery, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`
		res, execErr := tx.ExecContext(ctx, query,
			compiled.Name, compiled.Kind, enabledInt, compiled.Severity, compiled.Target,
			compiled.ConditionJSON, compiled.ForSec, compiled.CooldownSec, string(channelsBytes),
			notifyInt, nowSec, nowSec,
		)
		if execErr != nil {
			return fmt.Errorf("insert alert rule: %w", execErr)
		}

		newID, idErr := res.LastInsertId()
		if idErr != nil {
			return fmt.Errorf("last insert id: %w", idErr)
		}

		created = compiled.AlertRule
		created.ID = newID
		created.Channels = compiled.Channels
		created.CreatedAt = time.Unix(nowSec, 0).UTC()
		created.UpdatedAt = time.Unix(nowSec, 0).UTC()
		return nil
	})

	if writeErr != nil {
		return AlertRule{}, writeErr
	}

	return created, nil
}

// UpdateRule validates and updates an existing alert rule.
func (s *RuleStore) UpdateRule(ctx context.Context, r AlertRule) (AlertRule, error) {
	if r.ID <= 0 {
		return AlertRule{}, errors.New("invalid rule ID")
	}

	compiled, err := CompileRule(r)
	if err != nil {
		return AlertRule{}, err
	}

	channelsBytes, err := json.Marshal(compiled.Channels)
	if err != nil {
		return AlertRule{}, fmt.Errorf("marshal channels: %w", err)
	}

	var updated AlertRule
	writeErr := s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM alert_rule WHERE id = ?", r.ID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRuleNotFound
			}
			return fmt.Errorf("check rule exists: %w", err)
		}

		nowSec := time.Now().UTC().Unix()
		enabledInt := 0
		if compiled.Enabled {
			enabledInt = 1
		}
		notifyInt := 0
		if compiled.NotifyRecovery {
			notifyInt = 1
		}

		query := `
			UPDATE alert_rule SET
				name = ?, kind = ?, enabled = ?, severity = ?, target = ?, condition_json = ?,
				for_sec = ?, cooldown_sec = ?, channels_json = ?, notify_recovery = ?, updated_at = ?
			WHERE id = ?
		`
		_, execErr := tx.ExecContext(ctx, query,
			compiled.Name, compiled.Kind, enabledInt, compiled.Severity, compiled.Target,
			compiled.ConditionJSON, compiled.ForSec, compiled.CooldownSec, string(channelsBytes),
			notifyInt, nowSec, r.ID,
		)
		if execErr != nil {
			return fmt.Errorf("update alert rule: %w", execErr)
		}

		// Reset stale state for this rule
		_, _ = tx.ExecContext(ctx, "DELETE FROM alert_rule_state WHERE rule_id = ?", r.ID)

		updated = compiled.AlertRule
		updated.ID = r.ID
		updated.Channels = compiled.Channels
		updated.UpdatedAt = time.Unix(nowSec, 0).UTC()
		return nil
	})

	if writeErr != nil {
		return AlertRule{}, writeErr
	}

	return updated, nil
}

// SetRuleEnabled toggles the enabled state of an alert rule.
func (s *RuleStore) SetRuleEnabled(ctx context.Context, id int64, enabled bool) error {
	return s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		enabledInt := 0
		if enabled {
			enabledInt = 1
		}
		nowSec := time.Now().UTC().Unix()

		res, err := tx.ExecContext(ctx,
			"UPDATE alert_rule SET enabled = ?, updated_at = ? WHERE id = ?",
			enabledInt, nowSec, id,
		)
		if err != nil {
			return fmt.Errorf("set rule enabled: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return ErrRuleNotFound
		}

		// Reset state when toggling
		_, _ = tx.ExecContext(ctx, "DELETE FROM alert_rule_state WHERE rule_id = ?", id)
		return nil
	})
}

// DeleteRule removes an alert rule and cleans up associated state from config.db.
func (s *RuleStore) DeleteRule(ctx context.Context, id int64) error {
	return s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM alert_rule WHERE id = ?", id)
		if err != nil {
			return fmt.Errorf("delete rule: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return ErrRuleNotFound
		}

		_, _ = tx.ExecContext(ctx, "DELETE FROM alert_rule_state WHERE rule_id = ?", id)
		return nil
	})
}

// InsertAlertEvent records a new alert event (firing) into alert_event.
func (s *RuleStore) InsertAlertEvent(ctx context.Context, ev AlertEvent) (int64, error) {
	var eventID int64
	err := s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		nowSec := ev.CreatedAt.UTC().Unix()
		if nowSec <= 0 {
			nowSec = time.Now().UTC().Unix()
		}

		query := `
			INSERT INTO alert_event (
				rule_id, rule_name, target, severity, state, started, resolved,
				summary, details_json, notify_status, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`
		res, execErr := tx.ExecContext(ctx, query,
			ev.RuleID, ev.RuleName, ev.Target, ev.Severity, ev.State,
			ev.Started, ev.Resolved, ev.Summary, ev.DetailsJSON, ev.NotifyStatus, nowSec,
		)
		if execErr != nil {
			return fmt.Errorf("insert alert event: %w", execErr)
		}

		var idErr error
		eventID, idErr = res.LastInsertId()
		return idErr
	})

	return eventID, err
}

// UpdateAlertEventResolved marks an alert event as resolved in config.db.
func (s *RuleStore) UpdateAlertEventResolved(ctx context.Context, id int64, resolvedAt int64) error {
	return s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE alert_event SET state = 'resolved', resolved = ? WHERE id = ?",
			resolvedAt, id,
		)
		return err
	})
}

// UpdateAlertEventNotifyStatus updates the delivery status of an alert event.
func (s *RuleStore) UpdateAlertEventNotifyStatus(ctx context.Context, id int64, status string) error {
	return s.handle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE alert_event SET notify_status = ? WHERE id = ?",
			status, id,
		)
		return err
	})
}

// ListAlertEvents queries historical alert incidents with keyset cursor pagination.
func (s *RuleStore) ListAlertEvents(ctx context.Context, filter EventFilter) ([]AlertEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	var whereClauses []string
	var args []any

	if filter.RuleID > 0 {
		whereClauses = append(whereClauses, "rule_id = ?")
		args = append(args, filter.RuleID)
	}
	if filter.Severity != "" {
		whereClauses = append(whereClauses, "severity = ?")
		args = append(args, strings.ToLower(filter.Severity))
	}
	if filter.State != "" {
		whereClauses = append(whereClauses, "state = ?")
		args = append(args, strings.ToLower(filter.State))
	}
	if filter.Cursor > 0 {
		whereClauses = append(whereClauses, "created_at < ?")
		args = append(args, filter.Cursor)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT id, rule_id, rule_name, target, severity, state, started, resolved,
		       summary, details_json, notify_status, created_at
		FROM alert_event
		%s
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, whereSQL)

	args = append(args, limit)

	rows, err := s.handle.QueryRO().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alert events: %w", err)
	}
	defer rows.Close()

	var events []AlertEvent
	for rows.Next() {
		var ev AlertEvent
		var createdAtSec int64

		if err := rows.Scan(
			&ev.ID, &ev.RuleID, &ev.RuleName, &ev.Target, &ev.Severity, &ev.State,
			&ev.Started, &ev.Resolved, &ev.Summary, &ev.DetailsJSON, &ev.NotifyStatus, &createdAtSec,
		); err != nil {
			return nil, fmt.Errorf("scan alert event: %w", err)
		}

		ev.CreatedAt = time.Unix(createdAtSec, 0).UTC()
		events = append(events, ev)
	}

	return events, rows.Err()
}
