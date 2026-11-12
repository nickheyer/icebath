package goals

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Pause reason of a goal the user paused without naming one
const UserPausedReason = "user-paused"

// What the goal loop does after a turn, the status nil when the session has no goal
type GoalDecision struct {
	Status             any
	ShouldContinue     bool
	ContinuationPrompt *string
	Verdict            string
	Reason             string
	Message            string
}

// Decision fields as an ordered JSON object
func (d GoalDecision) ToDict() *pyjson.Object {
	var prompt any
	if d.ContinuationPrompt != nil {
		prompt = *d.ContinuationPrompt
	}
	return pyjson.ObjectOf("status", d.Status, "should_continue", d.ShouldContinue, "continuation_prompt", prompt,
		"verdict", d.Verdict, "reason", d.Reason, "message", d.Message)
}

// Per session goal state and the continuation decisions the CLI and the gateway act on
type GoalManager struct {
	SessionID       string
	DefaultMaxTurns int64
	ctx             context.Context
	state           *GoalState
}

// Manager of a session's goal loaded from the session store, a zero default meaning twenty turns
func NewGoalManager(ctx context.Context, sessionID string, defaultMaxTurns int64) *GoalManager {
	if defaultMaxTurns == 0 {
		defaultMaxTurns = DefaultGoalMaxTurns
	}
	return &GoalManager{SessionID: sessionID, DefaultMaxTurns: defaultMaxTurns, ctx: ctx, state: LoadGoal(ctx, sessionID)}
}

// Manager whose reads run on a state held by another process, nothing loaded from the session store
func MirrorGoalManager(ctx context.Context, sessionID string, state *GoalState) *GoalManager {
	return &GoalManager{SessionID: sessionID, DefaultMaxTurns: DefaultGoalMaxTurns, ctx: ctx, state: state}
}

// Calls of a goal manager the goal command and the CLI make, answered in process or by the gateway
type Manager interface {
	Session() string
	Context() context.Context
	Refresh()
	State() *GoalState
	IsActive() bool
	HasGoal() bool
	HasContract() bool
	StatusLine() string
	RenderSubgoals() string
	RenderGates() string
	RenderContract() string
	NextContinuationPrompt() *string
	Set(goal string, maxTurns int64, contract *GoalContract) (*GoalState, error)
	Pause(reason string) *GoalState
	Resume(resetBudget bool) *GoalState
	Clear()
	MarkDone(reason string)
	AddSubgoal(text string) (string, error)
	RemoveSubgoal(index1Based int64) (string, error)
	ClearSubgoals() (int, error)
	AddGate(command string, timeoutSeconds, maxRetries int64) (GoalGate, error)
	RemoveGate(index1Based int64) (string, error)
	ClearGates() (int, error)
	WaitOn(pid int64, reason string) (*GoalState, error)
	WaitOnSession(sessionID, reason string) (*GoalState, error)
	WaitForSeconds(seconds int64, reason string, onDelegations int64) (*GoalState, error)
	StopWaiting() bool
	IsWaiting() bool
	EvaluateAfterTurn(lastResponse string, backgroundProcesses []map[string]any, activeDelegations int) (GoalDecision, error)
}

var _ Manager = (*GoalManager)(nil)

// Session id the manager is bound to
func (m *GoalManager) Session() string { return m.SessionID }

// Context the manager reads the store and calls the judge under
func (m *GoalManager) Context() context.Context { return m.ctx }

// Reads the state again from the session store
func (m *GoalManager) Refresh() { m.state = LoadGoal(m.ctx, m.SessionID) }

// Current goal state, nil when the session has none
func (m *GoalManager) State() *GoalState { return m.state }

// True when the goal is active
func (m *GoalManager) IsActive() bool { return m.state != nil && m.state.Status == "active" }

// True when the goal is active or paused
func (m *GoalManager) HasGoal() bool {
	return m.state != nil && (m.state.Status == "active" || m.state.Status == "paused")
}

// True when the goal carries a completion contract
func (m *GoalManager) HasContract() bool { return m.state != nil && m.state.HasContract() }

// Seconds since the epoch as a float
func goalTime() float64 { return float64(goalNow().UnixNano()) / 1e9 }

// Count with its noun, plural unless the count is one
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Text of an optional stored value, the fallback when it is falsy
func textOr(v any, fallback string) string { return pyrepr.Str(pyrepr.Or(v, fallback)) }

