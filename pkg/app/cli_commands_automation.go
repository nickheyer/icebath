package app

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Handles /bundles, the installed skill bundles rendered in session
func (c *CLI) handleBundlesCommand(string) {
	reply, err := commands.ExecuteCommand(c.ctx, "bundles", commands.CommandContext{Surface: "cli"})
	if err != nil {
		cp("\033[1;31m" + c.commandsT("bundles.unavailable", map[string]any{"error": err.Error()}) + resetANSI)
		return
	}
	bundles, _ := reply.Data["bundles"].([]map[string]any)
	if len(bundles) == 0 {
		cp("  "+c.commandsT("bundles.none_installed", nil), dimLine(c.commandsT("bundles.create_hint", nil)),
			dimLine(c.commandsT("bundles.directory", map[string]any{"dir": reply.Data["dir"]})))
		return
	}
	cp("\n  ▣ " + boldANSI + c.commandsT("bundles.title", nil) + resetANSI + " " + c.commandsT("bundles.installed_count", map[string]any{"count": len(bundles)}))
	for _, info := range bundles {
		var skillNames []string
		switch v := info["skills"].(type) {
		case []string:
			skillNames = v
		case []any:
			for _, s := range v {
				skillNames = append(skillNames, pyrepr.Str(s))
			}
		}
		desc := cliStr(info["description"], "")
		if desc == "" {
			desc = c.commandsT("bundles.default_desc", map[string]any{"count": len(skillNames)})
		}
		chatConsolePrint("    [bold " + accentHex() + "]/" + pystr.Ljust(pyrepr.Str(info["slug"]), 20) + "[/] [dim]-[/] " + pyrich.Escape(desc) +
			" [dim]" + pyrich.Escape(c.commandsT("bundles.skills_count", map[string]any{"count": len(skillNames)})) + "[/]")
		for _, s := range skillNames {
			chatConsolePrint("        [dim]· " + pyrich.Escape(s) + "[/]")
		}
	}
	cp("\n" + dimLine(c.commandsT("bundles.invoke_hint", nil)))
}

// Prints the dim no active session line for a missing session manager
func (c *CLI) sessionManagerUnavailable(label string) {
	cp(dimLine(c.commandsT("shared.unavailable_no_session", map[string]any{"label": label})))
}

// Handles /heartbeat set, status, pause, resume and clear for the session's recurring instruction
func (c *CLI) handleHeartbeatCommand(cmd string) {
	arg := commandArg(cmd, false)
	lower := pystr.Lower(arg)
	mgr := c.getHeartbeatManager()
	if mgr == nil {
		c.sessionManagerUnavailable(c.commandsT("heartbeat.label", nil))
		return
	}
	switch {
	case arg == "" || lower == "status":
		line, err := mgr.StatusLine()
		if err != nil {
			panic(err)
		}
		cp("  " + line)
	case lower == "pause":
		if st := mgr.Pause(); st != nil {
			cp("  " + c.commandsT("heartbeat.paused", map[string]any{"prompt": st.Prompt}))
		} else {
			cp(dimLine(c.commandsT("heartbeat.none_set", nil)))
		}
	case lower == "resume":
		st := mgr.Resume()
		if st == nil {
			cp(dimLine(c.commandsT("heartbeat.none_to_resume", nil)))
			return
		}
		c.startHeartbeatWatchdog()
		cp("  " + c.commandsT("heartbeat.resumed", map[string]any{"interval": agent.FormatHeartbeatInterval(st.IntervalSeconds), "prompt": st.Prompt}))
	case lower == "clear" || lower == "stop" || lower == "off":
		if mgr.Clear() {
			cp("  " + c.commandsT("heartbeat.cleared", nil))
		} else {
			cp(dimLine(c.commandsT("heartbeat.none_set", nil)))
		}
	default:
		c.heartbeatSet(mgr, arg)
	}
}

