package tuiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
		return map[string]any{"type": "session", "target": *state.WaitingOnSession, "reason": reason}
	}
	if state.WaitingOnPID != nil {
		return map[string]any{"type": "pid", "target": *state.WaitingOnPID, "reason": reason}
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

// Persisted heartbeat fields the desktop renders, nil for no heartbeat or a cleared one
func safeHeartbeatSnapshot(state *agent.HeartbeatState) map[string]any {
	if state == nil || state.Status == "cleared" {
		return nil
	}
	return map[string]any{"prompt": state.Prompt, "status": state.Status, "interval_seconds": state.IntervalSeconds,
		"created_at": state.CreatedAt, "last_fired_at": state.LastFiredAt, "fire_count": state.FireCount}
}

// Snapshot reader every control path calls, the real one swapped only where a hermes case stalls a read
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
func sessionPublishLock(session *tuigateway.Session) *sync.Mutex {
	if lock, ok := session.Get("control_publish_lock").(*sync.Mutex); ok {
		return lock
	}
	publishLockGuard.Lock()
	defer publishLockGuard.Unlock()
	lock, _ := session.SetDefault("control_publish_lock", &sync.Mutex{}).(*sync.Mutex)
	return lock
}

// Publishes session.control.update for one live session, skipping an absent or unchanged snapshot when asked
func publishSessionControlSnapshot(ctx context.Context, sid string, session *tuigateway.Session, onlyIfPresent, onlyIfChanged bool) {
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
		tuigateway.Emit(ctx, "session.control.update", sid, map[string]any{"control": control})
	}); failure != nil {
		logger().Debug("session.control.update publish failed for "+sid, "error", errorMessage(failure))
	}
}

// Goal and loop control actions routed through their slash command, name and argument
var actionCommandMap = map[string][2]string{
	"goal.pause": {"goal", "pause"}, "goal.resume": {"goal", "resume"}, "goal.clear": {"goal", "clear"},
	"goal.unwait": {"goal", "unwait"}, "loop.pause": {"loop", "pause"}, "loop.resume": {"loop", "resume"},
	"loop.stop": {"loop", "stop"},
}

// Control actions with no slash command, run through the manager APIs
var managerActions = map[string]bool{
	"subgoal.add": true, "subgoal.remove": true, "subgoal.clear": true,
	"heartbeat.pause": true, "heartbeat.resume": true, "heartbeat.clear": true,
}

var sessionControlRegistry = &handlerRegistry{}

func init() {
	sessionControlRegistry.scopedMethod("session.control.read", sessionControlRead)
	sessionControlRegistry.scopedMethod("session.control", sessionControl)
	sessionControlRegistry.install()
}

// Current stable control snapshot of a live session
func sessionControlRead(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return rpcErr(rid, 4001, "session has no stored key", nil)
	}
	var control map[string]any
	if failure := catchPanic(func() {
		var err error
		control, err = snapshotControlFn(ctx, sessionKey)
		panicIf(err)
	}); failure != nil {
		logger().Debug("session.control.read failed: " + errorMessage(failure))
		return rpcErr(rid, 5031, "session.control.read failed: "+errorMessage(failure), nil)
	}
	return ok(rid, map[string]any{"control": control})
}

// Runs one allowlisted control action and answers with its exact resulting snapshot
func sessionControl(ctx context.Context, rid any, params map[string]any) map[string]any {
	rawAction, isText := params["action"].(string)
	action := strings.TrimSpace(rawAction)
	if !isText || action == "" {
		return rpcErr(rid, 4004, "action is required", nil)
	}
	if strings.HasPrefix(action, "goal.gate") {
		return rpcErr(rid, 4004, "gate actions are not allowed through session.control", nil)
	}
	if _, command := actionCommandMap[action]; !command && !managerActions[action] {
		return rpcErr(rid, 4004, "unknown action: "+action, nil)
	}
	args := map[string]any{}
	if raw, present := params["args"]; present {
		object, isObject := raw.(map[string]any)
		if !isObject {
			return rpcErr(rid, 4004, "args must be an object", nil)
		}
		args = object
	}
	validated, validationError := validateActionArgs(rid, action, args)
	if validationError != nil {
		return validationError
	}
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return rpcErr(rid, 4001, "session has no stored key", nil)
	}
	var sessionID any = ""
	if truthy(params["session_id"]) {
		sessionID = params["session_id"]
	}
	var actionResult map[string]any
	if command, isCommand := actionCommandMap[action]; isCommand {
		actionResult = dispatchControlCommand(ctx, rid, sessionID, command[0], command[1])
	} else {
		result, err := executeManagerAction(ctx, sessionKey, action, validated)
		if err != nil {
			if kind := pyExceptionKind(err); kind != "RuntimeError" && kind != "ValueError" && kind != "IndexError" {
				panic(err)
			}
			return rpcErr(rid, 4004, managerErrorMessage(action, err), nil)
		}
		actionResult = result
	}
	if _, failed := actionResult["error"]; failed {
		return actionResult
	}
	var control map[string]any
	if failure := catchPanic(func() {
		var err error
		control, err = snapshotControlFn(ctx, sessionKey)
		panicIf(err)
	}); failure != nil {
		logger().Debug("session.control snapshot after " + action + " failed: " + errorMessage(failure))
		return rpcErr(rid, 5031, "session.control snapshot failed: "+errorMessage(failure), nil)
	}
	if _, isCommand := actionCommandMap[action]; !isCommand {
		publishSessionControlSnapshot(ctx, strOr(sessionID), session, false, false)
	}
	return ok(rid, map[string]any{"control": control, "dispatch": dispatchEnvelope(actionResult)})
}

