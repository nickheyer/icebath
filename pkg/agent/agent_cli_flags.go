package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/goals"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Classic CLI launch flags of the gateway agent kept on the in process agent
type cliFlagsState struct {
	oneTurn                    bool
	oneTurnAlias               string
	oneTurnSessionModel        string
	oneTurnSessionReasoning    any
	oneTurnSessionReasoningSet bool
	controlUpdateCallback      func()
	controlRevision            string
	finite                     bool
	reasoningPushed            bool
}

// The session.mutate reasoning effort of a reasoning config, none when the config carries no override
func effortWord(cfg map[string]any) (string, bool) {
	if cfg == nil {
		return "", false
	}
	if enabled, has := cfg["enabled"]; has && enabled == false {
		return "none", true
	}
	effort, ok := cfg["effort"].(string)
	if !ok || effort == "" {
		return "", false
	}
	return effort, true
}

// SetPushReasoning commits a user picked level to the session row so later rebuilds stop re-sending it
func (a *AIAgent) SetPushReasoning(on bool) {
	a.reasoningPushed = false
	if !on {
		return
	}
	if _, ok := effortWord(a.reasoningConfig); !ok {
		return
	}
	a.reasoningPushed = a.pinReasoning()
}

// Writes the reasoning config into the session's model config, true when the row holds it
func (a *AIAgent) pinReasoning() bool {
	if a.persistDisabled || a.sessionDB == nil || a.sessionID == "" {
		return false
	}
	if err := a.ensureDBSession(); err != nil || !a.sessionDBCreated {
		return false
	}
	err := a.sessionDB.PatchSessionModelConfig(a.homeContext(), a.sessionID, map[string]any{"reasoning_config": a.reasoningConfig})
	return err == nil
}

// SetOneTurn keeps a /model --once pick off the session row and applies its level to this turn only
func (a *AIAgent) SetOneTurn(on bool, pending map[string]any) {
	a.oneTurn = on
	a.oneTurnAlias, a.oneTurnSessionModel = "", ""
	a.oneTurnSessionReasoning, a.oneTurnSessionReasoningSet = nil, false
	if !on {
		return
	}
	if alias, ok := pending["one_turn_alias"].(string); ok {
		a.oneTurnAlias = alias
	}
	if model, ok := pending["model"].(string); ok {
		a.oneTurnSessionModel = model
	}
	if saved, ok := pending["reasoning_config"]; ok {
		a.oneTurnSessionReasoning, a.oneTurnSessionReasoningSet = saved, true
	}
	if effort, ok := pending["one_turn_reasoning"]; ok && effort != nil {
		if parsed := config.ParseReasoningEffort(effort); parsed != nil {
			a.reasoningConfig = parsed
		}
	}
}

// OneTurnAlias returns the model alias a /model --once pick resolved through, empty otherwise
func (a *AIAgent) OneTurnAlias() string { return a.oneTurnAlias }

// SetControlUpdateCallback installs the hook a tool that moved the goal or loop mid turn fires
func (a *AIAgent) SetControlUpdateCallback(fn func()) { a.controlUpdateCallback = fn }

// SetAutoLoadSkillsResult seeds the once per lifecycle skills.auto_load prompt the preload rendered
func (a *AIAgent) SetAutoLoadSkillsResult(prompt string, loaded, missing []string) {
	a.autoLoadSkillsResult = autoLoadSkillsResult{prompt: prompt, loaded: loaded, missing: missing}
	a.autoLoadSkillsResolved = true
}

// SetFinite marks the turn finite so clarify, approvals and completions run headless
func (a *AIAgent) SetFinite(on bool) {
	a.finite = on
	tools.SetFiniteTurn(a.env, on)
}

// ReasoningPushed reports whether the session row holds the user picked reasoning level
func (a *AIAgent) ReasoningPushed() bool { return a.reasoningPushed }

// Fires the control update callback after a tool when the goal, loop or heartbeat snapshot changed
func (a *AIAgent) publishControlIfChanged() {
	if a.controlUpdateCallback == nil || a.sessionDB == nil || a.sessionID == "" {
		return
	}
	revision, err := a.controlSnapshotRevision()
	if err != nil {
		runAgentLog().Debug("session.control.update skipped for " + a.sessionID + ": " + err.Error())
		return
	}
	if revision == a.controlRevision {
		return
	}
	a.controlRevision = revision
	a.controlUpdateCallback()
}

// Revision of the visible goal, loop and heartbeat state of the conversation's current segment
func (a *AIAgent) controlSnapshotRevision() (string, error) {
	ctx := a.homeContext()
	target, err := a.sessionDB.GetCompressionTip(ctx, a.sessionID)
	if err != nil {
		return "", err
	}
	var goalState *goals.GoalState
	var loopState *SessionLoopState
	var heartbeatState *HeartbeatState
	if raw, err := a.controlMeta(ctx, goals.GoalMetaKey(target)); err != nil {
		return "", err
	} else if raw != "" {
		if goalState, err = goals.GoalStateFromJSON(raw); err != nil {
			return "", err
		}
	}
	if raw, err := a.controlMeta(ctx, loopMetaPrefix+target); err != nil {
		return "", err
	} else if raw != "" {
		if loopState, err = SessionLoopStateFromJSON(raw); err != nil {
			return "", err
		}
	}
	if raw, err := a.controlMeta(ctx, heartbeatMetaPrefix+target); err != nil {
		return "", err
	} else if raw != "" {
		if heartbeatState, err = HeartbeatStateFromJSON(raw); err != nil {
			return "", err
		}
	}
	goal := safeGoalSnapshot(goalState)
	deferred := false
	if goal != nil {
		status, _ := goal.Get("status")
		_, barrier := goal.Get("wait_barrier")
		deferred = pyStr(status) == "active" && !barrier
	}
	loop := safeLoopSnapshot(loopState, deferred && loopState != nil && pyStr(loopState.Status) == "active")
	heartbeat := safeHeartbeatSnapshot(heartbeatState)
	return controlSnapshotRevision(goal, loop, heartbeat)
}