// Sets the heartbeat from every 10m prompt or 10m prompt
func (c *CLI) heartbeatSet(mgr agent.HeartbeatAutomation, arg string) {
	tokens := pystr.SplitWhitespace(arg, 2)
	var interval *big.Int
	var err error
	prompt := ""
	if len(tokens) >= 2 && pystr.Lower(tokens[0]) == "every" {
		if interval, err = agent.ParseHeartbeatInterval("every " + tokens[1]); err != nil {
			panic(err)
		}
		if len(tokens) > 2 {
			prompt = tokens[2]
		}
	} else if len(tokens) > 0 {
		if interval, err = agent.ParseHeartbeatInterval(tokens[0]); err != nil {
			panic(err)
		}
		if interval != nil && interval.Sign() > 0 {
			prompt = pystr.Strip(arg[len(tokens[0]):])
		}
	}
	if interval == nil {
		cp("  "+c.commandsT("heartbeat.usage", nil), dimLine(c.commandsT("heartbeat.usage_also", nil)))
		return
	}
	if interval.Sign() < 0 {
		cp("  " + c.commandsT("heartbeat.interval_too_small", map[string]any{"min_seconds": agent.MinHeartbeatIntervalSeconds}))
		return
	}
	if pystr.Strip(prompt) == "" {
		cp("  " + c.commandsT("heartbeat.prompt_required", nil))
		return
	}
	st, err := mgr.Set(prompt, interval)
	if err != nil {
		if !agent.IsHeartbeatValueError(err) {
			panic(err)
		}
		cp("  " + c.commandsT("heartbeat.invalid_label", nil) + ": " + err.Error())
		return
	}
	c.startHeartbeatWatchdog()
	cp("  "+c.commandsT("heartbeat.set", map[string]any{"interval": agent.FormatHeartbeatInterval(st.IntervalSeconds), "prompt": st.Prompt}),
		dimLine(c.commandsT("heartbeat.set_note", nil)))
}

// Agent that forks its memory and skill review on demand, the AIAgent review chokepoint
type backgroundReviewSpawner interface {
	SpawnBackgroundReview(ctx context.Context, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string, explicit bool)
}

// Agent exposing the tool names its turn may call
type validToolNamer interface {
	ValidToolNames() map[string]bool
}

var _ backgroundReviewSpawner = (*agent.AIAgent)(nil)
var _ validToolNamer = (*agent.AIAgent)(nil)

// Runs a command step and returns the text of any panic it raised, the except Exception of the handlers
func raisedText(step func()) (text string, raised bool) {
	defer func() {
		if r := recover(); r != nil {
			text, raised = fmt.Sprint(r), true
			if err, ok := r.(error); ok {
				text = err.Error()
			}
		}
	}()
	step()
	return "", false
}

// Starts the background memory and skill review fork on the agent with the optional focus text
func (c *CLI) handleRefineCommand(cmd string) {
	focus := commandArg(cmd, false)
	a := c.agentRef()
	if a == nil {
		cp(dimLine(c.commandsT("refine.nothing_yet", nil)))
		return
	}
	snapshot := append([]map[string]any(nil), c.conversationHistory...)
	if len(snapshot) == 0 {
		cp(dimLine(c.commandsT("refine.empty", nil)))
		return
	}
	spawner, ok := a.(backgroundReviewSpawner)
	if !ok {
		missing := fmt.Sprintf("'%s' object has no attribute '_spawn_background_review'", pyrepr.TypeName(a))
		cp("  " + c.commandsT("refine.failed", map[string]any{"error": missing}))
		return
	}
	reviewSkills := false
	if named, ok := a.(validToolNamer); ok {
		reviewSkills = named.ValidToolNames()["skill_manage"]
	}
	var focusArg *string
	if focus != "" {
		focusArg = &focus
	}
	if text, raised := raisedText(func() { spawner.SpawnBackgroundReview(c.ctx, snapshot, true, reviewSkills, focusArg, true) }); raised {
		cp("  " + c.commandsT("refine.failed", map[string]any{"error": text}))
		return
	}
	tail := ""
	if focus != "" {
		tail = c.commandsT("refine.focus_suffix", map[string]any{"focus": focus})
	}
	cp("  " + c.commandsT("refine.started", map[string]any{"focus": tail}))
}

// Dispatches an independent reviewer subagent over the recent conversation with the argument as its instructions
func (c *CLI) handleReviewCommand(cmd string) {
	prompt := commandArg(cmd, false)
	a := c.agentRef()
	if a == nil {
		cp(dimLine(c.commandsT("review.nothing_yet", nil)))
		return
	}
	snapshot := append([]map[string]any(nil), c.conversationHistory...)
	var result *pyjson.Object
	var rejected error
	if text, raised := raisedText(func() { result, rejected = tools.StartReview(c.ctx, c.env, a, snapshot, prompt) }); raised {
		cp("  " + c.commandsT("review.failed", map[string]any{"error": text}))
		return
	}
	if rejected != nil {
		cp(dimLine(rejected.Error()))
		return
	}
	cp("  " + tools.FormatDispatchNote(result, prompt))
}

