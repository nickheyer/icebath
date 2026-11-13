package agent

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One Python parameter of a manager method, keyword only or positional with its default
type automationParam struct {
	name     string
	kwOnly   bool
	required bool
	def      any
}

// Values of a call's parameters in declaration order, the TypeError Python raises for a call that does not fit
func bindAutomationCall(method string, params []automationParam, args []any, kwargs *pyjson.Object) ([]any, error) {
	typeError := func(format string, a ...any) error {
		return &pyint.Error{Kind: "TypeError", Msg: method + "() " + fmt.Sprintf(format, a...)}
	}
	positional := 0
	for _, p := range params {
		if !p.kwOnly {
			positional++
		}
	}
	if len(args) > positional {
		return nil, typeError("takes %d positional arguments but %d were given", positional+1, len(args)+1)
	}
	values := make([]any, len(params))
	set := make([]bool, len(params))
	for i, value := range args {
		values[i], set[i] = value, true
	}
	if kwargs != nil {
		for _, key := range kwargs.Keys {
			index := -1
			for i, p := range params {
				if p.name == key {
					index = i
				}
			}
			if index < 0 {
				return nil, typeError("got an unexpected keyword argument '%s'", key)
			}
			if set[index] {
				return nil, typeError("got multiple values for argument '%s'", key)
			}
			values[index], set[index] = kwargs.GetOr(key, nil), true
		}
	}
	for i, p := range params {
		if set[i] {
			continue
		}
		if p.required {
			kind := "positional"
			if p.kwOnly {
				kind = "keyword-only"
			}
			return nil, typeError("missing 1 required %s argument: '%s'", kind, p.name)
		}
		values[i] = p.def
	}
	return values, nil
}

// Text value of a bound argument, a TypeError for any other type
func automationText(method, name string, value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("%s() argument '%s' must be str, not %s", method, name, pyrepr.TypeName(value))}
	}
	return text, nil
}

// Integer value of a bound argument, a TypeError for any other type
func automationInt(method, name string, value any) (int64, error) {
	n, ok := value.(int64)
	if !ok {
		return 0, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("%s() argument '%s' must be int, not %s", method, name, pyrepr.TypeName(value))}
	}
	return n, nil
}

// Optional integer value of a bound argument, nil for None
func automationOptionalInt(method, name string, value any) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	n, err := automationInt(method, name, value)
	return &n, err
}

// Manager return value as JSON, states and gates keeping their own serialisation and containers wired item by item
func wireAutomationValue(value any) (any, error) {
	switch v := value.(type) {
	case nil, string, bool, int, int64, float64, *big.Int:
		return v, nil
	case *goals.GoalState:
		if v == nil {
			return nil, nil
		}
		text, err := v.ToJSON()
		return pyjson.ObjectOf("__state__", text), err
	case *SessionLoopState:
		if v == nil {
			return nil, nil
		}
		text, err := v.ToJSON()
		return pyjson.ObjectOf("__state__", text), err
	case *HeartbeatState:
		if v == nil {
			return nil, nil
		}
		text, err := v.ToJSON()
		return pyjson.ObjectOf("__state__", text), err
	case goals.GoalGate:
		return pyjson.ObjectOf("__gate__", v.ToDict()), nil
	case goals.GoalCommandResult:
		var prompt, clearPending any
		if v.Prompt != nil {
			prompt = *v.Prompt
		}
		if v.ClearPending != "" {
			clearPending = v.ClearPending
		}
		return pyjson.ObjectOf("output", v.Output, "prompt", prompt, "kickoff", v.Kickoff, "clear_pending", clearPending, "error", v.Error), nil
	case *pyjson.Object:
		if v == nil {
			return nil, nil
		}
		out := pyjson.NewObject()
		for _, key := range v.Keys {
			wired, err := wireAutomationValue(v.GetOr(key, nil))
			if err != nil {
				return nil, err
			}
			out.Set(key, wired)
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			wired, err := wireAutomationValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = wired
		}
		return out, nil
	}
	return pyrepr.Str(value), nil
}

// Python error class of a manager failure the call answers with, empty for one that propagates
func automationErrorClass(err error) string {
	var pyErr *pyint.Error
	if errors.As(err, &pyErr) {
		switch pyErr.Kind {
		case "RuntimeError", "ValueError", "IndexError":
			return pyErr.Kind
		}
	}
	var named interface{ PyTypeName() string }
	if errors.As(err, &named) {
		switch kind := named.PyTypeName(); kind {
		case "RuntimeError", "ValueError", "IndexError":
			return kind
		}
	}
	return ""
}

