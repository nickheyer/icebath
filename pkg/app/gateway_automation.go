package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var gatewayAutomationLogger = slog.Default().With("logger", "hermes_cli.gateway_automation")

// Manager refusals the gateway answers with, raised here under the same class
var automationErrorKinds = map[string]bool{"RuntimeError": true, "ValueError": true, "IndexError": true}

// Value of key in a wire dict of either JSON shape, absent for any other value
func automationDictGet(value any, key string) (any, bool) {
	switch d := value.(type) {
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return d.Get(key)
	case map[string]any:
		v, ok := d[key]
		return v, ok
	}
	return nil, false
}

// Keys of a wire dict of either JSON shape, not ok for any other value
func automationDictKeys(value any) ([]string, bool) {
	switch d := value.(type) {
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return d.Keys, true
	case map[string]any:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		return keys, true
	}
	return nil, false
}

// Wire value with each state wrapper turned into its JSON text and each gate wrapper into a gate
func decodeAutomationValue(value any) (any, error) {
	if keys, isDict := automationDictKeys(value); isDict {
		if len(keys) == 1 && keys[0] == "__state__" {
			state, _ := automationDictGet(value, "__state__")
			return state, nil
		}
		if len(keys) == 1 && keys[0] == "__gate__" {
			raw, _ := automationDictGet(value, "__gate__")
			return goals.GoalGateFromDict(pyjson.Ordered(raw))
		}
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			raw, _ := automationDictGet(value, k)
			decoded, err := decodeAutomationValue(raw)
			if err != nil {
				return nil, err
			}
			out[k] = decoded
		}
		return out, nil
	}
	if list, isList := value.([]any); isList {
		out := make([]any, len(list))
		for i, item := range list {
			decoded, err := decodeAutomationValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = decoded
		}
		return out, nil
	}
	return value, nil
}

// Mirror of one gateway held automation row and the call that runs a manager method there
type automationRemote[S any] struct {
	ctx        context.Context
	sessionID  string
	kind       string
	connection *gatewaycli.GatewayConnection
	fromJSON   func(string) (*S, error)
	status     func(*S) any
	mu         sync.Mutex
	state      *S
}

// Answer of one remote call, the mirrored state when the gateway returned its row
type automationAnswer[S any] struct {
	value   any
	state   *S
	isState bool
}