// A state_meta value, empty when the key is absent
func (a *AIAgent) controlMeta(ctx context.Context, key string) (string, error) {
	meta, has := a.sessionDB.(interface {
		GetMeta(ctx context.Context, key string) (string, bool, error)
	})
	if !has {
		return "", &workerStoreError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get_meta'", sessionStoreKind(a.sessionDB))}
	}
	raw, ok, err := meta.GetMeta(ctx, key)
	if err != nil || !ok {
		return "", err
	}
	return raw, nil
}

// The stable viewer facing goal fields, nil for no goal or a cleared one
func safeGoalSnapshot(s *goals.GoalState) *pyjson.Object {
	if s == nil || pyStr(s.Status) == "cleared" {
		return nil
	}
	subgoals := make([]any, 0, len(s.Subgoals))
	for _, g := range s.Subgoals {
		subgoals = append(subgoals, g)
	}
	gates := make([]any, 0, len(s.Gates))
	for _, g := range s.Gates {
		var exit any
		if g.LastExitCode != nil {
			exit = *g.LastExitCode
		}
		gates = append(gates, pyjson.ObjectOf("command", g.Command, "timeout_seconds", g.TimeoutSeconds,
			"max_retries", g.MaxRetries, "attempts", g.Attempts, "last_exit_code", exit))
	}
	snapshot := pyjson.ObjectOf("title", s.Goal, "status", s.Status, "turns_used", s.TurnsUsed, "max_turns", s.MaxTurns,
		"contract", s.Contract.ToDict(), "subgoals", subgoals, "gates", gates)
	if s.CreatedAt != 0 {
		snapshot.Set("created_at", s.CreatedAt)
	}
	if s.LastTurnAt != 0 {
		snapshot.Set("updated_at", s.LastTurnAt)
	}
	if pyTruthy(s.PausedReason) {
		snapshot.Set("paused_reason", s.PausedReason)
	}
	if pyTruthy(s.LastVerdict) {
		snapshot.Set("last_verdict", s.LastVerdict)
	}
	if pyTruthy(s.LastReason) {
		snapshot.Set("last_reason", s.LastReason)
	}
	if barrier := goalWaitBarrier(s); barrier != nil {
		snapshot.Set("wait_barrier", barrier)
	}
	return snapshot
}

// The absolute barrier a goal waits on, nil when it waits on nothing the viewer can show
func goalWaitBarrier(s *goals.GoalState) *pyjson.Object {
	reason := pyStr(s.WaitingReason)
	if s.WaitingUntil != 0 && float64(time.Now().UnixNano())/1e9 < s.WaitingUntil {
		return pyjson.ObjectOf("type", "until", "until_at", s.WaitingUntil, "reason", reason)
	}
	if s.WaitingOnSession != nil {
		return pyjson.ObjectOf("type", "session", "target", *s.WaitingOnSession, "reason", reason)
	}
	if s.WaitingOnPID != nil {
		return pyjson.ObjectOf("type", "pid", "target", *s.WaitingOnPID, "reason", reason)
	}
	return nil
}

// The allowlisted persisted loop fields without its route, nil for no loop or a cleared one
func safeLoopSnapshot(s *SessionLoopState, deferredByGoal bool) *pyjson.Object {
	if s == nil || pyStr(s.Status) == "cleared" {
		return nil
	}
	snapshot := pyjson.ObjectOf("prompt", s.Prompt, "status", s.Status, "mode", s.Mode, "interval_seconds", s.IntervalSeconds,
		"current_delay", s.CurrentDelay, "times", s.Times, "until", s.Until, "max_ticks", s.MaxTicks, "ticks_fired", s.TicksFired,
		"created_at", s.CreatedAt, "last_fired_at", s.LastFiredAt, "next_due_at", s.NextDueAt,
		"awaiting_response", s.AwaitingResponse, "deferred_by_goal", deferredByGoal)
	if pyTruthy(s.PausedReason) {
		snapshot.Set("paused_reason", s.PausedReason)
	}
	if pyTruthy(s.LastStopReason) {
		snapshot.Set("last_stop_reason", s.LastStopReason)
	}
	return snapshot
}

// The persisted heartbeat fields the desktop renders, nil for no heartbeat or a cleared one
func safeHeartbeatSnapshot(s *HeartbeatState) *pyjson.Object {
	if s == nil || s.Status == "cleared" {
		return nil
	}
	return pyjson.ObjectOf("prompt", s.Prompt, "status", s.Status, "interval_seconds", s.IntervalSeconds,
		"created_at", s.CreatedAt, "last_fired_at", s.LastFiredAt, "fire_count", s.FireCount)
}

// Hash of the canonical visible state so equal reads always share a revision, empty with no state
func controlSnapshotRevision(goal, loop, heartbeat *pyjson.Object) (string, error) {
	if goal == nil && loop == nil && heartbeat == nil {
		return "", nil
	}
	var goalV, loopV, heartbeatV any
	if goal != nil {
		goalV = goal
	}
	if loop != nil {
		loopV = loop
	}
	if heartbeat != nil {
		heartbeatV = heartbeat
	}
	canonical, err := pyjson.Dumps(pyjson.ObjectOf("goal", goalV, "loop", loopV, "heartbeat", heartbeatV),
		pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}
