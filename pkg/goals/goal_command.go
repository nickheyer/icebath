package goals

import (
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Outcome of one goal command, the prompt nil when no turn should follow
type GoalCommandResult struct {
	Output       string
	Prompt       *string
	Kickoff      bool
	ClearPending string
	Error        bool
}

// Renders a user facing goal message from its translation key, English default and values
type GoalRender func(key, def string, values map[string]any) string

func goalCommandLogger() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.goal_command")
}

// English rendering that fills the default template and ignores the key
func EnglishGoalRender(key, def string, values map[string]any) string {
	return mustGoalFormat(def, values)
}

type goalHandler func(m Manager, arg string, render GoalRender) (GoalCommandResult, error)

func goalStatus(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	return GoalCommandResult{Output: m.StatusLine()}, nil
}

func goalShow(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	return GoalCommandResult{Output: m.StatusLine() + "\n" + m.RenderContract()}, nil
}

func goalPause(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	state := m.Pause(UserPausedReason)
	if state == nil {
		return GoalCommandResult{Output: render("gateway.goal.no_goal_set", "No goal set.", nil)}, nil
	}
	return GoalCommandResult{Output: render("gateway.goal.paused", "⏸ Goal paused: {goal}", map[string]any{"goal": state.Goal}), ClearPending: "pause"}, nil
}

func goalResume(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	state := m.Resume(true)
	if state == nil {
		return GoalCommandResult{Output: render("gateway.goal.no_resume", "No goal to resume.", nil)}, nil
	}
	return GoalCommandResult{Output: render("gateway.goal.resumed", "▶ Goal resumed: {goal}", map[string]any{"goal": state.Goal}),
		Prompt: m.NextContinuationPrompt()}, nil
}

func goalClear(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	had := m.HasGoal()
	m.Clear()
	if had {
		return GoalCommandResult{Output: render("gateway.goal_cleared", "✓ Goal cleared.", nil), ClearPending: "clear"}, nil
	}
	return GoalCommandResult{Output: render("gateway.no_active_goal", "No active goal.", nil), ClearPending: "clear"}, nil
}

func goalUnwait(m Manager, arg string, render GoalRender) (GoalCommandResult, error) {
	if m.StopWaiting() {
		return GoalCommandResult{Output: "▶ Wait barrier cleared — goal loop resumes."}, nil
	}
	return GoalCommandResult{Output: "No wait barrier set."}, nil
}

// Python split of text on whitespace into at most two parts
func splitOnce(text string) []string { return pystr.SplitWhitespace(text, 1) }

// Parks the goal on the pid the command names, a usage error for a missing or non integer pid
func goalWait(m Manager, arg string) (GoalCommandResult, error) {
	if arg == "" {
		return GoalCommandResult{Output: "Usage: /goal wait <pid> [reason]", Error: true}, nil
	}
	tokens := splitOnce(arg)
	pid, err := pyint.Of(tokens[0])
	if err != nil {
		return GoalCommandResult{Output: "/goal wait: <pid> must be an integer process id.", Error: true}, nil
	}
	reason := ""
	if len(tokens) > 1 {
		reason = pystr.Strip(tokens[1])
	}
	switch {
	case pid.IsInt64():
		if _, err := m.WaitOn(pid.Int64(), reason); err != nil {
			return GoalCommandResult{}, err
		}
	case pid.Sign() <= 0:
		return GoalCommandResult{}, &pyint.Error{Kind: "ValueError", Msg: "pid must be a positive integer"}
	default:
		if state := m.State(); state == nil || state.Status != "active" {
			return GoalCommandResult{}, &pyint.Error{Kind: "RuntimeError", Msg: "no active goal to park"}
		}
		return GoalCommandResult{}, &pyint.Error{Kind: "ValueError", Msg: "pid is not alive on this host"}
	}
	suffix := ""
	if reason != "" {
		suffix = " (" + reason + ")"
	}
	return GoalCommandResult{Output: fmt.Sprintf("⏳ Goal parked on pid %s%s. Loop pauses until it exits.", pid, suffix)}, nil
}

func goalGateAdd(m Manager, arg string) (GoalCommandResult, error) {
	gate, err := m.AddGate(arg, 0, 0)
	if err != nil {
		return GoalCommandResult{}, err
	}
	return GoalCommandResult{Output: fmt.Sprintf("⚿ Gate added: $ %s (%d retries, %ds timeout). It must pass before the goal can complete.",
		gate.Command, gate.MaxRetries, gate.TimeoutSeconds)}, nil
}