// Mirrored state, nil when the session has none
func (r *automationRemote[S]) current() *S {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Replaces the mirror with the row the gateway returned, a cleared or absent row leaving none
func (r *automationRemote[S]) adopt(raw any) (*S, error) {
	var state *S
	if pyrepr.Truthy(raw) {
		text, isText := raw.(string)
		if !isText {
			return nil, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s", pyrepr.TypeName(raw))}
		}
		parsed, err := r.fromJSON(text)
		if err != nil {
			return nil, err
		}
		state = parsed
	}
	if state != nil && pyrepr.Equal(r.status(state), "cleared") {
		state = nil
	}
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
	return state, nil
}

// Runs one manager call on the gateway and adopts the row it returns, a dropped call answering nothing
func (r *automationRemote[S]) rpc(call string, args []any, kwargs map[string]any) (automationAnswer[S], error) {
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	reply, err := r.connection.Call(r.ctx, "session.automation", map[string]any{"session_id": r.sessionID, "kind": r.kind,
		"call": call, "args": args, "kwargs": kwargs})
	if err != nil {
		if !isGatewayClientError(err) {
			return automationAnswer[S]{}, err
		}
		gatewayAutomationLogger.Debug(fmt.Sprintf("session.automation %s.%s failed: %s", r.kind, call, err.Error()))
		return automationAnswer[S]{}, nil
	}
	rawState, _ := automationDictGet(reply, "state")
	state, err := r.adopt(rawState)
	if err != nil {
		return automationAnswer[S]{}, err
	}
	if refusal, _ := automationDictGet(reply, "error"); pyrepr.Truthy(refusal) {
		kind, _ := automationDictGet(refusal, "type")
		message, _ := automationDictGet(refusal, "message")
		class, _ := kind.(string)
		if !automationErrorKinds[class] {
			class = "RuntimeError"
		}
		return automationAnswer[S]{}, &pyint.Error{Kind: class, Msg: strOrEmpty(message)}
	}
	wire, _ := automationDictGet(reply, "value")
	value, err := decodeAutomationValue(wire)
	if err != nil {
		return automationAnswer[S]{}, err
	}
	if text, isText := value.(string); isText && text != "" && pyrepr.Equal(text, rawState) {
		return automationAnswer[S]{state: state, isState: true}, nil
	}
	return automationAnswer[S]{value: value}, nil
}

// State a call answered with, nil for no answer and an AttributeError for any other value
func (a automationAnswer[S]) stateValue() (*S, error) {
	if a.isState || a.value == nil {
		return a.state, nil
	}
	return nil, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'status'", pyrepr.TypeName(a.value))}
}

// Loads the gateway's row into the mirror
func (r *automationRemote[S]) refresh() error {
	_, err := r.rpc("state", nil, nil)
	return err
}

// State of a call whose local counterpart cannot fail, a failure raised as its panic
func mustAutomationState[S any](answer automationAnswer[S], err error) *S {
	if err != nil {
		panic(err)
	}
	state, err := answer.stateValue()
	if err != nil {
		panic(err)
	}
	return state
}

// Value of a call whose local counterpart cannot fail, a failure raised as its panic
func mustAutomationValue[S any](answer automationAnswer[S], err error) any {
	if err != nil {
		panic(err)
	}
	if answer.isState {
		if answer.state == nil {
			return nil
		}
		return answer.state
	}
	return answer.value
}

// Goal manager of a gateway session, every mutation and judgement one session.automation call
type GatewayGoalManager struct {
	remote *automationRemote[goals.GoalState]
}

var _ goals.Manager = (*GatewayGoalManager)(nil)

// Goal manager over the gateway connection with the session's row loaded
func NewGatewayGoalManager(ctx context.Context, sessionID string, connection *gatewaycli.GatewayConnection) (*GatewayGoalManager, error) {
	m := &GatewayGoalManager{remote: &automationRemote[goals.GoalState]{ctx: ctx, sessionID: sessionID, kind: "goal", connection: connection,
		fromJSON: goals.GoalStateFromJSON, status: func(s *goals.GoalState) any { return s.Status }}}
	return m, m.remote.refresh()
}

// Local manager whose reads run on the mirrored state
func (m *GatewayGoalManager) local() *goals.GoalManager {
	return goals.MirrorGoalManager(m.remote.ctx, m.remote.sessionID, m.remote.current())
}

func (m *GatewayGoalManager) Session() string          { return m.remote.sessionID }
func (m *GatewayGoalManager) Context() context.Context { return m.remote.ctx }
func (m *GatewayGoalManager) State() *goals.GoalState  { return m.remote.current() }
func (m *GatewayGoalManager) IsActive() bool           { return m.local().IsActive() }
func (m *GatewayGoalManager) HasGoal() bool            { return m.local().HasGoal() }
func (m *GatewayGoalManager) HasContract() bool        { return m.local().HasContract() }
func (m *GatewayGoalManager) StatusLine() string       { return m.local().StatusLine() }
func (m *GatewayGoalManager) RenderSubgoals() string   { return m.local().RenderSubgoals() }
func (m *GatewayGoalManager) RenderGates() string      { return m.local().RenderGates() }
func (m *GatewayGoalManager) RenderContract() string   { return m.local().RenderContract() }
func (m *GatewayGoalManager) NextContinuationPrompt() *string {
	return m.local().NextContinuationPrompt()
}

// Rereads the gateway's row into the mirror
func (m *GatewayGoalManager) Refresh() {
	if err := m.remote.refresh(); err != nil {
		panic(err)
	}
}

// Sets the goal on the gateway, a zero budget leaving the gateway's default
func (m *GatewayGoalManager) Set(goal string, maxTurns int64, contract *goals.GoalContract) (*goals.GoalState, error) {
	kwargs := map[string]any{}
	if maxTurns != 0 {
		kwargs["max_turns"] = maxTurns
	}
	if contract != nil {
		kwargs["contract"] = contract.ToDict()
	}
	answer, err := m.remote.rpc("set", []any{goal}, kwargs)
	if err != nil {
		return nil, err
	}
	return answer.stateValue()
}

func (m *GatewayGoalManager) Pause(reason string) *goals.GoalState {
	return mustAutomationState(m.remote.rpc("pause", nil, map[string]any{"reason": reason}))
}

func (m *GatewayGoalManager) Resume(resetBudget bool) *goals.GoalState {
	return mustAutomationState(m.remote.rpc("resume", nil, map[string]any{"reset_budget": resetBudget}))
}

func (m *GatewayGoalManager) Clear() { mustAutomationValue(m.remote.rpc("clear", nil, nil)) }

func (m *GatewayGoalManager) MarkDone(reason string) {
	mustAutomationValue(m.remote.rpc("mark_done", []any{reason}, nil))
}

// Text of a value a call answered with, the None text for no answer
func automationText[S any](answer automationAnswer[S], err error) (string, error) {
	if err != nil {
		return "", err
	}
	if answer.isState {
		return pyrepr.Str(nil), nil
	}
	return pyrepr.Str(answer.value), nil
}

// Count a call answered with, zero for no answer
func automationCount[S any](answer automationAnswer[S], err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n, _ := pyIntValue(answer.value)
	return n, nil
}

func (m *GatewayGoalManager) AddSubgoal(text string) (string, error) {
	return automationText(m.remote.rpc("add_subgoal", []any{text}, nil))
}

func (m *GatewayGoalManager) RemoveSubgoal(index1Based int64) (string, error) {
	return automationText(m.remote.rpc("remove_subgoal", []any{index1Based}, nil))
}

func (m *GatewayGoalManager) ClearSubgoals() (int, error) {
	return automationCount(m.remote.rpc("clear_subgoals", nil, nil))
}

// Adds a quality gate on the gateway, zero timeout and retries leaving the gateway's defaults
func (m *GatewayGoalManager) AddGate(command string, timeoutSeconds, maxRetries int64) (goals.GoalGate, error) {
	kwargs := map[string]any{}
	if timeoutSeconds != 0 {
		kwargs["timeout_seconds"] = timeoutSeconds
	}
	if maxRetries != 0 {
		kwargs["max_retries"] = maxRetries
	}
	answer, err := m.remote.rpc("add_gate", []any{command}, kwargs)
	if err != nil {
		return goals.GoalGate{}, err
	}
	gate, isGate := answer.value.(goals.GoalGate)
	if !isGate {
		return goals.GoalGate{}, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'command'", pyrepr.TypeName(mustAutomationValue(answer, nil)))}
	}
	return gate, nil
}

func (m *GatewayGoalManager) RemoveGate(index1Based int64) (string, error) {
	return automationText(m.remote.rpc("remove_gate", []any{index1Based}, nil))
}

func (m *GatewayGoalManager) ClearGates() (int, error) {
	return automationCount(m.remote.rpc("clear_gates", nil, nil))
}

// State a parking call answered with
func (m *GatewayGoalManager) parked(call string, args []any, kwargs map[string]any) (*goals.GoalState, error) {
	answer, err := m.remote.rpc(call, args, kwargs)
	if err != nil {
		return nil, err
	}
	return answer.stateValue()
}

func (m *GatewayGoalManager) WaitOn(pid int64, reason string) (*goals.GoalState, error) {
	return m.parked("wait_on", []any{pid}, map[string]any{"reason": reason})
}

func (m *GatewayGoalManager) WaitOnSession(sessionID, reason string) (*goals.GoalState, error) {
	return m.parked("wait_on_session", []any{sessionID}, map[string]any{"reason": reason})
}

func (m *GatewayGoalManager) WaitForSeconds(seconds int64, reason string, onDelegations int64) (*goals.GoalState, error) {
	return m.parked("wait_for_seconds", []any{seconds}, map[string]any{"reason": reason, "on_delegations": onDelegations})
}

func (m *GatewayGoalManager) StopWaiting() bool {
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("stop_waiting", nil, nil)))
}