// Calls that are more than one manager method, the second result false for a plain method
func specialAutomationCall(ctx context.Context, mgr any, kind, target, call string, args []any, kwargs *pyjson.Object) (any, bool, error) {
	if kind == "goal" && call == "command" {
		padded := append(append([]any{}, args...), "", nil)
		arg := ""
		if pyrepr.Truthy(padded[0]) {
			arg = pyrepr.Str(padded[0])
		}
		result, err := goals.DispatchGoalCommand(mgr.(*goals.GoalManager), arg, goals.GoalCommandOptions{AuthorizeGate: func() string { return "" }, LastUserMessage: padded[1]})
		if err != nil {
			return nil, true, err
		}
		return result, true, nil
	}
	if kind == "goal" && call == "evaluate_after_turn" {
		last := ""
		if len(args) > 0 && pyrepr.Truthy(args[0]) {
			last = pyrepr.Str(args[0])
		}
		processes, delegations := func() (running []map[string]any, live int) {
			defer func() {
				if recover() != nil {
					running, live = nil, 0
				}
			}()
			return goals.GatherBackgroundProcesses(ctx, "", target), goals.CountActiveDelegations(target)
		}()
		decision, err := mgr.(*goals.GoalManager).EvaluateAfterTurn(last, processes, delegations)
		if err != nil {
			return nil, true, err
		}
		return decision.ToDict(), true, nil
	}
	if kind == "goal" && call == "set" && kwargs != nil {
		if raw, isDict := kwargs.GetOr("contract", nil).(*pyjson.Object); isDict {
			bound := kwargs.Copy()
			bound.Set("contract", goals.GoalContractFromDict(raw))
			value, err := invokeGoal(mgr.(*goals.GoalManager), call, args, bound)
			return value, true, err
		}
	}
	if kind == "loop" && call == "goal_blocks_tick" {
		return GoalBlocksLoopTick(ctx, target), true, nil
	}
	if call == "state" {
		return nil, true, nil
	}
	return nil, false, nil
}

// Goal manager call with its arguments bound like the Python method
func invokeGoal(mgr *goals.GoalManager, call string, args []any, kwargs *pyjson.Object) (any, error) {
	bind := func(params ...automationParam) ([]any, error) { return bindAutomationCall(call, params, args, kwargs) }
	req := func(name string) automationParam { return automationParam{name: name, required: true} }
	switch call {
	case "set":
		v, err := bind(req("goal"), automationParam{name: "max_turns", kwOnly: true}, automationParam{name: "contract", kwOnly: true})
		if err != nil {
			return nil, err
		}
		goal, err := automationText(call, "goal", v[0])
		if err != nil {
			return nil, err
		}
		maxTurns, err := automationOptionalInt(call, "max_turns", v[1])
		if err != nil {
			return nil, err
		}
		var turns int64
		if maxTurns != nil {
			turns = *maxTurns
		}
		var contract *goals.GoalContract
		if parsed, isContract := v[2].(goals.GoalContract); isContract {
			contract = &parsed
		}
		return mgr.Set(goal, turns, contract)
	case "pause":
		v, err := bind(automationParam{name: "reason", def: "user-paused"})
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[0])
		if err != nil {
			return nil, err
		}
		return mgr.Pause(reason), nil
	case "resume":
		v, err := bind(automationParam{name: "reset_budget", kwOnly: true, def: true})
		if err != nil {
			return nil, err
		}
		return mgr.Resume(pyrepr.Truthy(v[0])), nil
	case "clear":
		if _, err := bind(); err != nil {
			return nil, err
		}
		mgr.Clear()
		return nil, nil
	case "mark_done":
		v, err := bind(req("reason"))
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[0])
		if err != nil {
			return nil, err
		}
		mgr.MarkDone(reason)
		return nil, nil
	case "add_subgoal":
		v, err := bind(req("text"))
		if err != nil {
			return nil, err
		}
		text, err := automationText(call, "text", v[0])
		if err != nil {
			return nil, err
		}
		return mgr.AddSubgoal(text)
	case "remove_subgoal", "remove_gate":
		v, err := bind(req("index_1based"))
		if err != nil {
			return nil, err
		}
		index, err := automationInt(call, "index_1based", v[0])
		if err != nil {
			return nil, err
		}
		if call == "remove_gate" {
			return mgr.RemoveGate(index)
		}
		return mgr.RemoveSubgoal(index)
	case "clear_subgoals", "clear_gates":
		if _, err := bind(); err != nil {
			return nil, err
		}
		if call == "clear_gates" {
			return mgr.ClearGates()
		}
		return mgr.ClearSubgoals()
	case "add_gate":
		v, err := bind(req("command"), automationParam{name: "timeout_seconds", kwOnly: true}, automationParam{name: "max_retries", kwOnly: true})
		if err != nil {
			return nil, err
		}
		command, err := automationText(call, "command", v[0])
		if err != nil {
			return nil, err
		}
		timeout, err := automationOptionalInt(call, "timeout_seconds", v[1])
		if err != nil {
			return nil, err
		}
		retries, err := automationOptionalInt(call, "max_retries", v[2])
		if err != nil {
			return nil, err
		}
		var timeoutValue, retriesValue int64
		if timeout != nil {
			timeoutValue = *timeout
		}
		if retries != nil {
			retriesValue = *retries
		}
		return mgr.AddGate(command, timeoutValue, retriesValue)
	case "wait_on":
		v, err := bind(req("pid"), automationParam{name: "reason", def: ""})
		if err != nil {
			return nil, err
		}
		pid, err := automationInt(call, "pid", v[0])
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[1])
		if err != nil {
			return nil, err
		}
		return mgr.WaitOn(pid, reason)
	case "wait_on_session":
		v, err := bind(req("session_id"), automationParam{name: "reason", def: ""})
		if err != nil {
			return nil, err
		}
		sessionID, err := automationText(call, "session_id", v[0])
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[1])
		if err != nil {
			return nil, err
		}
		return mgr.WaitOnSession(sessionID, reason)
	case "wait_for_seconds":
		v, err := bind(req("seconds"), automationParam{name: "reason", def: ""}, automationParam{name: "on_delegations", kwOnly: true, def: int64(0)})
		if err != nil {
			return nil, err
		}
		seconds, err := automationInt(call, "seconds", v[0])
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[1])
		if err != nil {
			return nil, err
		}
		onDelegations, err := automationInt(call, "on_delegations", v[2])
		if err != nil {
			return nil, err
		}
		return mgr.WaitForSeconds(seconds, reason, onDelegations)
	case "stop_waiting":
		if _, err := bind(); err != nil {
			return nil, err
		}
		return mgr.StopWaiting(), nil
	case "is_waiting":
		if _, err := bind(); err != nil {
			return nil, err
		}
		return mgr.IsWaiting(), nil
	}
	return nil, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'GoalManager' object has no attribute '%s'", call)}
}