// One line describing the goal, its budget and any wait barrier
func (m *GoalManager) StatusLine() string {
	s := m.state
	if s == nil || s.Status == "cleared" {
		return "No active goal. Set one with /goal <text>."
	}
	meta := fmt.Sprintf("%d/%d turns", s.TurnsUsed, s.MaxTurns)
	if len(s.Subgoals) > 0 {
		meta += ", " + countNoun(len(s.Subgoals), "subgoal")
	}
	if m.HasContract() {
		meta += ", contract"
	}
	if len(s.Gates) > 0 {
		meta += ", " + countNoun(len(s.Gates), "gate")
	}
	goal := pyrepr.Str(s.Goal)
	switch s.Status {
	case "active":
		if s.WaitingOnSession != nil && *s.WaitingOnSession != "" && goalSessionWaiting(*s.WaitingOnSession) {
			return fmt.Sprintf("⏳ Goal (parked on %s, %s): %s", textOr(s.WaitingReason, "session "+*s.WaitingOnSession), meta, goal)
		}
		if s.WaitingOnPID != nil && *s.WaitingOnPID != 0 && goalPidAlive(*s.WaitingOnPID) {
			return fmt.Sprintf("⏳ Goal (parked on %s, %s): %s", textOr(s.WaitingReason, fmt.Sprintf("pid %d", *s.WaitingOnPID)), meta, goal)
		}
		if s.WaitingUntil != 0 && goalTime() < s.WaitingUntil {
			remaining := int64(s.WaitingUntil - goalTime())
			return fmt.Sprintf("⏳ Goal (parked %ds — %s, %s): %s", remaining, textOr(s.WaitingReason, fmt.Sprintf("%ds", remaining)), meta, goal)
		}
		return fmt.Sprintf("⊙ Goal (active, %s): %s", meta, goal)
	case "paused":
		extra := ""
		if pyrepr.Truthy(s.PausedReason) {
			extra = " — " + pyrepr.Str(s.PausedReason)
		}
		return fmt.Sprintf("⏸ Goal (paused, %s%s): %s", meta, extra, goal)
	case "done":
		return fmt.Sprintf("✓ Goal done (%s): %s", meta, goal)
	}
	return fmt.Sprintf("Goal (%s, %s): %s", pyrepr.Str(s.Status), meta, goal)
}

// Persists the state and returns it
func (m *GoalManager) save() *GoalState {
	SaveGoal(m.ctx, m.SessionID, m.state)
	return m.state
}

// State of an active or paused goal, a RuntimeError when there is none
func (m *GoalManager) requireGoal() (*GoalState, error) {
	if m.state == nil || !m.HasGoal() {
		return nil, &pyint.Error{Kind: "RuntimeError", Msg: "no active goal"}
	}
	return m.state, nil
}

// State of an active goal, a RuntimeError when there is none to park
func (m *GoalManager) requireActive() (*GoalState, error) {
	if m.state == nil || m.state.Status != "active" {
		return nil, &pyint.Error{Kind: "RuntimeError", Msg: "no active goal to park"}
	}
	return m.state, nil
}

// Marks the goal paused with a reason and persists it
func (m *GoalManager) pauseState(reason string) {
	m.state.Status = "paused"
	m.state.PausedReason = reason
	m.save()
}

// Pauses the goal with a reason and returns the paused decision
func (m *GoalManager) pauseDecision(pausedReason, verdict, reason, message string) GoalDecision {
	m.pauseState(pausedReason)
	return GoalDecision{Status: "paused", Verdict: verdict, Reason: reason, Message: message}
}

// Starts a fresh active goal, a zero turn budget taking the manager default, a ValueError for empty text
func (m *GoalManager) Set(goal string, maxTurns int64, contract *GoalContract) (*GoalState, error) {
	goal = pystr.Strip(goal)
	if goal == "" {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "goal text is empty"}
	}
	if maxTurns == 0 {
		maxTurns = m.DefaultMaxTurns
	}
	m.state = NewGoalState(goal)
	m.state.CreatedAt, m.state.MaxTurns = goalTime(), maxTurns
	if contract != nil {
		m.state.Contract = *contract
	}
	return m.save(), nil
}

// Attaches or replaces the completion contract, nil when the session has no goal
func (m *GoalManager) SetContract(contract GoalContract) *GoalState {
	if m.state == nil {
		return nil
	}
	m.state.Contract = contract
	return m.save()
}

// Pauses the goal and lifts any wait barrier, nil when the session has no goal
func (m *GoalManager) Pause(reason string) *GoalState {
	if m.state == nil {
		return nil
	}
	m.state.Status = "paused"
	m.state.PausedReason = reason
	m.state.ClearWait()
	return m.save()
}

// Reactivates the goal with no wait barrier, resetting the turn budget when asked
func (m *GoalManager) Resume(resetBudget bool) *GoalState {
	if m.state == nil {
		return nil
	}
	m.state.Status = "active"
	m.state.PausedReason = nil
	m.state.ClearWait()
	if resetBudget {
		m.state.TurnsUsed = 0
	}
	return m.save()
}

