package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
		cp("  " + mgr.StatusLine())
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
	var interval int64
	parsed := false
	prompt := ""
	if len(tokens) >= 2 && pystr.Lower(tokens[0]) == "every" {
		interval, parsed = agent.ParseHeartbeatInterval("every " + tokens[1])
		if len(tokens) > 2 {
			prompt = tokens[2]
		}
	} else if len(tokens) > 0 {
		interval, parsed = agent.ParseHeartbeatInterval(tokens[0])
		if parsed && interval > 0 {
			prompt = pystr.Strip(arg[len(tokens[0]):])
		}
	}
	if !parsed {
		cp("  "+c.commandsT("heartbeat.usage", nil), dimLine(c.commandsT("heartbeat.usage_also", nil)))
		return
	}
	if interval < 0 {
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

// Answers /refine off the gateway, where no agent exists yet, the gateway running it whenever a session is open
func (c *CLI) handleRefineCommand(cmd string) {
	cp(dimLine(c.commandsT("refine.nothing_yet", nil)))
}

// Answers /review off the gateway, where no agent exists yet, the gateway running it whenever a session is open
func (c *CLI) handleReviewCommand(cmd string) {
	cp(dimLine(c.commandsT("review.nothing_yet", nil)))
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