// Barrier check asked of the gateway, a state with no barrier answered locally with no round trip
func (m *GatewayGoalManager) IsWaiting() bool {
	s := m.remote.current()
	if s == nil || (s.WaitingOnPID == nil && s.WaitingOnSession == nil && s.WaitingUntil == 0) {
		return false
	}
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("is_waiting", nil, nil)))
}

// Runs a whole goal command on the gateway, its contract drafter being a model call there
func (m *GatewayGoalManager) Command(arg string, lastUserMessage any) (goals.GoalCommandResult, error) {
	answer, err := m.remote.rpc("command", []any{arg, lastUserMessage}, nil)
	if err != nil {
		return goals.GoalCommandResult{}, err
	}
	fields, isDict := answer.value.(map[string]any)
	if !isDict || answer.isState {
		return goals.GoalCommandResult{Output: "Goal unavailable: the gateway did not answer.", Error: true}, nil
	}
	output, hasOutput := fields["output"]
	if !hasOutput {
		return goals.GoalCommandResult{}, &pyint.Error{Kind: "TypeError", Msg: "GoalCommandResult.__init__() missing 1 required positional argument: 'output'"}
	}
	result := goals.GoalCommandResult{Output: pyrepr.Str(output), Kickoff: pyrepr.Truthy(fields["kickoff"]), Error: pyrepr.Truthy(fields["error"])}
	if prompt := fields["prompt"]; prompt != nil {
		text := pyrepr.Str(prompt)
		result.Prompt = &text
	}
	if pending := fields["clear_pending"]; pending != nil {
		result.ClearPending = pyrepr.Str(pending)
	}
	return result, nil
}

