package alert

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// AlertState defines the lifecycle state of a rule target.
type AlertState string

const (
	StateOK       AlertState = "ok"
	StatePending  AlertState = "pending"
	StateFiring   AlertState = "firing"
	StateResolved AlertState = "resolved"
)

// StateKey identifies a tracked alert state uniquely by rule ID and target.
type StateKey struct {
	RuleID int64
	Target string
}

// RuleTargetState encapsulates the live in-memory state of an alert rule target.
type RuleTargetState struct {
	RuleID        int64             `json:"rule_id"`
	RuleName      string            `json:"rule_name"`
	Target        string            `json:"target"`
	State         AlertState        `json:"state"`
	Severity      string            `json:"severity"`
	Since         int64             `json:"since"`
	LastFired     int64             `json:"last_fired"`
	LastNotified  int64             `json:"last_notified"`
	NotifiedSev   string            `json:"notified_sev"`
	ActiveEventID int64             `json:"active_event_id"`
	LastValue     float64           `json:"last_value"`
	LastSummary   string            `json:"last_summary"`
	LastDetails   map[string]string `json:"last_details"`
}

// NotificationDispatcher defines the interface for dispatching notifications.
type NotificationDispatcher interface {
	Dispatch(channels []string, n Notification)
}

// StateMachine manages the lifecycle transitions, cooldown suppressions,
// and database persistence for alert observations.
type StateMachine struct {
	ruleCache  *RuleCache
	ruleStore  *RuleStore
	dbHandle   *db.Handle
	dispatcher NotificationDispatcher
	clock      model.Clock
	mu         sync.RWMutex
	states     map[StateKey]*RuleTargetState
}

// NewStateMachine creates a new Alert State Machine.
func NewStateMachine(cache *RuleCache, store *RuleStore, dbHandle *db.Handle, disp NotificationDispatcher, clock model.Clock) *StateMachine {
	if clock == nil {
		clock = model.SystemClock{}
	}
	sm := &StateMachine{
		ruleCache:  cache,
		ruleStore:  store,
		dbHandle:   dbHandle,
		dispatcher: disp,
		clock:      clock,
		states:     make(map[StateKey]*RuleTargetState),
	}

	// Wire OnSwap callback into ruleCache if provided
	if cache != nil {
		cache.SetOnSwap(sm.OnRulesSwapped)
	}

	return sm
}