// Validated arguments of the only actions with input, or the 4004 error response
func validateActionArgs(rid any, action string, args map[string]any) (map[string]any, map[string]any) {
	switch action {
	case "subgoal.add":
		text, isText := args["text"].(string)
		text = strings.TrimSpace(text)
		if !isText || text == "" {
			return nil, rpcErr(rid, 4004, "subgoal text is required", nil)
		}
		return map[string]any{"text": text}, nil
	case "subgoal.remove":
		index, isInt := pyrepr.Int(args["index"])
		if !isInt {
			return nil, rpcErr(rid, 4004, "subgoal index must be an integer", nil)
		}
		if index.Sign() < 1 {
			return nil, rpcErr(rid, 4004, "subgoal index must be >= 1", nil)
		}
		return map[string]any{"index": index}, nil
	}
	return map[string]any{}, nil
}

// Delegates a fixed intent to the registered command dispatcher
func dispatchControlCommand(ctx context.Context, rid, sessionID any, name, arg string) (out map[string]any) {
	handler := lookupMethod("command.dispatch")
	if handler == nil {
		return rpcErr(rid, 5031, "command.dispatch unavailable", nil)
	}
	if failure := catchPanic(func() {
		out = handler(ctx, rid, map[string]any{"session_id": sessionID, "name": name, "arg": arg})
	}); failure != nil {
		logger().Debug("command.dispatch " + name + " " + arg + " failed: " + errorMessage(failure))
		return rpcErr(rid, 5031, "dispatch failed: "+errorMessage(failure), nil)
	}
	return out
}

// Runs a control that has no slash command through the subgoal or heartbeat manager
func executeManagerAction(ctx context.Context, sessionKey, action string, args map[string]any) (map[string]any, error) {
	if strings.HasPrefix(action, "subgoal.") {
		return executeSubgoalAction(ctx, sessionKey, action, args)
	}
	return executeHeartbeatAction(ctx, sessionKey, action), nil
}

// Adds, removes or clears a subgoal of the session goal
func executeSubgoalAction(ctx context.Context, sessionKey, action string, args map[string]any) (map[string]any, error) {
	manager := goals.NewGoalManager(ctx, sessionKey, 0)
	var output string
	switch action {
	case "subgoal.add":
		text, err := manager.AddSubgoal(args["text"].(string))
		if err != nil {
			return nil, err
		}
		output = fmt.Sprintf("✓ Added subgoal %d: %s", len(manager.State().Subgoals), text)
	case "subgoal.remove":
		index := args["index"].(*big.Int)
		position := int64(math.MaxInt64)
		if index.IsInt64() {
			position = index.Int64()
		}
		text, err := manager.RemoveSubgoal(position)
		if err != nil {
			return nil, err
		}
		output = fmt.Sprintf("✓ Removed subgoal %s: %s", index.String(), text)
	default:
		count, err := manager.ClearSubgoals()
		if err != nil {
			return nil, err
		}
		output = "No subgoals to clear."
		if count != 0 {
			plural := "s"
			if count == 1 {
				plural = ""
			}
			output = fmt.Sprintf("✓ Cleared %d subgoal%s.", count, plural)
		}
	}
	return map[string]any{"result": map[string]any{"type": "exec", "output": output}}, nil
}

// Pauses, resumes or clears the session heartbeat
func executeHeartbeatAction(ctx context.Context, sessionKey, action string) map[string]any {
	manager := agent.NewHeartbeatManager(ctx, sessionKey)
	var output string
	switch action {
	case "heartbeat.pause":
		output = "No heartbeat set."
		if state := manager.Pause(); state != nil {
			output = "⏸ Heartbeat paused: " + state.Prompt
		}
	case "heartbeat.resume":
		output = "No heartbeat to resume."
		if state := manager.Resume(); state != nil {
			output = fmt.Sprintf("▶ Heartbeat resumed (every %s): %s", agent.FormatHeartbeatInterval(state.IntervalSeconds), state.Prompt)
		}
	case "heartbeat.clear":
		output = "No heartbeat set."
		if manager.Clear() {
			output = "✓ Heartbeat cleared."
		}
	default:
		return rpcErr(nil, 4004, "unknown heartbeat action: "+action, nil)
	}
	return map[string]any{"result": map[string]any{"type": "exec", "output": output}}
}

// Python exception class an error stands for
func pyExceptionKind(err error) string {
	if e, isPy := err.(*pyint.Error); isPy {
		return e.Kind
	}
	return fmt.Sprintf("%T", err)
}

// Manager failure text prefixed with the slash form of the action
func managerErrorMessage(action string, err error) string {
	prefix := map[string]string{"subgoal.add": "/subgoal", "subgoal.remove": "/subgoal remove", "subgoal.clear": "/subgoal clear"}[action]
	if prefix == "" {
		prefix = action
	}
	return prefix + ": " + err.Error()
}

// User visible envelope of a command result without model facing data
func dispatchEnvelope(response map[string]any) map[string]any {
	result, _ := response["result"].(map[string]any)
	return map[string]any{"type": result["type"], "output": result["output"], "notice": result["notice"],
		"message": result["message"], "display": result["display"]}
}