// Marks the goal cleared in the store and drops it from the manager
func (m *GoalManager) Clear() {
	if m.state == nil {
		return
	}
	m.state.Status = "cleared"
	m.save()
	m.state = nil
}

// Marks the goal done with the reason as its last verdict
func (m *GoalManager) MarkDone(reason string) {
	if m.state == nil {
		return
	}
	m.state.Status = "done"
	m.state.LastVerdict = "done"
	m.state.LastReason = reason
	m.save()
}

// Appends a user criterion and returns it, a RuntimeError without a goal and a ValueError for empty text
func (m *GoalManager) AddSubgoal(text string) (string, error) {
	state, err := m.requireGoal()
	if err != nil {
		return "", err
	}
	text = pystr.Strip(text)
	if text == "" {
		return "", &pyint.Error{Kind: "ValueError", Msg: "subgoal text is empty"}
	}
	state.Subgoals = append(state.Subgoals, text)
	m.save()
	return text, nil
}

// Zero based position of a one based index, an IndexError outside the list
func goalIndex(index1Based int64, length int) (int, error) {
	idx := index1Based - 1
	if idx < 0 || idx >= int64(length) {
		return 0, &pyint.Error{Kind: "IndexError", Msg: fmt.Sprintf("index out of range (1..%d)", length)}
	}
	return int(idx), nil
}

// Removes the one based item of the subgoals or gates list and returns it, an IndexError outside the list
func (m *GoalManager) popItem(attr string, index1Based int64) (any, error) {
	state, err := m.requireGoal()
	if err != nil {
		return nil, err
	}
	var removed any
	switch attr {
	case "subgoals":
		idx, err := goalIndex(index1Based, len(state.Subgoals))
		if err != nil {
			return nil, err
		}
		removed = state.Subgoals[idx]
		state.Subgoals = append(state.Subgoals[:idx:idx], state.Subgoals[idx+1:]...)
	case "gates":
		idx, err := goalIndex(index1Based, len(state.Gates))
		if err != nil {
			return nil, err
		}
		removed = state.Gates[idx]
		state.Gates = append(state.Gates[:idx:idx], state.Gates[idx+1:]...)
	default:
		return nil, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'GoalState' object has no attribute '%s'", attr)}
	}
	m.save()
	return removed, nil
}

// Empties the subgoals or gates list and returns how many items it held
func (m *GoalManager) clearItems(attr string) (int, error) {
	state, err := m.requireGoal()
	if err != nil {
		return 0, err
	}
	previous := 0
	switch attr {
	case "subgoals":
		previous, state.Subgoals = len(state.Subgoals), []string{}
	case "gates":
		previous, state.Gates = len(state.Gates), []GoalGate{}
	default:
		return 0, &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'GoalState' object has no attribute '%s'", attr)}
	}
	m.save()
	return previous, nil
}

// Removes a subgoal by its one based index and returns its text
func (m *GoalManager) RemoveSubgoal(index1Based int64) (string, error) {
	removed, err := m.popItem("subgoals", index1Based)
	if err != nil {
		return "", err
	}
	return removed.(string), nil
}

// Removes every subgoal and returns how many there were
func (m *GoalManager) ClearSubgoals() (int, error) { return m.clearItems("subgoals") }

// Numbered subgoals for the subgoal command, a hint when there are none
func (m *GoalManager) RenderSubgoals() string {
	if m.state == nil {
		return "(no active goal)"
	}
	if block := m.state.RenderSubgoalsBlock(); block != "" {
		return block
	}
	return "(no subgoals — use /subgoal <text> to add criteria)"
}

// Appends a quality gate command, zero timeout or retries taking the defaults
func (m *GoalManager) AddGate(command string, timeoutSeconds, maxRetries int64) (GoalGate, error) {
	state, err := m.requireGoal()
	if err != nil {
		return GoalGate{}, err
	}
	command = pystr.Strip(command)
	if command == "" {
		return GoalGate{}, &pyint.Error{Kind: "ValueError", Msg: "gate command is empty"}
	}
	gate := NewGoalGate(command)
	if timeoutSeconds != 0 {
		gate.TimeoutSeconds = timeoutSeconds
	}
	if maxRetries != 0 {
		gate.MaxRetries = maxRetries
	}
	state.Gates = append(state.Gates, gate)
	m.save()
	return gate, nil
}

// Removes a gate by its one based index and returns its command
func (m *GoalManager) RemoveGate(index1Based int64) (string, error) {
	removed, err := m.popItem("gates", index1Based)
	if err != nil {
		return "", err
	}
	return removed.(GoalGate).Command, nil
}