// Observe implements ObservationSink. It processes an evaluation observation and transitions state.
func (sm *StateMachine) Observe(ctx context.Context, obs EvalObservation) {
	if obs.Rule == nil {
		return
	}

	nowSec := obs.At.UTC().Unix()
	if nowSec <= 0 {
		nowSec = sm.clock.Now().UTC().Unix()
	}

	key := StateKey{
		RuleID: obs.Rule.ID,
		Target: obs.Target,
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	st, exists := sm.states[key]
	if !exists {
		if !obs.Breached {
			// Target is healthy and has no pending/firing state; do not store StateOK.
			return
		}
		st = &RuleTargetState{
			RuleID:   obs.Rule.ID,
			RuleName: obs.Rule.Name,
			Target:   obs.Target,
			State:    StateOK,
			Severity: obs.Rule.Severity,
			Since:    nowSec,
		}
		sm.states[key] = st
	}

	if obs.Breached {
		sm.handleBreach(ctx, obs, st, nowSec, key)
	} else {
		sm.handleRecovery(ctx, obs, st, nowSec, key)
	}
}

func (sm *StateMachine) handleBreach(ctx context.Context, obs EvalObservation, st *RuleTargetState, nowSec int64, key StateKey) {
	st.RuleName = obs.Rule.Name
	st.Severity = obs.Rule.Severity
	st.LastValue = obs.Value
	st.LastSummary = obs.Summary
	st.LastDetails = obs.Details

	switch st.State {
	case StateOK:
		if obs.Rule.ForSec > 0 {
			// Transition to Pending
			st.State = StatePending
			st.Since = nowSec
			sm.persistState(ctx, st)
		} else {
			// Instant fire
			sm.transitionToFiring(ctx, obs, st, nowSec)
		}

	case StatePending:
		// Check if sustained duration condition satisfied
		if nowSec-st.Since >= int64(obs.Rule.ForSec) {
			sm.transitionToFiring(ctx, obs, st, nowSec)
		} else {
			sm.persistState(ctx, st)
		}

	case StateFiring:
		// Ongoing breach: check cooldown & severity escalation
		shouldNotify := false
		cooldownSatisfied := obs.Rule.CooldownSec > 0 && (nowSec-st.LastNotified) >= int64(obs.Rule.CooldownSec)
		severityEscalated := severityRank(obs.Rule.Severity) > severityRank(st.NotifiedSev)

		if cooldownSatisfied || severityEscalated {
			shouldNotify = true
		}

		if shouldNotify {
			st.LastNotified = nowSec
			st.NotifiedSev = obs.Rule.Severity
			sm.persistState(ctx, st)
			sm.dispatchNotification(obs, st, "firing")
		} else {
			sm.persistState(ctx, st)
		}
	}
}

func (sm *StateMachine) transitionToFiring(ctx context.Context, obs EvalObservation, st *RuleTargetState, nowSec int64) {
	st.State = StateFiring
	st.Since = nowSec
	st.LastFired = nowSec
	st.LastNotified = nowSec
	st.NotifiedSev = obs.Rule.Severity

	// Create open alert_event in config.db
	if sm.ruleStore != nil {
		detailsBytes, _ := json.Marshal(obs.Details)
		eventID, err := sm.ruleStore.InsertAlertEvent(ctx, AlertEvent{
			RuleID:       obs.Rule.ID,
			RuleName:     obs.Rule.Name,
			Target:       obs.Target,
			Severity:     obs.Rule.Severity,
			State:        "firing",
			Started:      nowSec,
			Resolved:     0,
			Summary:      obs.Summary,
			DetailsJSON:  string(detailsBytes),
			NotifyStatus: "pending",
			CreatedAt:    time.Unix(nowSec, 0).UTC(),
		})
		if err == nil {
			st.ActiveEventID = eventID
		}
	}

	sm.persistState(ctx, st)
	sm.dispatchNotification(obs, st, "firing")
}

func (sm *StateMachine) handleRecovery(ctx context.Context, obs EvalObservation, st *RuleTargetState, nowSec int64, key StateKey) {
	switch st.State {
	case StatePending:
		// Transient blip cleared before for_sec: reset to OK with zero notifications
		st.State = StateOK
		st.Since = nowSec
		sm.deleteState(ctx, key)

	case StateFiring:
		// Active alert resolved!
		st.State = StateOK
		st.Since = nowSec

		// Resolve open event in config.db
		if sm.ruleStore != nil && st.ActiveEventID > 0 {
			_ = sm.ruleStore.UpdateAlertEventResolved(ctx, st.ActiveEventID, nowSec)
		}

		// Dispatch resolution notification if enabled
		if obs.Rule.NotifyRecovery {
			sm.dispatchNotification(obs, st, "resolved")
		}
		st.ActiveEventID = 0
		sm.deleteState(ctx, key)

	case StateOK:
		// Prune any orphaned healthy state from memory and DB
		sm.deleteState(ctx, key)
	}
}

func (sm *StateMachine) dispatchNotification(obs EvalObservation, st *RuleTargetState, stateStr string) {
	if sm.dispatcher == nil || len(obs.Rule.Channels) == 0 {
		return
	}

	n := Notification{
		EventID:  st.ActiveEventID,
		Rule:     obs.Rule.Name,
		Target:   obs.Target,
		Severity: obs.Rule.Severity,
		State:    stateStr,
		Summary:  obs.Summary,
		Details:  obs.Details,
		At:       time.Unix(obs.At.UTC().Unix(), 0).UTC(),
	}

	sm.dispatcher.Dispatch(obs.Rule.Channels, n)
}

func (sm *StateMachine) persistState(ctx context.Context, st *RuleTargetState) {
	if sm.dbHandle == nil {
		return
	}

	detailsBytes, _ := json.Marshal(st.LastDetails)
	_ = sm.dbHandle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		query := `
			INSERT INTO alert_rule_state (
				rule_id, target, state, since, last_fired, last_notified,
				notified_sev, active_event_id, value, summary, details_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(rule_id, target) DO UPDATE SET
				state = excluded.state,
				since = excluded.since,
				last_fired = excluded.last_fired,
				last_notified = excluded.last_notified,
				notified_sev = excluded.notified_sev,
				active_event_id = excluded.active_event_id,
				value = excluded.value,
				summary = excluded.summary,
				details_json = excluded.details_json
		`
		_, err := tx.ExecContext(ctx, query,
			st.RuleID, st.Target, string(st.State), st.Since, st.LastFired, st.LastNotified,
			st.NotifiedSev, st.ActiveEventID, st.LastValue, st.LastSummary, string(detailsBytes),
		)
		return err
	})
}

