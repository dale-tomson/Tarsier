package alert

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dale-tomson/Tarsier/internal/docker"
	"github.com/dale-tomson/Tarsier/internal/model"
)

type containerWindowKey struct {
	ruleID        int64
	containerName string
}

type containerWindowState struct {
	timestamps []int64 // bounded circular/slice buffer of event timestamps
	breached   bool
	lastTS     int64
}

// ContainerEvaluator evaluates container lifecycle events against active container rules.
type ContainerEvaluator struct {
	cache   *RuleCache
	sink    ObservationSink
	clock   model.Clock
	mu      sync.Mutex
	windows map[containerWindowKey]*containerWindowState
}

// NewContainerEvaluator creates a new ContainerEvaluator.
func NewContainerEvaluator(cache *RuleCache, sink ObservationSink, clock model.Clock) *ContainerEvaluator {
	if clock == nil {
		clock = model.SystemClock{}
	}
	return &ContainerEvaluator{
		cache:   cache,
		sink:    sink,
		clock:   clock,
		windows: make(map[containerWindowKey]*containerWindowState),
	}
}

// Run executes periodic window expiry sweeps every 10 seconds.
func (e *ContainerEvaluator) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.SweepWindows(ctx)
		}
	}
}

// OnContainerEvent processes a container lifecycle transition event from Docker.
func (e *ContainerEvaluator) OnContainerEvent(ctx context.Context, ev docker.ContainerEvent) {
	if e.cache == nil || e.sink == nil {
		return
	}

	snap := e.cache.Snapshot()
	if snap == nil || len(snap.ContainerRules) == 0 {
		return
	}

	target := "container:" + ev.ContainerName
	evAction := strings.ToLower(strings.TrimSpace(ev.Action))
	evTS := ev.TS
	if evTS <= 0 {
		evTS = e.clock.Now().UTC().Unix()
	}
	evTime := time.Unix(evTS, 0).UTC()

	for _, rule := range snap.ContainerRules {
		if rule.ContainerCond == nil {
			continue
		}

		// Check target match
		if !rule.MatchesTarget(target) && !rule.MatchesTarget(ev.ContainerName) {
			continue
		}

		cond := rule.ContainerCond
		isImmediate := cond.Threshold <= 1 && cond.WindowSec == 0

		if isImmediate {
			// Immediate single-event trigger (e.g. oom, die, stop)
			eventMatched := false
			for _, allowed := range cond.Events {
				if strings.EqualFold(allowed, evAction) {
					eventMatched = true
					break
				}
				// "down" tracks die or stop
				if allowed == "down" && (evAction == "die" || evAction == "stop") {
					eventMatched = true
					break
				}
			}

			if eventMatched {
				summary := fmt.Sprintf("container %s received %s event", ev.ContainerName, evAction)
				if ev.ExitCode != 0 {
					summary = fmt.Sprintf("container %s received %s event (exit code %d)", ev.ContainerName, evAction, ev.ExitCode)
				}

				details := map[string]string{
					"container": ev.ContainerName,
					"image":     ev.Image,
					"event":     evAction,
					"exit_code": strconv.Itoa(ev.ExitCode),
				}
				if ev.Detail != "" {
					details["detail"] = ev.Detail
				}

				e.sink.Observe(ctx, EvalObservation{
					Rule:     rule,
					Target:   target,
					Breached: true,
					Value:    1,
					Summary:  summary,
					Details:  details,
					At:       evTime,
				})
			} else if evAction == "start" {
				// Container recovery: "start" clears "down" / "die" / "stop" alerts
				tracksDown := false
				for _, allowed := range cond.Events {
					if allowed == "down" || allowed == "die" || allowed == "stop" {
						tracksDown = true
						break
					}
				}
				if tracksDown {
					e.sink.Observe(ctx, EvalObservation{
						Rule:     rule,
						Target:   target,
						Breached: false,
						Value:    0,
						Summary:  fmt.Sprintf("container %s started", ev.ContainerName),
						Details: map[string]string{
							"container": ev.ContainerName,
							"image":     ev.Image,
							"event":     "start",
						},
						At: evTime,
					})
				}
			}
			continue
		}

		// Sliding-window frequency trigger (e.g. restart loop)
		eventMatched := false
		for _, allowed := range cond.Events {
			if strings.EqualFold(allowed, evAction) {
				eventMatched = true
				break
			}
			if allowed == "down" && (evAction == "die" || evAction == "stop") {
				eventMatched = true
				break
			}
		}

		if !eventMatched {
			continue
		}

		key := containerWindowKey{ruleID: rule.ID, containerName: ev.ContainerName}
		e.mu.Lock()
		st, exists := e.windows[key]
		if !exists {
			st = &containerWindowState{
				timestamps: make([]int64, 0, 16),
			}
			e.windows[key] = st
		}

		// Append timestamp (capped at 256)
		if len(st.timestamps) >= 256 {
			st.timestamps = st.timestamps[1:]
		}
		st.timestamps = append(st.timestamps, evTS)
		st.lastTS = evTS

		// Prune timestamps older than window
		cutoff := evTS - int64(cond.WindowSec)
		validCount := 0
		for _, t := range st.timestamps {
			if t >= cutoff {
				validCount++
			}
		}

		shouldFire := validCount >= cond.Threshold
		wasBreached := st.breached
		if shouldFire {
			st.breached = true
		}
		e.mu.Unlock()

		if shouldFire && !wasBreached {
			summary := fmt.Sprintf("container %s breached event threshold (%d %s in %ds)",
				ev.ContainerName, validCount, strings.Join(cond.Events, ","), cond.WindowSec)

			details := map[string]string{
				"container":  ev.ContainerName,
				"image":      ev.Image,
				"events":     strings.Join(cond.Events, ","),
				"count":      strconv.Itoa(validCount),
				"threshold":  strconv.Itoa(cond.Threshold),
				"window_sec": strconv.Itoa(cond.WindowSec),
			}

			e.sink.Observe(ctx, EvalObservation{
				Rule:     rule,
				Target:   target,
				Breached: true,
				Value:    float64(validCount),
				Summary:  summary,
				Details:  details,
				At:       evTime,
			})
		}
	}
}