func goalGateRemove(m Manager, arg string) (GoalCommandResult, error) {
	index, err := pyint.Of(arg)
	if err != nil {
		return GoalCommandResult{}, err
	}
	var command string
	if index.IsInt64() {
		command, err = m.RemoveGate(index.Int64())
	} else if !m.HasGoal() {
		err = &pyint.Error{Kind: "RuntimeError", Msg: "no active goal"}
	} else {
		_, err = goalIndex(-1, len(m.State().Gates))
	}
	if err != nil {
		return GoalCommandResult{}, err
	}
	return GoalCommandResult{Output: "✓ Gate removed: $ " + command}, nil
}

func goalGateClear(m Manager, arg string) (GoalCommandResult, error) {
	count, err := m.ClearGates()
	if err != nil {
		return GoalCommandResult{}, err
	}
	return GoalCommandResult{Output: "✓ Cleared " + countNoun(count, "gate") + "."}, nil
}

var (
	goalGateHandlers = map[string]func(Manager, string) (GoalCommandResult, error){
		"add": goalGateAdd, "remove": goalGateRemove, "rm": goalGateRemove, "clear": goalGateClear,
	}
	goalExactHandlers = map[string]goalHandler{
		"": goalStatus, "status": goalStatus, "show": goalShow, "pause": goalPause,
		"resume": goalResume, "continue": goalResume, "unpause": goalResume,
		"clear": goalClear, "stop": goalClear, "done": goalClear, "unwait": goalUnwait,
	}
	goalBareControlVerbs   = map[string]bool{"resume": true, "continue": true, "unpause": true, "pause": true, "status": true, "show": true, "unwait": true}
	goalControlFirstWords  = map[string]bool{"resume": true, "continue": true, "unpause": true, "pause": true, "status": true, "show": true, "unwait": true, "wait": true, "gate": true}
	goalCommandDraft       = DraftContract
	goalCommandErrorKinds  = map[string]bool{"RuntimeError": true, "ValueError": true, "IndexError": true}
	goalDraftingInProgress = "Drafting completion contract…"
)

// Control verb result with the trailing words noted as ignored
func goalBareControl(m Manager, verb, rest string, render GoalRender) (GoalCommandResult, error) {
	result, err := goalExactHandlers[verb](m, "", render)
	if err != nil {
		return result, err
	}
	result.Output += fmt.Sprintf("\n(ignored %s: a control command never sets goal text — use /goal -- <text> when the goal really starts with a control word.)", pyrepr.Repr(rest))
	return result, nil
}

// Gate subcommand dispatch, gate creation only after the adapter authorizes it
func goalGate(m Manager, arg string, authorizeGate func() string) (GoalCommandResult, error) {
	if arg == "" || pystr.Lower(arg) == "list" {
		return GoalCommandResult{Output: m.RenderGates()}, nil
	}
	tokens := splitOnce(arg)
	verb, rest := pystr.Lower(tokens[0]), ""
	if len(tokens) > 1 {
		rest = pystr.Strip(tokens[1])
	}
	handler, known := goalGateHandlers[verb]
	if !known || (verb == "clear" && rest != "") || (verb != "clear" && rest == "") {
		return GoalCommandResult{Output: "Usage: /goal gate [list | add <command> | remove <N> | clear]", Error: true}, nil
	}
	if verb == "add" {
		if denial := authorizeGate(); denial != "" {
			return GoalCommandResult{Output: denial, Error: true}, nil
		}
	}
	result, err := handler(m, rest)
	if err != nil {
		if pyErr, isPy := err.(*pyint.Error); isPy && goalCommandErrorKinds[pyErr.Kind] {
			operation := verb
			if verb == "rm" {
				operation = "remove"
			}
			return GoalCommandResult{Output: fmt.Sprintf("/goal gate %s: %s", operation, pyErr.Msg), Error: true}, nil
		}
		return GoalCommandResult{}, err
	}
	return result, nil
}

