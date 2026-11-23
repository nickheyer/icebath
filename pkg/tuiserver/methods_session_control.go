package tuiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stable frontend safe goal fields, nil for no goal or a cleared one
func safeGoalSnapshot(state *goals.GoalState) map[string]any {
	if state == nil || state.Status == "cleared" {
		return nil
	}
	gates := []any{}
	for _, gate := range state.Gates {
		var exit any
		if gate.LastExitCode != nil {
			exit = *gate.LastExitCode
		}
		gates = append(gates, map[string]any{"command": gate.Command, "timeout_seconds": gate.TimeoutSeconds,
			"max_retries": gate.MaxRetries, "attempts": gate.Attempts, "last_exit_code": exit})
	}
	subgoals := []any{}
	for _, s := range state.Subgoals {
		subgoals = append(subgoals, s)
	}
	snapshot := map[string]any{"title": state.Goal, "status": state.Status, "turns_used": state.TurnsUsed, "max_turns": state.MaxTurns,
		"contract": state.Contract.ToDict(), "subgoals": subgoals, "gates": gates}
	if state.CreatedAt != 0 {
		snapshot["created_at"] = state.CreatedAt
	}
	if state.LastTurnAt != 0 {
		snapshot["updated_at"] = state.LastTurnAt
	}
	if truthy(state.PausedReason) {
		snapshot["paused_reason"] = state.PausedReason
	}
	if truthy(state.LastVerdict) {
		snapshot["last_verdict"] = state.LastVerdict
	}
	if truthy(state.LastReason) {
		snapshot["last_reason"] = state.LastReason
	}
	if barrier := extractWaitBarrier(state); barrier != nil {
		snapshot["wait_barrier"] = barrier
	}
	return snapshot
}

// Absolute wait barrier of a goal, the countdown left to the client
func extractWaitBarrier(state *goals.GoalState) map[string]any {
	var reason any = ""
	if truthy(state.WaitingReason) {
		reason = state.WaitingReason
	}
	if state.WaitingUntil != 0 && nowSeconds() < state.WaitingUntil {
		return map[string]any{"type": "until", "until_at": state.WaitingUntil, "reason": reason}
	}
	if state.WaitingOnSession != nil {
		return map[string]any{"type": "session", "session_id": *state.WaitingOnSession, "reason": reason}
	}
	if state.WaitingOnPID != nil {
		return map[string]any{"type": "pid", "pid": *state.WaitingOnPID, "reason": reason}
	}
	return nil
}

// Allowlisted persisted loop fields without the route, nil for no loop or a cleared one
func safeLoopSnapshot(state *agent.SessionLoopState, deferredByGoal bool) map[string]any {
	if state == nil || state.Status == "cleared" {
		return nil
	}
	snapshot := map[string]any{"prompt": state.Prompt, "status": state.Status, "mode": state.Mode, "interval_seconds": state.IntervalSeconds,
		"current_delay": state.CurrentDelay, "times": state.Times, "until": state.Until, "max_ticks": state.MaxTicks,
		"ticks_fired": state.TicksFired, "created_at": state.CreatedAt, "last_fired_at": state.LastFiredAt,
		"next_due_at": state.NextDueAt, "awaiting_response": state.AwaitingResponse, "deferred_by_goal": deferredByGoal}
	if truthy(state.PausedReason) {
		snapshot["paused_reason"] = state.PausedReason
	}
	if truthy(state.LastStopReason) {
		snapshot["last_stop_reason"] = state.LastStopReason
	}
	return snapshot
}

// Persisted heartbeat fields a client renders, nil for no heartbeat or a cleared one
func safeHeartbeatSnapshot(state *agent.HeartbeatState) map[string]any {
	if state == nil || state.Status == "cleared" {
		return nil
	}
	return map[string]any{"prompt": state.Prompt, "status": state.Status, "interval_seconds": state.IntervalSeconds,
		"created_at": state.CreatedAt, "last_fired_at": state.LastFiredAt, "fire_count": state.FireCount}
}

