package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Manager call a viewer runs on the owner against the target's persisted state, assigned in cmd/icebath/wiring.go
var ViewerAutomationInvoke func(ctx context.Context, runner any, kind, target, call string, args []any, kwargs *pyjson.Object) (*pyjson.Object, error)

var viewerAutomationCalls = map[string]map[string]bool{
	"goal": nameSet("state", "set", "pause", "resume", "clear", "mark_done", "add_subgoal", "remove_subgoal",
		"clear_subgoals", "add_gate", "remove_gate", "clear_gates", "wait_on", "wait_on_session",
		"wait_for_seconds", "stop_waiting", "is_waiting", "evaluate_after_turn", "command"),
	"loop": nameSet("state", "set", "pause", "resume", "clear", "fire_tick", "abandon_tick", "complete_tick",
		"goal_blocks_tick"),
	"heartbeat": nameSet("state", "set", "pause", "resume", "clear", "due_prompt", "abandon_fire"),
}

var (
	viewerAutomationReads    = nameSet("state", "is_waiting", "goal_blocks_tick")
	viewerAutomationWatchdog = nameSet("set", "resume", "due_prompt")
)

// Kind, call, positional and keyword arguments of an automation request
func parseViewerAutomation(params *pyjson.Object) (string, string, []any, *pyjson.Object, error) {
	if paramsOutside(params, "session_id", "profile", "kind", "call", "args", "kwargs") {
		return "", "", nil, nil, runtimeRefusal("invalid_params")
	}
	kind, _ := params.GetOr("kind", nil).(string)
	call, _ := params.GetOr("call", nil).(string)
	rawArgs, rawKwargs := params.GetOr("args", nil), params.GetOr("kwargs", nil)
	var args []any
	if pyTruthy(rawArgs) {
		list, isList := rawArgs.([]any)
		if !isList {
			return "", "", nil, nil, runtimeRefusal("invalid_params")
		}
		args = list
	}
	kwargs := pyjson.NewObject()
	if pyTruthy(rawKwargs) {
		dict, isDict := rawKwargs.(*pyjson.Object)
		if !isDict {
			return "", "", nil, nil, runtimeRefusal("invalid_params")
		}
		kwargs = dict
	}
	calls, known := viewerAutomationCalls[kind]
	if !known || !calls[call] {
		return "", "", nil, nil, runtimeRefusal("invalid_params")
	}
	for _, key := range kwargs.Keys {
		if strings.HasPrefix(key, "_") {
			return "", "", nil, nil, runtimeRefusal("invalid_params")
		}
	}
	return kind, call, args, kwargs, nil
}

// Goal, loop and heartbeat meta rows of the target
func automationRows(ctx context.Context, a *SessionAuthority, target string) ([3]any, error) {
	var rows [3]any
	for i, kind := range []string{"goal", "loop", "heartbeat"} {
		value, ok, err := a.DB.GetMeta(ctx, kind+":"+target)
		if err != nil {
			return rows, err
		}
		if ok {
			rows[i] = value
		}
	}
	return rows, nil
}

// Session automation verb, the classic CLI's goal, loop and heartbeat manager calls run on the owner
func automation(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	kind, call, args, kwargs, err := parseViewerAutomation(params)
	if err != nil {
		return nil, err
	}
	if ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	a := c.authority
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.viewerAutomation == nil {
		a.viewerAutomation = map[string]*AuthorityConnection{}
	}
	a.viewerAutomation[ref.SessionID] = c
	a.mu.Unlock()
	var before *[3]any
	if viewerAutomationReads[call] {
		rows, err := automationRows(ctx, a, target)
		if err != nil {
			return nil, err
		}
		before = &rows
	}
	scoped, err := sessionWorkspace(profileRuntimeScope(ctx, a.ProfileID, true), a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	result, err := ViewerAutomationInvoke(scoped, a.Runner, kind, target, call, args, kwargs)
	if err != nil {
		return nil, err
	}
	if kind == "heartbeat" && viewerAutomationWatchdog[call] && (call == "due_prompt" || result.GetOr("value", nil) != nil) {
		a.mu.Lock()
		if a.viewerHeartbeat == nil {
			a.viewerHeartbeat = map[string]*AuthorityConnection{}
		}
		a.viewerHeartbeat[ref.SessionID] = c
		a.mu.Unlock()
	}
	changed := before == nil
	if !changed {
		after, err := automationRows(ctx, a, target)
		if err != nil {
			return nil, err
		}
		changed = after != *before
	}
	if changed {
		if err := PublishControl(ctx, a, ref.SessionID, false, false); err != nil {
			return nil, err
		}
	}
	return result, nil
}