// Removes every gate and returns how many there were
func (m *GoalManager) ClearGates() (int, error) { return m.clearItems("gates") }

// Numbered gates with their last result for the gate command
func (m *GoalManager) RenderGates() string {
	if m.state == nil {
		return "(no active goal)"
	}
	if len(m.state.Gates) == 0 {
		return "(no quality gates — use /goal gate add <command> to require one)"
	}
	lines := make([]string, len(m.state.Gates))
	for i, g := range m.state.Gates {
		status := ""
		if g.LastExitCode != nil && *g.LastExitCode == 0 {
			status = " ✓ passing"
		} else if g.LastExitCode != nil {
			status = fmt.Sprintf(" ✗ failing (exit %d, attempt %d/%d)", *g.LastExitCode, g.Attempts, g.MaxRetries)
		}
		lines[i] = fmt.Sprintf("- %d. $ %s%s", i+1, g.Command, status)
	}
	return strings.Join(lines, "\n")
}

// Decision of the first failing gate in order, nil when every gate passes or none exist
func (m *GoalManager) checkGates() (*GoalDecision, error) {
	state := m.state
	if state == nil || len(state.Gates) == 0 {
		return nil, nil
	}
	gateCwd, refusal := gateWorkspace(m.ctx)
	if refusal != "" {
		decision := m.pauseDecision("quality gates not run: "+refusal, "gate_failed", "gates not run: "+refusal,
			"⏸ Goal paused — quality gates not run: "+refusal+". Fix the workspace or /goal gate remove the gates, then /goal resume.")
		return &decision, nil
	}
	for i := range state.Gates {
		gate := &state.Gates[i]
		passed, exitCode, tail, interrupted := goalRunGate(m.ctx, *gate, gateCwd)
		if interrupted != nil {
			return nil, interrupted
		}
		gate.LastExitCode, gate.LastOutputTail = &exitCode, tail
		if passed {
			gate.Attempts = 0
			continue
		}
		gate.Attempts++
		if gate.Attempts > gate.MaxRetries {
			decision := m.pauseDecision(fmt.Sprintf("quality gate exhausted %d retries: $ %s", gate.Attempts-1, gate.Command),
				"gate_failed", "gate exhausted retries: $ "+gate.Command,
				fmt.Sprintf("⏸ Goal paused — quality gate still failing after %d retries: $ %s (exit %d). Fix it manually or /goal gate remove it, then /goal resume.",
					gate.MaxRetries, gate.Command, exitCode))
			return &decision, nil
		}
		m.save()
		output := tail
		if output == "" {
			output = "(no output)"
		}
		prompt := mustGoalFormat(continuationPromptGateFailedTemplate, map[string]any{"goal": state.Goal, "command": gate.Command, "exit_code": exitCode,
			"attempt": gate.Attempts, "max_retries": gate.MaxRetries, "output": output})
		return &GoalDecision{Status: "active", ShouldContinue: true, ContinuationPrompt: &prompt, Verdict: "gate_failed",
			Reason: fmt.Sprintf("gate failed (exit %d): $ %s", exitCode, gate.Command),
			Message: fmt.Sprintf("✗ Quality gate failed (%d/%d turns, attempt %d/%d): $ %s", state.TurnsUsed, state.MaxTurns, gate.Attempts,
				gate.MaxRetries, gate.Command)}, nil
	}
	m.save()
	return nil, nil
}

// Clears the barrier, applies the new one and stamps the reason and the park time
func (m *GoalManager) park(reason string, apply func(state *GoalState)) (*GoalState, error) {
	state, err := m.requireActive()
	if err != nil {
		return nil, err
	}
	state.ClearWait()
	apply(state)
	state.WaitingReason = nil
	if stripped := pystr.Strip(reason); stripped != "" {
		state.WaitingReason = stripped
	}
	state.WaitingSince = goalTime()
	return m.save(), nil
}

// Parks the loop until a background pid exits, a ValueError for a dead or invalid pid
func (m *GoalManager) WaitOn(pid int64, reason string) (*GoalState, error) {
	if _, err := m.requireActive(); err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "pid must be a positive integer"}
	}
	if !goalPidAlive(pid) {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "pid is not alive on this host"}
	}
	return m.park(reason, func(state *GoalState) { state.WaitingOnPID = &pid })
}

// Parks the loop on a process registry session's own trigger, its exit or a watch pattern match
func (m *GoalManager) WaitOnSession(sessionID, reason string) (*GoalState, error) {
	if _, err := m.requireActive(); err != nil {
		return nil, err
	}
	sessionID = pystr.Strip(sessionID)
	if sessionID == "" {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "session_id must be a non-empty string"}
	}
	return m.park(reason, func(state *GoalState) { state.WaitingOnSession = &sessionID })
}