// Snapshot reader every control path calls, the real one swapped only where a test stalls a read
var snapshotControlFn = snapshotControl

// Persisted session control state serialized once without wall clock churn
func snapshotControl(ctx context.Context, sessionKey string) (map[string]any, error) {
	goalState := goals.LoadGoal(ctx, sessionKey)
	loopState := agent.LoadLoop(ctx, sessionKey)
	heartbeatState := agent.LoadHeartbeat(ctx, sessionKey)
	deferredByGoal := loopState != nil && loopState.Status == "active" && goalState != nil && goalState.Status == "active" &&
		agent.GoalBlocksLoopTick(ctx, sessionKey)
	goal, loop, heartbeat := safeGoalSnapshot(goalState), safeLoopSnapshot(loopState, deferredByGoal), safeHeartbeatSnapshot(heartbeatState)
	revision, err := snapshotRevision(goal, loop, heartbeat)
	if err != nil {
		return nil, err
	}
	return map[string]any{"goal": nilMap(goal), "loop": nilMap(loop), "heartbeat": nilMap(heartbeat), "revision": revision,
		"updated_at": snapshotUpdatedAt(goalState, loopState, heartbeatState)}, nil
}

// A nil snapshot as JSON null
func nilMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// SHA256 of the canonical visible state, empty when nothing is set
func snapshotRevision(goal, loop, heartbeat map[string]any) (string, error) {
	if goal == nil && loop == nil && heartbeat == nil {
		return "", nil
	}
	canonical, err := pyjson.Dumps(map[string]any{"goal": nilMap(goal), "loop": nilMap(loop), "heartbeat": nilMap(heartbeat)},
		pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

// Latest persisted timestamp of the three states, zero when none
func snapshotUpdatedAt(goalState *goals.GoalState, loopState *agent.SessionLoopState, heartbeatState *agent.HeartbeatState) any {
	var candidates []float64
	if goalState != nil {
		candidates = append(candidates, goalState.CreatedAt, goalState.LastTurnAt)
	}
	if loopState != nil {
		candidates = append(candidates, loopState.CreatedAt, loopState.LastFiredAt)
	}
	if heartbeatState != nil {
		candidates = append(candidates, heartbeatState.CreatedAt, heartbeatState.LastFiredAt)
	}
	best, found := 0.0, false
	for _, c := range candidates {
		if c != 0 && (!found || c > best) {
			best, found = c, true
		}
	}
	if !found {
		return int64(0)
	}
	return best
}

var publishLockGuard sync.Mutex

// Per session lock serializing the snapshot read, revision compare and emit
func sessionPublishLock(session *tuihost.Session) *sync.Mutex {
	if lock, ok := session.Get("control_publish_lock").(*sync.Mutex); ok {
		return lock
	}
	publishLockGuard.Lock()
	defer publishLockGuard.Unlock()
	lock, _ := session.SetDefault("control_publish_lock", &sync.Mutex{}).(*sync.Mutex)
	return lock
}

// Publishes session.control.update for one live session, skipping an absent or unchanged snapshot when asked
func publishSessionControlSnapshot(ctx context.Context, sid string, session *tuihost.Session, onlyIfPresent, onlyIfChanged bool) {
	if session == nil {
		return
	}
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return
	}
	if failure := catchPanic(func() {
		lock := sessionPublishLock(session)
		lock.Lock()
		defer lock.Unlock()
		control, err := snapshotControlFn(sessionProfileRuntimeScope(ctx, session, true), sessionKey)
		panicIf(err)
		revision := control["revision"].(string)
		lastRevision := strOr(session.Get("control_revision"))
		if (onlyIfPresent && revision == "") || (onlyIfChanged && revision == lastRevision) {
			return
		}
		session.Set("control_revision", revision)
		tuihost.Emit(ctx, "session.control.update", sid, map[string]any{"control": control})
	}); failure != nil {
		pkgLog().Debug("session.control.update publish failed for "+sid, "error", errorMessage(failure))
	}
}