// SweepWindows prunes expired sliding-window entries and emits recovery when frequencies drop below threshold.
func (e *ContainerEvaluator) SweepWindows(ctx context.Context) {
	if e.cache == nil || e.sink == nil {
		return
	}

	snap := e.cache.Snapshot()
	if snap == nil {
		return
	}

	nowSec := e.clock.Now().UTC().Unix()
	nowTime := time.Unix(nowSec, 0).UTC()

	e.mu.Lock()
	defer e.mu.Unlock()

	for key, st := range e.windows {
		rule := snap.ByID[key.ruleID]
		if rule == nil || !rule.Enabled || rule.ContainerCond == nil {
			delete(e.windows, key)
			continue
		}

		cond := rule.ContainerCond
		if cond.WindowSec <= 0 {
			delete(e.windows, key)
			continue
		}

		cutoff := nowSec - int64(cond.WindowSec)
		validCount := 0
		var pruned []int64
		for _, ts := range st.timestamps {
			if ts >= cutoff {
				validCount++
				pruned = append(pruned, ts)
			}
		}
		st.timestamps = pruned

		if st.breached && validCount < cond.Threshold {
			st.breached = false
			target := "container:" + key.containerName

			e.sink.Observe(ctx, EvalObservation{
				Rule:     rule,
				Target:   target,
				Breached: false,
				Value:    float64(validCount),
				Summary: fmt.Sprintf("container %s event frequency normalized (%d events in %ds)",
					key.containerName, validCount, cond.WindowSec),
				Details: map[string]string{
					"container":  key.containerName,
					"count":      strconv.Itoa(validCount),
					"threshold":  strconv.Itoa(cond.Threshold),
					"window_sec": strconv.Itoa(cond.WindowSec),
				},
				At: nowTime,
			})
		}

		if len(st.timestamps) == 0 && !st.breached {
			delete(e.windows, key)
		}
	}
}

// PurgeRule removes window tracking for a specific rule ID.
func (e *ContainerEvaluator) PurgeRule(ruleID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for k := range e.windows {
		if k.ruleID == ruleID {
			delete(e.windows, k)
		}
	}
}