// Sets a goal from inline contract lines or a drafted contract and returns its kickoff prompt
func goalSet(m Manager, arg string, drafting bool, lastUserMessage any, render GoalRender, progress func(string)) (GoalCommandResult, error) {
	var contract *GoalContract
	headline := arg
	if drafting {
		if arg == "" {
			return GoalCommandResult{Output: "Usage: /goal draft <objective in plain language>", Error: true}, nil
		}
		if progress != nil {
			progress(goalDraftingInProgress)
		}
		contract = func() (drafted *GoalContract) {
			defer func() {
				if r := recover(); r != nil {
					goalCommandLogger().Debug(fmt.Sprintf("goal draft failed: %v", r))
					drafted = nil
				}
			}()
			return goalCommandDraft(m.Context(), arg, nil)
		}()
	} else {
		var parsed GoalContract
		headline, parsed = ParseContract(arg)
		if !parsed.IsEmpty() {
			contract = &parsed
		}
	}
	var previous any = ""
	if m.HasGoal() {
		previous = m.State().Goal
	}
	if headline == "" {
		headline = arg
	}
	state, err := m.Set(headline, 0, contract)
	if err != nil {
		return GoalCommandResult{}, err
	}
	if state == nil {
		return GoalCommandResult{}, &pyint.Error{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'goal'"}
	}
	goal := pyrepr.Str(state.Goal)
	output := render("gateway.goal.set", "⊙ Goal set ({budget}-turn budget): {goal}", map[string]any{"budget": state.MaxTurns, "goal": state.Goal})
	if pyrepr.Truthy(previous) && !pyrepr.Equal(previous, state.Goal) {
		output += "\n(replaced the previous goal)\n  was: " + pyrepr.Str(previous)
	}
	if state.HasContract() {
		label := "Completion contract:"
		if drafting {
			label = "Drafted completion contract:"
		}
		output += "\n" + label + "\n" + state.Contract.RenderBlock()
	}
	if drafting {
		if state.HasContract() {
			output += "\nTighten any field by re-setting the goal with inline lines (e.g. verify: <command>), then /goal resume. Use /goal show to review."
		} else {
			output += "\nCouldn't draft a contract (aux model unavailable) — running as a free-form goal. The per-turn judge still applies."
		}
	} else {
		against := ""
		if state.HasContract() {
			against = " against the contract above"
		}
		output += "\nAfter each turn, a judge model checks if the goal is done" + against + ". Icebath keeps working until it is, you pause/clear it, or the budget is " +
			"exhausted. Use /goal status, /goal show, /goal pause, /goal resume, /goal clear."
	}
	prompt := GoalKickPrompt(goal, lastUserMessage)
	return GoalCommandResult{Output: output, Prompt: &prompt, Kickoff: true}, nil
}

// True when the command controls an existing goal rather than replacing it
func IsGoalControl(arg string) bool {
	normalized := pystr.Lower(pystr.Strip(arg))
	first := ""
	if normalized != "" {
		first = splitOnce(normalized)[0]
	}
	_, exact := goalExactHandlers[normalized]
	return exact || goalControlFirstWords[first]
}

// Optional inputs of one goal command, a nil render meaning English
type GoalCommandOptions struct {
	AuthorizeGate   func() string
	LastUserMessage any
	Render          GoalRender
	Progress        func(string)
}

// Applies one goal command to the manager, the goal errors rendered as an invalid goal result
func DispatchGoalCommand(m Manager, arg string, o GoalCommandOptions) (GoalCommandResult, error) {
	render := o.Render
	if render == nil {
		render = EnglishGoalRender
	}
	arg = pystr.Strip(arg)
	tokens := splitOnce(arg)
	verb, rest := "", ""
	if len(tokens) > 0 {
		verb = pystr.Lower(tokens[0])
	}
	if len(tokens) > 1 {
		rest = pystr.Strip(tokens[1])
	}
	prefix := "Invalid goal"
	result, err := func() (GoalCommandResult, error) {
		if verb == "--" {
			return goalSet(m, rest, false, o.LastUserMessage, render, o.Progress)
		}
		if handler, exact := goalExactHandlers[pystr.Lower(arg)]; exact {
			return handler(m, "", render)
		}
		if goalBareControlVerbs[verb] {
			return goalBareControl(m, verb, rest, render)
		}
		if verb == "wait" {
			prefix = "/goal wait"
			return goalWait(m, rest)
		}
		if verb == "gate" {
			prefix = "/goal gate"
			return goalGate(m, rest, o.AuthorizeGate)
		}
		text := arg
		if verb == "draft" {
			text = rest
		}
		return goalSet(m, text, verb == "draft", o.LastUserMessage, render, o.Progress)
	}()
	if err == nil {
		return result, nil
	}
	pyErr, isPy := err.(*pyint.Error)
	if !isPy || !goalCommandErrorKinds[pyErr.Kind] {
		return GoalCommandResult{}, err
	}
	output := prefix + ": " + pyErr.Msg
	if prefix == "Invalid goal" {
		output = render("gateway.goal.invalid", "Invalid goal: {error}", map[string]any{"error": pyErr.Msg})
	}
	return GoalCommandResult{Output: output, Error: true}, nil
}