// Handles /goal through the shared goal dispatcher, a returned prompt queued as the next turn
func (c *CLI) handleGoalCommand(cmd string) {
	mgr := c.getGoalManager()
	if mgr == nil {
		c.sessionManagerUnavailable(c.commandsT("goal.label", nil))
		return
	}
	history := make([]any, len(c.conversationHistory))
	for i, m := range c.conversationHistory {
		history[i] = m
	}
	result, err := dispatchGoalCommand(mgr, commandArg(cmd, false), goals.GoalCommandOptions{
		AuthorizeGate:   func() string { return "" },
		Progress:        func(text string) { cp(dimLine(text)) },
		LastUserMessage: goals.LastUserMessageContent(history)})
	if err != nil {
		panic(err)
	}
	for _, line := range pystr.Splitlines(result.Output, false) {
		cp("  " + line)
	}
	if result.Prompt != nil && *result.Prompt != "" {
		queued := c.kickGoal(*result.Prompt)
		if !result.Kickoff {
			if queued {
				cp(dimLine(c.commandsT("goal.continuing", nil)))
			} else {
				cp(dimLine(c.commandsT("goal.send_to_kick", nil)))
			}
		}
	}
}

// Queues the next goal turn without touching the cached history
func (c *CLI) kickGoal(prompt string) (queued bool) {
	defer func() {
		if recover() != nil {
			queued = false
		}
	}()
	c.pendingInput.Put(prompt)
	return true
}

// Handles /loop, recurring in session wakeups and their controls
func (c *CLI) handleLoopCommand(cmd string) {
	arg := commandArg(cmd, false)
	mgr := c.getLoopManager()
	if mgr == nil {
		c.sessionManagerUnavailable(c.commandsT("loop.label", nil))
		return
	}
	result, err := agent.DispatchLoopCommand(c.ctx, mgr, arg, nil)
	if err != nil {
		panic(err)
	}
	for _, line := range pystr.Splitlines(result.Output, false) {
		cp("  " + line)
	}
	if result.Created {
		func() {
			defer func() { _ = recover() }()
			if c.goalBlocksLoopTick(mgr) {
				cp(dimLine(c.commandsT("loop.goal_defers_note", nil)))
			}
		}()
	}
}

// Handles /subgoal, listing, appending, removing or clearing the goal's subgoals
func (c *CLI) handleSubgoalCommand(cmd string) {
	parts := pystr.SplitWhitespace(pystr.Strip(cmd), 2)
	arg := ""
	if len(parts) > 1 {
		arg = pystr.Strip(strings.Join(parts[1:], " "))
	}
	mgr := c.getGoalManager()
	if mgr == nil {
		c.sessionManagerUnavailable(c.commandsT("goal.label", nil))
		return
	}
	if !mgr.HasGoal() {
		cp(dimLine(c.commandsT("subgoal.no_goal", nil)))
		return
	}
	if arg == "" {
		cp("  "+mgr.StatusLine(), "  "+mgr.RenderSubgoals())
		return
	}
	tokens := pystr.SplitWhitespace(arg, 1)
	verb, rest := pystr.Lower(tokens[0]), ""
	if len(tokens) > 1 {
		rest = pystr.Strip(tokens[1])
	}
	switch verb {
	case "remove":
		if rest == "" {
			cp("  " + c.commandsT("subgoal.usage_remove", nil))
			return
		}
		idx, ok := parsePyInt(strings.Fields(rest)[0])
		if !ok {
			cp("  " + c.commandsT("subgoal.remove_needs_int", nil))
			return
		}
		removed, err := mgr.RemoveSubgoal(int64(idx))
		if err != nil {
			cp("  /subgoal remove: " + err.Error())
			return
		}
		cp("  " + c.commandsT("subgoal.removed", map[string]any{"index": idx, "text": removed}))
	case "clear":
		prev, err := mgr.ClearSubgoals()
		if err != nil {
			cp("  /subgoal clear: " + err.Error())
			return
		}
		if prev > 0 {
			cp("  " + c.commandsTN("subgoal.cleared", prev, nil))
		} else {
			cp(dimLine(c.commandsT("subgoal.none_to_clear", nil)))
		}
	default:
		text, err := mgr.AddSubgoal(arg)
		if err != nil {
			cp("  /subgoal: " + err.Error())
			return
		}
		idx := 0
		if st := mgr.State(); st != nil {
			idx = len(st.Subgoals)
		}
		cp("  " + c.commandsT("subgoal.added", map[string]any{"index": idx, "text": text}))
	}
}
