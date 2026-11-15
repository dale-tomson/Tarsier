package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/logs"
)

// RuleWithState combines a persistent alert rule with its live runtime state.
type RuleWithState struct {
	alert.AlertRule
	LiveState     string  `json:"live_state"` // "ok" | "pending" | "firing"
	ActiveTargets int     `json:"active_targets"`
	LastFired     *string `json:"last_fired,omitempty"` // RFC 3339 timestamp
}

// RulePreviewRequest describes the parameters for live dry-run evaluation.
type RulePreviewRequest struct {
	Kind          string   `json:"kind"`
	Target        string   `json:"target"`
	Severity      string   `json:"severity"`
	ConditionJSON string   `json:"condition_json"`
	ForSec        int      `json:"for_sec"`
	Channels      []string `json:"channels"`
}

// RulePreviewResponse captures the live evaluation outcome.
type RulePreviewResponse struct {
	WouldFireNow     bool            `json:"would_fire_now"`
	MatchedTargets   []MatchedTarget `json:"matched_targets"`
	RecentMatches24h int             `json:"recent_matches_24h"`
	SampleLines      []string        `json:"sample_lines"`
}

// MatchedTarget represents a specific target matching preview criteria right now.
type MatchedTarget struct {
	Target  string  `json:"target"`
	Value   float64 `json:"value"`
	Summary string  `json:"summary"`
}

// RulesExportBundle represents the portable export format for rules.
type RulesExportBundle struct {
	Version int               `json:"version"`
	Rules   []alert.AlertRule `json:"rules"`
}