func (sm *StateMachine) deleteState(ctx context.Context, key StateKey) {
	delete(sm.states, key)
	if sm.dbHandle == nil {
		return
	}

	_ = sm.dbHandle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM alert_rule_state WHERE rule_id = ? AND target = ?",
			key.RuleID, key.Target,
		)
		return err
	})
}

// Restore loads persistent alert states from config.db on startup.
func (sm *StateMachine) Restore(ctx context.Context) error {
	if sm.dbHandle == nil {
		return nil
	}

	query := `
		SELECT rule_id, target, state, since, last_fired, last_notified,
		       notified_sev, active_event_id, value, summary, details_json
		FROM alert_rule_state
	`
	rows, err := sm.dbHandle.QueryRO().QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("restore alert states: %w", err)
	}
	defer rows.Close()

	var snap *RuleSet
	if sm.ruleCache != nil {
		snap = sm.ruleCache.Snapshot()
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	for rows.Next() {
		var st RuleTargetState
		var stateStr string
		var detailsJSON string

		if err := rows.Scan(
			&st.RuleID, &st.Target, &stateStr, &st.Since, &st.LastFired, &st.LastNotified,
			&st.NotifiedSev, &st.ActiveEventID, &st.LastValue, &st.LastSummary, &detailsJSON,
		); err != nil {
			return fmt.Errorf("scan alert state: %w", err)
		}

		st.State = AlertState(stateStr)
		if st.State == StateOK {
			continue
		}
		if len(detailsJSON) > 0 {
			_ = json.Unmarshal([]byte(detailsJSON), &st.LastDetails)
		}

		// Filter out rules no longer active or enabled
		if snap != nil {
			rule := snap.ByID[st.RuleID]
			if rule == nil || !rule.Enabled {
				continue
			}
			st.RuleName = rule.Name
			st.Severity = rule.Severity
		}

		key := StateKey{RuleID: st.RuleID, Target: st.Target}
		sm.states[key] = &st
	}

	return rows.Err()
}

// OnRulesSwapped cleans up any stale state when rules are updated, disabled, or deleted.
func (sm *StateMachine) OnRulesSwapped(oldSet, newSet *RuleSet) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for key, st := range sm.states {
		newRule, exists := newSet.ByID[key.RuleID]
		shouldPurge := false

		if !exists || !newRule.Enabled {
			shouldPurge = true
		} else if oldSet != nil {
			if oldRule, oldExists := oldSet.ByID[key.RuleID]; oldExists {
				if !newRule.UpdatedAt.Equal(oldRule.UpdatedAt) ||
					newRule.ForSec != oldRule.ForSec ||
					newRule.CooldownSec != oldRule.CooldownSec ||
					newRule.Severity != oldRule.Severity ||
					newRule.Target != oldRule.Target ||
					newRule.ConditionJSON != oldRule.ConditionJSON {
					shouldPurge = true
				}
			}
		}

		if shouldPurge {
			delete(sm.states, key)
			if sm.dbHandle != nil {
				_ = sm.dbHandle.ExecWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						"DELETE FROM alert_rule_state WHERE rule_id = ? AND target = ?",
						key.RuleID, key.Target,
					)
					return err
				})
			}
			// If firing, mark event resolved on best effort
			if st.State == StateFiring && st.ActiveEventID > 0 && sm.ruleStore != nil {
				_ = sm.ruleStore.UpdateAlertEventResolved(ctx, st.ActiveEventID, time.Now().UTC().Unix())
			}
		}
	}
}

// ActiveAlerts returns all currently firing alert states ordered newest LastFired first.
func (sm *StateMachine) ActiveAlerts() []RuleTargetState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var firing []RuleTargetState
	for _, st := range sm.states {
		if st.State == StateFiring {
			firing = append(firing, *st)
		}
	}

	sort.Slice(firing, func(i, j int) bool {
		return firing[i].LastFired > firing[j].LastFired
	})

	return firing
}

// AllStates returns a copy of all tracked rule target states (excluding StateOK).
func (sm *StateMachine) AllStates() []RuleTargetState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var nonOK []RuleTargetState
	for _, st := range sm.states {
		if st.State != StateOK {
			nonOK = append(nonOK, *st)
		}
	}
	return nonOK
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}