// Parks the loop for seconds, lifting early once fewer than onDelegations batches are live
func (m *GoalManager) WaitForSeconds(seconds int64, reason string, onDelegations int64) (*GoalState, error) {
	return m.waitForSeconds(big.NewInt(seconds), reason, onDelegations)
}

// Seconds wait of any Python int size, the OverflowError float addition raises past the float range
func (m *GoalManager) waitForSeconds(seconds *big.Int, reason string, onDelegations int64) (*GoalState, error) {
	if _, err := m.requireActive(); err != nil {
		return nil, err
	}
	if seconds.Sign() <= 0 {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "seconds must be a positive integer"}
	}
	span, _ := new(big.Float).SetInt(seconds).Float64()
	if math.IsInf(span, 0) {
		return nil, &pyint.Error{Kind: "OverflowError", Msg: "int too large to convert to float"}
	}
	until := goalTime() + span
	return m.park(reason, func(state *GoalState) {
		state.WaitingUntil = until
		state.WaitingOnDelegations = max(0, onDelegations)
	})
}

// Clears any wait barrier, true when one was set
func (m *GoalManager) StopWaiting() bool {
	s := m.state
	if s == nil || (s.WaitingOnPID == nil && s.WaitingOnSession == nil && s.WaitingUntil == 0) {
		return false
	}
	s.ClearWait()
	m.save()
	return true
}

// True while a barrier is set and unmet, a met or overlong barrier cleared on the way
func (m *GoalManager) IsWaiting() bool {
	s := m.state
	if s == nil {
		return false
	}
	still := false
	switch {
	case s.WaitingOnSession != nil:
		still = goalSessionWaiting(*s.WaitingOnSession)
	case s.WaitingOnPID != nil:
		still = goalPidAlive(*s.WaitingOnPID)
	case s.WaitingUntil != 0:
		still = goalTime() < s.WaitingUntil
		if still && s.WaitingOnDelegations > 0 && int64(CountActiveDelegations(m.SessionID)) < s.WaitingOnDelegations {
			still = false
		}
	default:
		return false
	}
	if still && s.WaitingSince != 0 && s.WaitingUntil == 0.0 && goalTime()-s.WaitingSince > maxBarrierWaitS {
		var target any
		if s.WaitingOnSession != nil && *s.WaitingOnSession != "" {
			target = *s.WaitingOnSession
		} else if s.WaitingOnPID != nil {
			target = *s.WaitingOnPID
		}
		goalsLogger().Info(fmt.Sprintf("goal %s: wait barrier on %s exceeded %ds; resuming judging", m.SessionID, pyrepr.Str(target), maxBarrierWaitS))
		still = false
	}
	if !still {
		m.StopWaiting()
	}
	return still
}

// Decision of a turn boundary reached while the goal is parked
func (m *GoalManager) waitingDecision(state *GoalState) GoalDecision {
	target := ""
	switch {
	case state.WaitingOnSession != nil:
		target = "session " + *state.WaitingOnSession
	case state.WaitingOnPID != nil:
		target = fmt.Sprintf("pid %d", *state.WaitingOnPID)
	default:
		target = fmt.Sprintf("%ds remaining", max(0, int64(state.WaitingUntil-goalTime())))
	}
	reason := textOr(state.WaitingReason, target)
	return GoalDecision{Status: "active", Verdict: "waiting", Reason: reason, Message: fmt.Sprintf("⏳ Goal parked — waiting on %s: %s", target, reason)}
}

// Sets the barrier a judge wait verdict names and parks, nil when this host cannot observe the barrier lifting
func (m *GoalManager) applyWaitDirective(wait map[string]any, reason string, activeDelegations int) (*GoalDecision, error) {
	target := ""
	switch {
	case pyrepr.Truthy(wait["session_id"]):
		state, err := m.WaitOnSession(pyrepr.Str(wait["session_id"]), reason)
		if err != nil {
			return nil, err
		}
		target = "session " + *state.WaitingOnSession
	case pyrepr.Truthy(wait["pid"]):
		pid, err := pyint.Of(wait["pid"])
		if err != nil {
			return nil, err
		}
		var state *GoalState
		if pid.IsInt64() {
			state, err = goalManagerWaitOn(m, pid.Int64(), reason)
		} else {
			err = &pyint.Error{Kind: "ValueError", Msg: "pid is not alive on this host"}
		}
		if err != nil {
			if pyErr, isPy := err.(*pyint.Error); isPy && pyErr.Kind == "ValueError" {
				goalsLogger().Info(fmt.Sprintf("goal judge: wait_on_pid %s is not alive on this host; continuing", pid))
				return nil, nil
			}
			return nil, err
		}
		target = fmt.Sprintf("pid %d", *state.WaitingOnPID)
	default:
		seconds, present := wait["seconds"]
		if !present {
			return nil, &pyint.Error{Kind: "KeyError", Msg: "'seconds'"}
		}
		n, err := pyint.Of(seconds)
		if err != nil {
			return nil, err
		}
		if _, err := m.waitForSeconds(n, reason, int64(activeDelegations)); err != nil {
			return nil, err
		}
		target = pyrepr.Str(seconds) + "s"
	}
	return &GoalDecision{Status: "active", Verdict: "wait", Reason: reason, Message: fmt.Sprintf("⏳ Goal parked (judge) — waiting on %s: %s", target, reason)}, nil
}

