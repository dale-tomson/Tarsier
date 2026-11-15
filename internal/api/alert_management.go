package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/auth"
)

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