// Judges the turn on the gateway with the gateway's background processes and delegations
func (m *GatewayGoalManager) EvaluateAfterTurn(lastResponse string, backgroundProcesses []map[string]any, activeDelegations int) (goals.GoalDecision, error) {
	answer, err := m.remote.rpc("evaluate_after_turn", []any{lastResponse}, nil)
	if err != nil {
		return goals.GoalDecision{}, err
	}
	fields, isDict := answer.value.(map[string]any)
	if !isDict || answer.isState {
		return goals.GoalDecision{Verdict: "inactive", Reason: "gateway unavailable"}, nil
	}
	decision := goals.GoalDecision{Status: fields["status"], ShouldContinue: pyrepr.Truthy(fields["should_continue"]),
		Verdict: strOrEmpty(fields["verdict"]), Reason: strOrEmpty(fields["reason"]), Message: strOrEmpty(fields["message"])}
	if prompt := fields["continuation_prompt"]; prompt != nil {
		text := pyrepr.Str(prompt)
		decision.ContinuationPrompt = &text
	}
	return decision, nil
}

// Loop manager of a gateway session, every mutation and tick one session.automation call
type GatewayLoopManager struct {
	remote *automationRemote[agent.SessionLoopState]
}

var _ agent.LoopAutomation = (*GatewayLoopManager)(nil)

// Loop manager over the gateway connection with the session's row loaded
func NewGatewayLoopManager(ctx context.Context, sessionID string, connection *gatewaycli.GatewayConnection) (*GatewayLoopManager, error) {
	m := &GatewayLoopManager{remote: &automationRemote[agent.SessionLoopState]{ctx: ctx, sessionID: sessionID, kind: "loop", connection: connection,
		fromJSON: agent.SessionLoopStateFromJSON, status: func(s *agent.SessionLoopState) any { return s.Status }}}
	return m, m.remote.refresh()
}