// StarterTemplate defines a curated pre-configured rule template.
type StarterTemplate struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Rule        alert.AlertRule `json:"rule"`
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
func (s *Server) handleGetTemplates(w http.ResponseWriter, r *http.Request) {
	defaultChannels := []string{"discord"}
	if s.cfg.Notifiers.Email.Enabled && len(s.cfg.Notifiers.Email.To) > 0 {
		defaultChannels = append(defaultChannels, "email")
	}

	templates := []StarterTemplate{
		{
			ID:          "high-host-cpu",
			Name:        "High Host CPU",
			Description: "Alerts when host CPU utilization exceeds 90% for 5 minutes",
			Rule: alert.AlertRule{
				Name:           "High Host CPU",
				Kind:           "metric",
				Enabled:        true,
				Severity:       "warning",
				Target:         "host",
				ConditionJSON:  `{"metric":"cpu_pct","operator":">","threshold":90.0}`,
				ForSec:         300,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "disk-almost-full",
			Name:        "Disk Almost Full",
			Description: "Alerts when any mounted disk reaches or exceeds 85% capacity",
			Rule: alert.AlertRule{
				Name:           "Disk Almost Full",
				Kind:           "metric",
				Enabled:        true,
				Severity:       "critical",
				Target:         "disk:*",
				ConditionJSON:  `{"metric":"disk_used_pct","operator":">=","threshold":85.0}`,
				ForSec:         60,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "high-host-memory",
			Name:        "High Host Memory",
			Description: "Alerts when host memory utilization exceeds 90% for 5 minutes",
			Rule: alert.AlertRule{
				Name:           "High Host Memory",
				Kind:           "metric",
				Enabled:        true,
				Severity:       "warning",
				Target:         "host",
				ConditionJSON:  `{"metric":"mem_used_pct","operator":">","threshold":90.0}`,
				ForSec:         300,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "container-down",
			Name:        "Container Down / Exited",
			Description: "Alerts immediately when any monitored container stops or dies",
			Rule: alert.AlertRule{
				Name:           "Container Down / Exited",
				Kind:           "container",
				Enabled:        true,
				Severity:       "critical",
				Target:         "*",
				ConditionJSON:  `{"events":["die","stop"],"threshold":1,"window_sec":0}`,
				ForSec:         0,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "container-oom",
			Name:        "Container OOM Killed",
			Description: "Alerts immediately when a container is killed by the out-of-memory killer",
			Rule: alert.AlertRule{
				Name:           "Container OOM Killed",
				Kind:           "container",
				Enabled:        true,
				Severity:       "critical",
				Target:         "*",
				ConditionJSON:  `{"events":["oom"],"threshold":1,"window_sec":0}`,
				ForSec:         0,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "container-restart-loop",
			Name:        "Container Restart Loop",
			Description: "Alerts when a container restarts 3 or more times within a 5-minute window",
			Rule: alert.AlertRule{
				Name:           "Container Restart Loop",
				Kind:           "container",
				Enabled:        true,
				Severity:       "warning",
				Target:         "*",
				ConditionJSON:  `{"events":["restart","die"],"threshold":3,"window_sec":300}`,
				ForSec:         0,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
		{
			ID:          "log-error-burst",
			Name:        "Log Error Burst",
			Description: "Alerts when 10 or more error-level log lines occur within 60 seconds",
			Rule: alert.AlertRule{
				Name:           "Log Error Burst",
				Kind:           "log",
				Enabled:        true,
				Severity:       "warning",
				Target:         "*",
				ConditionJSON:  `{"pattern":"error","is_regex":false,"ignore_case":true,"min_level":4,"threshold":10,"window_sec":60}`,
				ForSec:         0,
				CooldownSec:    300,
				Channels:       defaultChannels,
				NotifyRecovery: true,
			},
		},
	}

	auth.WriteJSON(w, http.StatusOK, templates)
}

// handlePreviewRule evaluates candidate rule parameters against live metrics and recent data.
func (s *Server) handlePreviewRule(w http.ResponseWriter, r *http.Request) {
	var req RulePreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed request payload")
		return
	}

	rule := alert.AlertRule{
		Name:          "preview-test",
		Kind:          req.Kind,
		Enabled:       true,
		Severity:      req.Severity,
		Target:        req.Target,
		ConditionJSON: req.ConditionJSON,
		ForSec:        req.ForSec,
		Channels:      req.Channels,
	}
	if len(rule.Channels) == 0 {
		rule.Channels = []string{"discord"}
	}

	compiled, err := alert.CompileRule(rule)
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return
	}

	resp := RulePreviewResponse{
		MatchedTargets: []MatchedTarget{},
		SampleLines:    []string{},
	}

	switch compiled.Kind {
	case "metric":
		if s.hotCache != nil && compiled.MetricCond != nil {
			cond := compiled.MetricCond
			var targets []string
			if compiled.Target == "host" {
				targets = []string{"host"}
			} else {
				for _, ent := range s.hotCache.EntitiesWithMetric(cond.Metric) {
					if compiled.MatchesTarget(ent) {
						targets = append(targets, ent)
					}
				}
			}

			nowSec := time.Now().UTC().Unix()
			for _, target := range targets {
				pt, ok := s.hotCache.GetLatest(target, cond.Metric)
				if !ok || (nowSec-pt.TS > 120) {
					continue
				}

				breached := false
				switch cond.Operator {
				case ">":
					breached = pt.Value > cond.Threshold
				case ">=":
					breached = pt.Value >= cond.Threshold
				case "<":
					breached = pt.Value < cond.Threshold
				case "<=":
					breached = pt.Value <= cond.Threshold
				case "==":
					breached = pt.Value == cond.Threshold
				}

				if breached {
					resp.WouldFireNow = true
					resp.MatchedTargets = append(resp.MatchedTargets, MatchedTarget{
						Target:  target,
						Value:   pt.Value,
						Summary: fmt.Sprintf("%s %s is %.1f (%s %.1f)", target, cond.Metric, pt.Value, cond.Operator, cond.Threshold),
					})
				}
			}
		}

	case "log":
		if s.tailCache != nil && compiled.LogCond != nil {
			cond := compiled.LogCond
			totalMatches := 0

			// Scan recent lines from tail cache
			for _, sid := range s.tailCache.SourceIDs() {
				res := s.tailCache.QueryTail(logs.TailQuery{SourceID: sid, Limit: 200})
				for _, entry := range res.Entries {
					if cond.MinLevel > 0 && uint8(entry.Level) < cond.MinLevel {
						continue
					}
					lineBytes := []byte(entry.Line)
					if compiled.MatchLogLine(lineBytes) {
						totalMatches++
						if len(resp.SampleLines) < 5 {
							lineStr := entry.Line
							if len(lineStr) > 256 {
								lineStr = lineStr[:256] + "..."
							}
							resp.SampleLines = append(resp.SampleLines, lineStr)
						}
					}
				}
			}

			resp.RecentMatches24h = totalMatches
			if totalMatches >= cond.Threshold {
				resp.WouldFireNow = true
			}
		}

	case "container":
		if s.configDB != nil && compiled.ContainerCond != nil {
			cond := compiled.ContainerCond
			// Check recent container_event rows
			nowSec := time.Now().UTC().Unix()
			sinceSec := nowSec - 86400

			query := `
				SELECT ce.type, ce.detail, c.name
				FROM container_event ce
				JOIN container c ON c.id = ce.container_id
				WHERE ce.ts >= ?
				ORDER BY ce.ts DESC
				LIMIT 100
			`
			rows, qErr := s.configDB.QueryRO().QueryContext(r.Context(), query, sinceSec)
			if qErr == nil {
				defer rows.Close()
				matchedCount := 0
				for rows.Next() {
					var evType, detail, name string
					if err := rows.Scan(&evType, &detail, &name); err == nil {
						target := "container:" + name
						if compiled.MatchesTarget(target) || compiled.MatchesTarget(name) {
							for _, allowed := range cond.Events {
								if strings.EqualFold(allowed, evType) || (allowed == "down" && (evType == "die" || evType == "stop")) {
									matchedCount++
									break
								}
							}
						}
					}
				}
				resp.RecentMatches24h = matchedCount
				if matchedCount >= cond.Threshold {
					resp.WouldFireNow = true
				}
			}
		}
	}

	auth.WriteJSON(w, http.StatusOK, resp)
}

// handleExportRules exports configured rules as portable JSON.
func (s *Server) handleExportRules(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	rules, err := s.ruleStore.ListRules(r.Context())
	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	bundle := RulesExportBundle{
		Version: 1,
		Rules:   rules,
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\"tarsier-alert-rules.json\"")
	auth.WriteJSON(w, http.StatusOK, bundle)
}

// handleImportRules imports rules from JSON bundle.
func (s *Server) handleImportRules(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "store_unavailable", "alert store not configured")
		return
	}

	var bundle RulesExportBundle
	if err := json.NewDecoder(r.Body).Decode(&bundle); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed rules JSON bundle")
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "upsert"
	}

	existingRules, err := s.ruleStore.ListRules(r.Context())
	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	existingByName := make(map[string]alert.AlertRule)
	for _, er := range existingRules {
		existingByName[er.Name] = er
	}

	importedCount := 0
	skippedCount := 0

	for _, rule := range bundle.Rules {
		existing, found := existingByName[rule.Name]
		if found {
			if mode == "skip" {
				skippedCount++
				continue
			}
			// Upsert mode
			rule.ID = existing.ID
			if _, err := s.ruleStore.UpdateRule(r.Context(), rule); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "import_failed", fmt.Sprintf("failed to update rule %q: %v", rule.Name, err))
				return
			}
			importedCount++
		} else {
			if _, err := s.ruleStore.CreateRule(r.Context(), rule); err != nil {
				auth.WriteJSONError(w, http.StatusBadRequest, "import_failed", fmt.Sprintf("failed to create rule %q: %v", rule.Name, err))
				return
			}
			importedCount++
		}
	}

	if s.ruleCache != nil {
		_ = s.ruleCache.Reload(r.Context())
	}

	auth.WriteJSON(w, http.StatusOK, map[string]any{
		"imported": importedCount,
		"skipped":  skippedCount,
	})
}