// Pauses the goal because its turn budget is used up
func (m *GoalManager) budgetPause(state *GoalState, verdict, reason, note string) GoalDecision {
	return m.pauseDecision(fmt.Sprintf("turn budget exhausted (%d/%d)", state.TurnsUsed, state.MaxTurns), verdict, reason,
		fmt.Sprintf("⏸ Goal paused — %d/%d turns used%s. Use /goal resume to keep going, or /goal clear to stop.", state.TurnsUsed, state.MaxTurns, note))
}

// Runs the gates and the judge after a turn, updates the state and returns what the loop does next
func (m *GoalManager) EvaluateAfterTurn(lastResponse string, backgroundProcesses []map[string]any, activeDelegations int) (GoalDecision, error) {
	state := m.state
	if state == nil || state.Status != "active" {
		var status any
		if state != nil {
			status = state.Status
		}
		return GoalDecision{Status: status, Verdict: "inactive", Reason: "no active goal"}, nil
	}
	if m.IsWaiting() {
		return m.waitingDecision(state), nil
	}
	state.TurnsUsed++
	state.LastTurnAt = goalTime()
	gateDecision, err := m.checkGates()
	if err != nil {
		return GoalDecision{}, err
	}
	if gateDecision != nil {
		if gateDecision.ShouldContinue && state.TurnsUsed >= state.MaxTurns {
			return m.budgetPause(state, "gate_failed", gateDecision.Reason, " (a quality gate is still failing)"), nil
		}
		return *gateDecision, nil
	}
	goal, isText := state.Goal.(string)
	if !isText {
		return GoalDecision{}, strMethodError(state.Goal, "strip")
	}
	options := JudgeOptions{BackgroundProcesses: backgroundProcesses, ActiveDelegations: activeDelegations}
	if len(state.Subgoals) > 0 {
		options.Subgoals = state.Subgoals
	}
	if state.HasContract() {
		contract := state.Contract
		options.Contract = &contract
	}
	judged, err := judgeRaising(m.ctx, goal, lastResponse, options)
	if err != nil {
		return GoalDecision{}, err
	}
	verdict, reason := judged.Verdict, judged.Reason
	state.LastVerdict, state.LastReason = verdict, reason
	if judged.ParseFailed {
		state.ConsecutiveParseFailures++
	} else {
		state.ConsecutiveParseFailures = 0
	}
	if judged.TransportFailed {
		state.ConsecutiveTransportFailures++
	} else {
		state.ConsecutiveTransportFailures = 0
	}
	if verdict == "wait" && len(judged.Wait) > 0 {
		parked, err := m.applyWaitDirective(judged.Wait, reason, activeDelegations)
		if err != nil {
			return GoalDecision{}, err
		}
		if parked != nil {
			return *parked, nil
		}
	}
	if verdict == "blocked" {
		return m.pauseDecision("judged unachievable: "+reason, "blocked", reason,
			"🚫 Goal judged unachievable — paused: "+reason+" Re-scope with /goal set, or override with /goal resume."), nil
	}
	if verdict == "done" {
		state.Status = "done"
		m.save()
		return GoalDecision{Status: "done", Verdict: "done", Reason: reason, Message: "✓ Goal achieved: " + reason}, nil
	}
	transportFailures, parseFailures := state.ConsecutiveTransportFailures, state.ConsecutiveParseFailures
	if transportFailures >= DefaultMaxConsecutiveTransportFailures {
		return m.pauseDecision(fmt.Sprintf("judge API unreachable %d turns in a row (check auxiliary.goal_judge provider/key in config.yaml)", transportFailures),
			"continue", reason, fmt.Sprintf("⏸ Goal paused — judge API returned errors (%d turns). Check the goal_judge provider/key in ", transportFailures)+
				mustGoalFormat(judgeConfigHint, map[string]any{"provider": "deepseek", "model": "deepseek-flash"})), nil
	}
	if parseFailures >= DefaultMaxConsecutiveParseFailures {
		return m.pauseDecision(fmt.Sprintf("judge model returned unparseable output %d turns in a row", parseFailures), "continue", reason,
			fmt.Sprintf("⏸ Goal paused — the judge model (%d turns) isn't returning the required JSON verdict. Route the judge to a stricter model in ", parseFailures)+
				mustGoalFormat(judgeConfigHint, map[string]any{"provider": "openrouter", "model": "google/gemini-3-flash-preview"})), nil
	}
	if state.TurnsUsed >= state.MaxTurns {
		return m.budgetPause(state, "continue", reason, ""), nil
	}
	m.save()
	return GoalDecision{Status: "active", ShouldContinue: true, ContinuationPrompt: m.NextContinuationPrompt(), Verdict: "continue", Reason: reason,
		Message: fmt.Sprintf("↻ Continuing toward goal (%d/%d): %s", state.TurnsUsed, state.MaxTurns, reason)}, nil
}

