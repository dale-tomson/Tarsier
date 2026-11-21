package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/logs"
)

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
		s.previewMetricRule(&resp, compiled)
	case "log":
		s.previewLogRule(&resp, compiled)
	case "container":
		s.previewContainerRule(r.Context(), &resp, compiled)
	}

	auth.WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) previewMetricRule(resp *RulePreviewResponse, compiled *alert.CompiledRule) {
	if s.hotCache == nil || compiled.MetricCond == nil {
		return
	}

	cond := compiled.MetricCond
	metric := alert.NormalizeMetricName(cond.Metric)
	var targets []string
	if compiled.Target == "host" {
		targets = []string{"host"}
	} else {
		seen := make(map[string]struct{})
		for _, ent := range s.hotCache.EntitiesWithMetric(metric) {
			if compiled.MatchesTarget(ent) {
				if _, ok := seen[ent]; !ok {
					seen[ent] = struct{}{}
					targets = append(targets, ent)
				}
			}
		}
		if metric != cond.Metric {
			for _, ent := range s.hotCache.EntitiesWithMetric(cond.Metric) {
				if compiled.MatchesTarget(ent) {
					if _, ok := seen[ent]; !ok {
						seen[ent] = struct{}{}
						targets = append(targets, ent)
					}
				}
			}
		}
		sort.Strings(targets)
	}

	if len(targets) == 0 {
		resp.SampleLines = append(resp.SampleLines, fmt.Sprintf("No active telemetry found for target %q with metric %q in HotCache", compiled.Target, metric))
		return
	}

	nowSec := time.Now().UTC().Unix()
	var liveTargetsCount int
	for _, target := range targets {
		pt, ok := s.hotCache.GetLatest(target, metric)
		if !ok && metric != cond.Metric {
			pt, ok = s.hotCache.GetLatest(target, cond.Metric)
		}
		if !ok || (nowSec-pt.TS > 120) {
			continue
		}
		liveTargetsCount++

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
			summary := fmt.Sprintf("%s %s is %.1f (%s %.1f)", target, metric, pt.Value, cond.Operator, cond.Threshold)
			if compiled.ForSec > 0 {
				summary = fmt.Sprintf("%s %s is %.1f (%s %.1f; requires %s sustained)",
					target, metric, pt.Value, cond.Operator, cond.Threshold, alert.FormatDuration(compiled.ForSec))
			}
			resp.MatchedTargets = append(resp.MatchedTargets, MatchedTarget{
				Target:  target,
				Value:   pt.Value,
				Summary: summary,
			})
		}
	}

	if liveTargetsCount == 0 && len(resp.SampleLines) == 0 {
		resp.SampleLines = append(resp.SampleLines, fmt.Sprintf("Target matched %d entity(s), but no sample received within last 120s", len(targets)))
	}
}

func (s *Server) previewLogRule(resp *RulePreviewResponse, compiled *alert.CompiledRule) {
	if s.tailCache == nil || compiled.LogCond == nil {
		return
	}

	cond := compiled.LogCond
	totalMatches := 0

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

func (s *Server) previewContainerRule(ctx context.Context, resp *RulePreviewResponse, compiled *alert.CompiledRule) {
	if s.configDB == nil || compiled.ContainerCond == nil {
		return
	}

	cond := compiled.ContainerCond
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
	rows, qErr := s.configDB.QueryRO().QueryContext(ctx, query, sinceSec)
	if qErr != nil {
		return
	}
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