// Loop manager call with its arguments bound like the Python method
func invokeLoop(ctx context.Context, mgr *LoopManager, call string, args []any, kwargs *pyjson.Object) (any, error) {
	bind := func(params ...automationParam) ([]any, error) { return bindAutomationCall(call, params, args, kwargs) }
	switch call {
	case "set":
		v, err := bind(automationParam{name: "prompt", required: true}, automationParam{name: "interval_seconds", kwOnly: true},
			automationParam{name: "times", kwOnly: true, def: int64(0)}, automationParam{name: "until", kwOnly: true, def: ""},
			automationParam{name: "route", kwOnly: true})
		if err != nil {
			return nil, err
		}
		prompt := ""
		if v[0] != nil {
			if prompt, err = automationText(call, "prompt", v[0]); err != nil {
				return nil, err
			}
		}
		interval, err := automationOptionalInt(call, "interval_seconds", v[1])
		if err != nil {
			return nil, err
		}
		times, err := automationInt(call, "times", v[2])
		if err != nil {
			return nil, err
		}
		until, err := automationText(call, "until", v[3])
		if err != nil {
			return nil, err
		}
		route, _ := v[4].(*pyjson.Object)
		return mgr.Set(ctx, prompt, interval, times, until, route)
	case "pause":
		v, err := bind(automationParam{name: "reason", def: "user-paused"})
		if err != nil {
			return nil, err
		}
		reason, err := automationText(call, "reason", v[0])
		if err != nil {
			return nil, err
		}
		return mgr.Pause(ctx, reason)
	case "resume":
		if _, err := bind(); err != nil {
			return nil, err
		}
		return mgr.Resume(ctx), nil
	case "clear":
		if _, err := bind(); err != nil {
			return nil, err
		}
		return mgr.Clear(ctx), nil
	case "fire_tick":
		if _, err := bind(); err != nil {
			return nil, err
		}
		if prompt, ok := mgr.FireTick(ctx); ok {
			return prompt, nil
		}
		return nil, nil
	case "abandon_tick":
		if _, err := bind(); err != nil {
			return nil, err
		}
		mgr.AbandonTick(ctx)
		return nil, nil
	case "complete_tick":
		v, err := bind(automationParam{name: "last_response", required: true})
		if err != nil {
			return nil, err
		}
		last, err := automationText(call, "last_response", v[0])
		if err != nil {
			return nil, err
		}
		decision := mgr.CompleteTick(ctx, last)
		return pyjson.ObjectOf("status", decision.Status, "stopped", decision.Stopped, "reason", decision.Reason, "message", decision.Message), nil
	}
	return nil, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'LoopManager' object has no attribute '%s'", call)}
}