// User role message that continues an active goal, the contract first and subgoals folded in, nil when not active
func (m *GoalManager) NextContinuationPrompt() *string {
	s := m.state
	if s == nil || s.Status != "active" {
		return nil
	}
	prompt := ""
	switch {
	case s.HasContract():
		contractBlock := s.Contract.RenderBlock()
		if len(s.Subgoals) > 0 {
			contractBlock += "\n" + renderExtraCriteria(s.Subgoals)
		}
		prompt = mustGoalFormat(continuationPromptWithContractTemplate, map[string]any{"goal": s.Goal, "contract_block": contractBlock})
	case len(s.Subgoals) > 0:
		prompt = mustGoalFormat(continuationPromptWithSubgoalsTemplate, map[string]any{"goal": s.Goal, "subgoals_block": s.RenderSubgoalsBlock()})
	default:
		prompt = mustGoalFormat(continuationPromptTemplate, map[string]any{"goal": s.Goal})
	}
	return &prompt
}

// Completion contract block for the goal show and draft commands, a hint when there is none
func (m *GoalManager) RenderContract() string {
	if m.state == nil {
		return "(no active goal)"
	}
	if m.state.HasContract() {
		return m.state.Contract.RenderBlock()
	}
	return "(no completion contract — set one with /goal draft <objective> or inline field: value lines)"
}

var (
	// Judge the manager and the kanban loop ask, replaced by tests the way the hermes cases patch judge_goal
	goalJudgeFn = JudgeGoal
	// Pid barrier the judge driven wait sets, replaced by tests that race the liveness check
	goalManagerWaitOn = (*GoalManager).WaitOn
)

var kanbanTerminalStatuses = map[string][3]string{
	"done":              {"completed_by_worker", "worker completed the task", "task {task_id} completed by worker after {turns} turn(s)"},
	"blocked":           {"blocked_by_worker", "worker blocked the task", "task {task_id} blocked by worker after {turns} turn(s)"},
	"review":            {"review_requested_by_worker", "worker requested review", "task {task_id} handed off for review by worker after {turns} turn(s)"},
	"changes_requested": {"changes_requested_by_reviewer", "reviewer requested changes", "reviewer returned task {task_id} for changes after {turns} turn(s)"},
}

// Inputs of the kanban worker goal loop, a turn budget below one meaning twenty
type GoalLoopOptions struct {
	TaskID        string
	GoalText      string
	RunTurn       func(prompt string) (any, error)
	TaskStatus    func() (*string, error)
	Block         func(reason string) error
	MaxTurns      int
	FirstResponse string
	Log           func(string)
}

// How the kanban worker goal loop ended
type GoalLoopResult struct {
	Outcome   string
	TurnsUsed int
	Reason    string
}

// Error of a callback that raised, a panic counting as the exception it carries
func callRaising(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, isErr := r.(error); isErr {
				err = e
				return
			}
			err = &pyint.Error{Kind: "Exception", Msg: fmt.Sprint(r)}
		}
	}()
	return fn()
}