// Local manager whose reads run on the mirrored state
func (m *GatewayLoopManager) local() *agent.LoopManager {
	return agent.MirrorLoopManager(m.remote.sessionID, m.remote.current())
}

func (m *GatewayLoopManager) Session() string                { return m.remote.sessionID }
func (m *GatewayLoopManager) State() *agent.SessionLoopState { return m.remote.current() }
func (m *GatewayLoopManager) IsActive() bool                 { return m.local().IsActive() }
func (m *GatewayLoopManager) HasLoop() bool                  { return m.local().HasLoop() }
func (m *GatewayLoopManager) StatusLine() string             { return m.local().StatusLine() }
func (m *GatewayLoopManager) IsDue() bool                    { return m.local().IsDue() }

// Rereads the gateway's row into the mirror
func (m *GatewayLoopManager) Refresh(context.Context) {
	if err := m.remote.refresh(); err != nil {
		panic(err)
	}
}

// Sets the loop on the gateway, the route staying the gateway's own
func (m *GatewayLoopManager) Set(_ context.Context, prompt string, intervalSeconds *int64, times int64, until string, _ *pyjson.Object) (*agent.SessionLoopState, error) {
	var interval any
	if intervalSeconds != nil {
		interval = *intervalSeconds
	}
	answer, err := m.remote.rpc("set", []any{prompt}, map[string]any{"interval_seconds": interval, "times": times, "until": until})
	if err != nil {
		return nil, err
	}
	return answer.stateValue()
}

func (m *GatewayLoopManager) Pause(_ context.Context, reason string) (*agent.SessionLoopState, error) {
	answer, err := m.remote.rpc("pause", nil, map[string]any{"reason": reason})
	if err != nil {
		return nil, err
	}
	return answer.stateValue()
}

func (m *GatewayLoopManager) Resume(context.Context) *agent.SessionLoopState {
	return mustAutomationState(m.remote.rpc("resume", nil, nil))
}

func (m *GatewayLoopManager) Clear(context.Context) bool {
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("clear", nil, nil)))
}

// Claims a due tick on the gateway and returns its wakeup, false when the gateway fired none
func (m *GatewayLoopManager) FireTick(context.Context) (string, bool) {
	value := mustAutomationValue(m.remote.rpc("fire_tick", nil, nil))
	if value == nil {
		return "", false
	}
	return pyrepr.Str(value), true
}

func (m *GatewayLoopManager) AbandonTick(context.Context) {
	mustAutomationValue(m.remote.rpc("abandon_tick", nil, nil))
}

// Completes the fired tick on the gateway, an inert decision when the gateway did not answer
func (m *GatewayLoopManager) CompleteTick(_ context.Context, lastResponse string) agent.LoopDecision {
	fields, isDict := mustAutomationValue(m.remote.rpc("complete_tick", []any{lastResponse}, nil)).(map[string]any)
	if !isDict {
		return agent.LoopDecision{Reason: "gateway unavailable"}
	}
	return agent.LoopDecision{Status: fields["status"], Stopped: pyrepr.Truthy(fields["stopped"]),
		Reason: strOrEmpty(fields["reason"]), Message: strOrEmpty(fields["message"])}
}

// Whether the session's goal defers the loop tick, read where the goal row and its barrier live
func (m *GatewayLoopManager) GoalBlocksTick() bool {
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("goal_blocks_tick", nil, nil)))
}

// Heartbeat manager of a gateway session, every mutation and claim one session.automation call
type GatewayHeartbeatManager struct {
	remote *automationRemote[agent.HeartbeatState]
}

var _ agent.HeartbeatAutomation = (*GatewayHeartbeatManager)(nil)