// handleAlertHistory queries historical alert incidents with keyset pagination.
func (s *Server) handleAlertHistory(w http.ResponseWriter, r *http.Request) {
	if s.ruleStore == nil {
		auth.WriteJSON(w, http.StatusOK, []alert.AlertEvent{})
		return
	}

	q := r.URL.Query()
	limit := 200
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	var cursor int64
	if c := q.Get("cursor"); c != "" {
		if parsed, err := strconv.ParseInt(c, 10, 64); err == nil {
			cursor = parsed
		}
	}

	var ruleID int64
	if rid := q.Get("rule_id"); rid != "" {
		if parsed, err := strconv.ParseInt(rid, 10, 64); err == nil {
			ruleID = parsed
		}
	}

	filter := alert.EventFilter{
		RuleID:   ruleID,
		Severity: q.Get("severity"),
		State:    q.Get("state"),
		Cursor:   cursor,
		Limit:    limit,
	}

	events, err := s.ruleStore.ListAlertEvents(r.Context(), filter)
	if err != nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}

	if events == nil {
		events = []alert.AlertEvent{}
	}

	auth.WriteJSON(w, http.StatusOK, events)
}

// handleTestNotification dispatches an immediate test notification over the specified channel.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if s.dispatcher == nil {
		auth.WriteJSONError(w, http.StatusInternalServerError, "dispatcher_unavailable", "alert dispatcher not configured")
		return
	}

	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_json", "malformed request payload")
		return
	}

	channel := strings.TrimSpace(strings.ToLower(req.Channel))
	if channel == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "channel_required", "channel name is required ('email' or 'discord')")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := s.dispatcher.SendTest(ctx, channel); err != nil {
		auth.WriteJSONError(w, http.StatusBadGateway, "test_failed", err.Error())
		return
	}

	auth.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"channel": channel,
		"message": fmt.Sprintf("test notification successfully sent to %s", channel),
	})
}
