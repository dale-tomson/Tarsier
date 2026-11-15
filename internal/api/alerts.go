package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/auth"
)

// RuleWithState combines a persistent alert rule with its live runtime state.
type RuleWithState struct {
	alert.AlertRule
	LiveState     string  `json:"live_state"` // "ok" | "pending" | "firing"
	ActiveTargets int     `json:"active_targets"`
	LastFired     *string `json:"last_fired,omitempty"` // RFC 3339 timestamp
}


// handleActiveAlerts returns all currently firing alerts.
func (s *Server) handleActiveAlerts(w http.ResponseWriter, r *http.Request) {
	if s.stateMachine == nil {
		auth.WriteJSON(w, http.StatusOK, []alert.RuleTargetState{})
		return
	}

	active := s.stateMachine.ActiveAlerts()
	if active == nil {
		active = []alert.RuleTargetState{}
	}
	auth.WriteJSON(w, http.StatusOK, active)
}

// handleListRules retrieves all rules merged with their live state.
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSON(w, http.StatusOK, []RuleWithState{})
		return
	}

	rawRules, err := s.ruleStore.ListRules(r.Context())
	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "failed_query", "failed to list rules")
		return
	}

	liveStates := make(map[int64][]alert.RuleTargetState)
	if s.stateMachine != nil {
		for _, st := range s.stateMachine.AllStates() {
			liveStates[st.RuleID] = append(liveStates[st.RuleID], st)
		}
	}

	result := make([]RuleWithState, 0, len(rawRules))
	for _, rule := range rawRules {
		rws := RuleWithState{
			AlertRule: rule,
			LiveState: "ok",
		}

		states := liveStates[rule.ID]
		if len(states) > 0 {
			hasFiring := false
			hasPending := false
			var newestFired int64

			for _, st := range states {
				if st.State == alert.StateFiring {
					hasFiring = true
					if st.LastFired > newestFired {
						newestFired = st.LastFired
					}
				} else if st.State == alert.StatePending {
					hasPending = true
				}
			}

			if hasFiring {
				rws.LiveState = "firing"
				rws.ActiveTargets = len(states)
				if newestFired > 0 {
					tsStr := time.Unix(newestFired, 0).UTC().Format(time.RFC3339)
					rws.LastFired = &tsStr
				}
			} else if hasPending {
				rws.LiveState = "pending"
				rws.ActiveTargets = len(states)
			}
		}

		result = append(result, rws)
	}

	auth.WriteJSON(w, http.StatusOK, result)
}

// handleCreateRule validates, compiles, and inserts a new rule.
func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	var req alert.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed request payload")
		return
	}

	created, err := s.ruleStore.CreateRule(r.Context(), req)
	if err != nil {
		if errors.Is(err, alert.ErrRuleCapExceeded) {
			auth.WriteJSONError(w, http.StatusBadRequest, "rule_cap_exceeded", err.Error())
			return
		}
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return
	}

	if s.ruleCache != nil {
		_ = s.ruleCache.Reload(r.Context())
	}

	auth.WriteJSON(w, http.StatusCreated, created)
}

// handleGetRule retrieves a single rule by ID.
func (s *Server) handleGetRule(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_id", "rule ID must be a positive integer")
		return
	}

	rule, err := s.ruleStore.GetRule(r.Context(), id)
	if err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "rule not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	auth.WriteJSON(w, http.StatusOK, rule)
}

// handleUpdateRule validates and updates an existing rule.
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_id", "rule ID must be a positive integer")
		return
	}

	var req alert.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed request payload")
		return
	}
	req.ID = id

	updated, err := s.ruleStore.UpdateRule(r.Context(), req)
	if err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "rule not found")
			return
		}
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return
	}

	if s.ruleCache != nil {
		_ = s.ruleCache.Reload(r.Context())
	}

	auth.WriteJSON(w, http.StatusOK, updated)
}

// handleDeleteRule removes an alert rule.
func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_id", "rule ID must be a positive integer")
		return
	}

	if err := s.ruleStore.DeleteRule(r.Context(), id); err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "rule not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}

	if s.ruleCache != nil {
		_ = s.ruleCache.Reload(r.Context())
	}

	auth.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleEnableRule enables a disabled rule.
func (s *Server) handleEnableRule(w http.ResponseWriter, r *http.Request) {
	s.setRuleEnabledStatus(w, r, true)
}

// handleDisableRule disables an active rule.
func (s *Server) handleDisableRule(w http.ResponseWriter, r *http.Request) {
	s.setRuleEnabledStatus(w, r, false)
}

func (s *Server) setRuleEnabledStatus(w http.ResponseWriter, r *http.Request, enabled bool) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_id", "rule ID must be a positive integer")
		return
	}

	if err := s.ruleStore.SetRuleEnabled(r.Context(), id, enabled); err != nil {
		if errors.Is(err, alert.ErrRuleNotFound) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "rule not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "toggle_failed", err.Error())
		return
	}

	if s.ruleCache != nil {
		_ = s.ruleCache.Reload(r.Context())
	}

	auth.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enabled})
}

// handleGetTemplates returns the curated built-in starter templates.