// Heartbeat manager over the gateway connection with the session's row loaded
func NewGatewayHeartbeatManager(ctx context.Context, sessionID string, connection *gatewaycli.GatewayConnection) (*GatewayHeartbeatManager, error) {
	m := &GatewayHeartbeatManager{remote: &automationRemote[agent.HeartbeatState]{ctx: ctx, sessionID: sessionID, kind: "heartbeat", connection: connection,
		fromJSON: agent.HeartbeatStateFromJSON, status: func(s *agent.HeartbeatState) any { return s.Status }}}
	return m, m.remote.refresh()
}

// Local manager whose reads run on the mirrored state
func (m *GatewayHeartbeatManager) local() *agent.HeartbeatManager {
	return agent.MirrorHeartbeatManager(m.remote.ctx, m.remote.sessionID, m.remote.current())
}

func (m *GatewayHeartbeatManager) Session() string              { return m.remote.sessionID }
func (m *GatewayHeartbeatManager) State() *agent.HeartbeatState { return m.remote.current() }
func (m *GatewayHeartbeatManager) HasHeartbeat() bool           { return m.local().HasHeartbeat() }
func (m *GatewayHeartbeatManager) IsActive() bool               { return m.local().IsActive() }
func (m *GatewayHeartbeatManager) StatusLine() string           { return m.local().StatusLine() }

// Rereads the gateway's row into the mirror
func (m *GatewayHeartbeatManager) Refresh() {
	if err := m.remote.refresh(); err != nil {
		panic(err)
	}
}

// Sets the heartbeat on the gateway, its refusal raised as the local manager's ValueError
func (m *GatewayHeartbeatManager) Set(prompt string, intervalSeconds int64) (*agent.HeartbeatState, error) {
	answer, err := m.remote.rpc("set", []any{prompt, intervalSeconds}, nil)
	if err != nil {
		var refusal *pyint.Error
		if errors.As(err, &refusal) && refusal.Kind == "ValueError" {
			return nil, &agent.HeartbeatValueError{Msg: refusal.Msg}
		}
		return nil, err
	}
	return answer.stateValue()
}

func (m *GatewayHeartbeatManager) Pause() *agent.HeartbeatState {
	return mustAutomationState(m.remote.rpc("pause", nil, nil))
}

func (m *GatewayHeartbeatManager) Resume() *agent.HeartbeatState {
	return mustAutomationState(m.remote.rpc("resume", nil, nil))
}

func (m *GatewayHeartbeatManager) Clear() bool {
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("clear", nil, nil)))
}

// Claims a due tick on the gateway after the local due check, false when nothing fired
func (m *GatewayHeartbeatManager) DuePrompt(now *float64) (string, bool) {
	at := nowSeconds()
	if now != nil {
		at = *now
	}
	s := m.remote.current()
	if s == nil || !s.IsDue(at) {
		return "", false
	}
	value := mustAutomationValue(m.remote.rpc("due_prompt", nil, nil))
	if value == nil {
		return "", false
	}
	return pyrepr.Str(value), true
}

func (m *GatewayHeartbeatManager) AbandonFire() bool {
	return pyrepr.Truthy(mustAutomationValue(m.remote.rpc("abandon_fire", nil, nil)))
}

// Runs a goal command for the classic CLI, the draft form whole on a gateway manager
func dispatchGoalCommand(m goals.Manager, arg string, o goals.GoalCommandOptions) (goals.GoalCommandResult, error) {
	if remote, isRemote := m.(*GatewayGoalManager); isRemote {
		if words := pystr.SplitWhitespace(arg, 1); len(words) > 0 && words[0] == "draft" {
			if o.Progress != nil {
				o.Progress("Drafting completion contract…")
			}
			return remote.Command(arg, o.LastUserMessage)
		}
	}
	return goals.DispatchGoalCommand(m, arg, o)
}

// The CLI's gateway connection when its session store is the gateway's and its session is open, nil otherwise
func gatewayAutomationConnection(c *CLI) *gatewaycli.GatewayConnection {
	if c.sessionDB != nil && c.gatewaySessionOpen {
		return c.sessionDB.Connection
	}
	return nil
}