// Drives a kanban worker until it ends its task, the judge blocks it, or the budget runs out
func RunKanbanGoalLoop(ctx context.Context, o GoalLoopOptions) (GoalLoopResult, error) {
	logLine := func(msg string) {
		if o.Log != nil {
			_ = callRaising(func() error { o.Log(msg); return nil })
		}
	}
	block := func(message string) {
		if err := callRaising(func() error { return o.Block(message) }); err != nil {
			logLine(fmt.Sprintf("kanban goal loop: block_fn failed (%s)", pyrepr.ExceptionMessage(err)))
		}
	}
	turnsUsed := 1
	result := func(outcome, reason string) (GoalLoopResult, error) {
		return GoalLoopResult{Outcome: outcome, TurnsUsed: turnsUsed, Reason: reason}, nil
	}
	maxTurns := o.MaxTurns
	if maxTurns < 1 {
		maxTurns = DefaultGoalMaxTurns
	}
	var lastResponse any = o.FirstResponse
	nudgedToFinalize := false
	for {
		var status *string
		if err := callRaising(func() (err error) { status, err = o.TaskStatus(); return err }); err != nil {
			logLine(fmt.Sprintf("kanban goal loop: status check failed (%s); stopping", pyrepr.ExceptionMessage(err)))
			return result("stopped", "status check failed")
		}
		if status != nil {
			if terminal, known := kanbanTerminalStatuses[*status]; known {
				logLine("kanban goal loop: " + mustGoalFormat(terminal[2], map[string]any{"task_id": o.TaskID, "turns": int64(turnsUsed)}))
				return result(terminal[0], terminal[1])
			}
		}
		if status == nil || (*status != "running" && *status != "ready") {
			shown, plain := "None", "None"
			if status != nil {
				shown, plain = pyrepr.Repr(*status), *status
			}
			logLine(fmt.Sprintf("kanban goal loop: task %s status=%s; stopping", o.TaskID, shown))
			return result("stopped", "status="+plain)
		}
		judgeCtx := ctx
		if provider.PortalAffinityScope(ctx) == "" {
			judgeCtx = provider.WithPortalAffinityScope(ctx, "kanban:"+o.TaskID)
		}
		response, isText := lastResponse.(string)
		if !isText && pystr.Strip(o.GoalText) != "" {
			return GoalLoopResult{}, strMethodError(lastResponse, "strip")
		}
		judged, err := judgeRaising(judgeCtx, o.GoalText, response, JudgeOptions{})
		if err != nil {
			return GoalLoopResult{}, err
		}
		verdict, reason := judged.Verdict, judged.Reason
		if judged.TransportFailed {
			logLine(fmt.Sprintf("kanban goal loop: judge transport failed on turn %d; stopping", turnsUsed))
			return result("stopped", "judge transport failure")
		}
		if verdict == "wait" {
			verdict = "continue"
		}
		logLine(fmt.Sprintf("kanban goal loop: turn %d/%d verdict=%s reason=%s", turnsUsed, maxTurns, verdict, goalTruncate(reason, 120)))
		if verdict == "blocked" {
			logLine(fmt.Sprintf("kanban goal loop: task %s judged unachievable; blocking", o.TaskID))
			block("Goal-mode judge ruled the goal unachievable: " + reason)
			return result("blocked_unachievable", "judge verdict blocked: "+reason)
		}
		prompt := ""
		if verdict == "done" {
			if nudgedToFinalize {
				logLine(fmt.Sprintf("kanban goal loop: task %s judged done but worker won't finalize; blocking", o.TaskID))
				block("Goal-mode worker's output looked complete but it never called kanban_complete after a finalize nudge (" + reason + ").")
				return result("blocked_budget", "judged done, never finalized")
			}
			prompt = mustGoalFormat(kanbanGoalFinalizeTemplate, map[string]any{"reason": goalTruncate(reason, 400)})
			nudgedToFinalize = true
		} else {
			prompt = mustGoalFormat(kanbanGoalContinuationTemplate, map[string]any{"reason": goalTruncate(reason, 400)})
		}
		if turnsUsed >= maxTurns {
			logLine(fmt.Sprintf("kanban goal loop: task %s exhausted %d/%d turns; blocking", o.TaskID, turnsUsed, maxTurns))
			block(fmt.Sprintf("Goal-mode worker exhausted its turn budget (%d/%d) without completing the task. Last judge verdict: %s",
				turnsUsed, maxTurns, goalTruncate(reason, 300)))
			return result("blocked_budget", "turn budget exhausted")
		}
		var turn any
		if err := callRaising(func() (err error) { turn, err = o.RunTurn(prompt); return err }); err != nil {
			logLine(fmt.Sprintf("kanban goal loop: run_turn failed (%s); stopping", pyrepr.ExceptionMessage(err)))
			return result("stopped", "run_turn error: "+pyrepr.ExceptionName(err))
		}
		failed, failureReason := false, ""
		if dict, isDict := turn.(map[string]any); isDict {
			response, present := dict["response"]
			if !present {
				response = ""
			}
			lastResponse = pyrepr.Or(response, "")
			failed = pyrepr.Truthy(dict["failed"])
			failureReason = pyrepr.Str(pyrepr.Or(dict["failure_reason"], "unknown"))
		} else {
			lastResponse = pyrepr.Or(turn, "")
		}
		if failed {
			logLine(fmt.Sprintf("kanban goal loop: worker failed on turn %d (reason=%s); stopping", turnsUsed, failureReason))
			return result("stopped", "worker failed: "+failureReason)
		}
		turnsUsed++
	}
}