// Heartbeat manager call with its arguments bound like the Python method
func invokeHeartbeat(mgr *HeartbeatManager, call string, args []any, kwargs *pyjson.Object) (any, error) {
	bind := func(params ...automationParam) ([]any, error) { return bindAutomationCall(call, params, args, kwargs) }
	switch call {
	case "set":
		v, err := bind(automationParam{name: "prompt", required: true}, automationParam{name: "interval_seconds", required: true})
		if err != nil {
			return nil, err
		}
		prompt, err := automationText(call, "prompt", v[0])
		if err != nil {
			return nil, err
		}
		return mgr.Set(prompt, v[1])
	case "pause", "resume", "clear", "abandon_fire":
		if _, err := bind(); err != nil {
			return nil, err
		}
		switch call {
		case "pause":
			return mgr.Pause(), nil
		case "resume":
			return mgr.Resume(), nil
		case "clear":
			return mgr.Clear(), nil
		}
		return mgr.AbandonFire(), nil
	case "due_prompt":
		v, err := bind(automationParam{name: "now"})
		if err != nil {
			return nil, err
		}
		var now *float64
		switch t := v[0].(type) {
		case nil:
		case float64:
			now = &t
		case int64:
			f := float64(t)
			now = &f
		default:
			return nil, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("due_prompt() argument 'now' must be a number, not %s", pyrepr.TypeName(v[0]))}
		}
		if prompt, ok := mgr.DuePrompt(now); ok {
			return prompt, nil
		}
		return nil, nil
	}
	return nil, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'HeartbeatManager' object has no attribute '%s'", call)}
}

// Runs one viewer automation call against the target's persisted state, the value or the error with the state after
func InvokeViewerAutomation(ctx context.Context, runner any, kind, target, call string, args []any, kwargs *pyjson.Object) (*pyjson.Object, error) {
	var mgr any
	var plain func() (any, error)
	var stateJSON func() (any, error)
	switch kind {
	case "goal":
		maxTurns := int64(goals.DefaultGoalMaxTurns)
		if configured, ok := runner.(interface{ GoalMaxTurnsFromConfig(context.Context) int64 }); ok {
			maxTurns = configured.GoalMaxTurnsFromConfig(ctx)
		}
		goal := goals.NewGoalManager(ctx, target, maxTurns)
		mgr = goal
		plain = func() (any, error) { return invokeGoal(goal, call, args, kwargs) }
		stateJSON = func() (any, error) { return automationStateJSON(goal.State()) }
	case "loop":
		loop := NewLoopManager(ctx, target)
		mgr = loop
		plain = func() (any, error) { return invokeLoop(ctx, loop, call, args, kwargs) }
		stateJSON = func() (any, error) { return automationStateJSON(loop.State()) }
	default:
		heartbeat := NewHeartbeatManager(ctx, target)
		mgr = heartbeat
		plain = func() (any, error) { return invokeHeartbeat(heartbeat, call, args, kwargs) }
		stateJSON = func() (any, error) { return automationStateJSON(heartbeat.State()) }
	}
	value, special, err := specialAutomationCall(ctx, mgr, kind, target, call, args, kwargs)
	if !special {
		value, err = plain()
	}
	out := pyjson.NewObject()
	if err != nil {
		class := automationErrorClass(err)
		if class == "" {
			return nil, err
		}
		message := err.Error()
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) {
			message = pyErr.Msg
		}
		out.Set("error", pyjson.ObjectOf("type", class, "message", message))
	} else {
		wired, err := wireAutomationValue(value)
		if err != nil {
			return nil, err
		}
		out.Set("value", wired)
	}
	state, err := stateJSON()
	if err != nil {
		return nil, err
	}
	out.Set("state", state)
	return out, nil
}

// JSON text of a manager state, None when the manager holds none
func automationStateJSON(state interface{ ToJSON() (string, error) }) (any, error) {
	if state == nil || isNilState(state) {
		return nil, nil
	}
	return state.ToJSON()
}

// Whether a state interface holds a nil pointer
func isNilState(state any) bool {
	switch s := state.(type) {
	case *goals.GoalState:
		return s == nil
	case *SessionLoopState:
		return s == nil
	case *HeartbeatState:
		return s == nil
	}
	return false
}
