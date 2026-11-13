package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Text wrapped in the skin accent escape
func accentText(text string) string { return accentANSI.String() + text + resetANSI }

// Prints the lines framed by a blank line above and below
func sayBlock(lines ...string) {
	printLines("")
	printLines(lines...)
	printLines("")
}

// Tokens after the command word, a whitespace split when the quoting is unbalanced
func shlexArgs(cmd string) []string {
	if cmd == "" {
		return nil
	}
	parts, err := commands.Shlex(cmd)
	if err != nil {
		fields := pystr.SplitWhitespace(cmd, -1)
		if len(fields) > 0 {
			return fields[1:]
		}
		return nil
	}
	if len(parts) > 0 {
		return parts[1:]
	}
	return nil
}

// Handles /stop, interrupting a running turn first and stopping background processes and delegations
func (c *CLI) handleStopCommand() {
	if !c.agentRunning {
		c.stopBackgroundWork()
		return
	}
	if a := c.agentRef(); a != nil {
		cp("  " + c.t("cli.tui.interrupting", nil))
		requestHardInterrupt(a)
	}
	go c.stopBackgroundWork()
}

// Kills running background processes then interrupts background delegations, printing through cprint
func (c *CLI) stopBackgroundWork() {
	registry, view := c.processRegistry(), gatewayProcessView()
	if view != nil {
		view.refresh(c.ctx, true)
	}
	var running []map[string]any
	for _, p := range registry.listSessions(c.ctx) {
		if p["status"] == "running" {
			running = append(running, p)
		}
	}
	nAsync := tools.AsyncDelegationActiveCount()
	if view != nil {
		nAsync = view.activeDelegations()
	}
	if len(running) == 0 && nAsync == 0 {
		cp("  " + c.commandsT("stop.none_running", nil))
		return
	}
	if len(running) > 0 {
		cp("  " + c.commandsT("stop.stopping", map[string]any{"count": len(running)}))
		killed, err := registry.killAll(c.ctx, "cli.stop")
		if err != nil {
			panic(err)
		}
		cp("  " + c.commandsT("stop.stopped", map[string]any{"count": killed}))
	}
	if nAsync > 0 {
		var interrupted int
		if view != nil {
			count, err := view.interruptDelegations(c.ctx)
			if err != nil {
				panic(err)
			}
			interrupted = count
		} else {
			interrupted = tools.InterruptAllAsyncDelegations("/stop")
		}
		cp("  " + c.commandsT("stop.interrupted_delegations", map[string]any{"count": interrupted}))
	}
}

// Handles /agents, background processes, background delegations and the agent state
func (c *CLI) handleAgentsCommand() {
	registry, view := c.processRegistry(), gatewayProcessView()
	if view != nil {
		view.refresh(c.ctx, true)
	}
	processes := registry.listSessions(c.ctx)
	var running, finished []map[string]any
	for _, p := range processes {
		if p["status"] == "running" {
			running = append(running, p)
		} else {
			finished = append(finished, p)
		}
	}
	cp("  " + c.commandsT("agents.running_processes", map[string]any{"count": len(running)}))
	for _, p := range running {
		uptime, _ := pyrepr.Int(mapValueOr(p, "uptime_seconds", 0))
		seconds := 0
		if uptime != nil {
			seconds = int(uptime.Int64())
		}
		command := cliStr(mapValueOr(p, "command", ""), "")
		cp(fmt.Sprintf("    %s · %s · %s", cliStr(mapValueOr(p, "session_id", "?"), "?"), tools.FormatUptimeShort(seconds), pystr.Slice(command, 0, 80)))
	}
	if len(finished) > 0 {
		cp("  " + c.commandsT("agents.recently_finished", map[string]any{"count": len(finished)}))
	}
	if view == nil {
		c.printLocalDelegations()
	} else if nAsync := view.activeDelegations(); nAsync != 0 {
		cp("  " + c.commandsT("agents.background_delegations", map[string]any{"count": nAsync}))
	}
	if c.agentRunning {
		cp("  " + c.commandsT("agents.agent_running", nil))
	} else {
		cp("  " + c.commandsT("agents.agent_idle", nil))
	}
}

// Seconds of a delegation timing field formatted with no decimals, false when absent
func delegationSeconds(v any) (string, float64, bool) {
	if v == nil {
		return "", 0, false
	}
	f, ok := toFloat(v)
	if !ok {
		return pyrepr.Str(v), 0, true
	}
	text, err := pyrepr.Format(f, ".0f")
	if err != nil {
		panic(err)
	}
	return text, f, true
}

// Float of a numeric value
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// Prints this process's background delegations with live progress detail
func (c *CLI) printLocalDelegations() {
	delegations := func() (out []map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				out = nil
			}
		}()
		return tools.ListAsyncDelegations()
	}()
	if len(delegations) == 0 {
		return
	}
	runningCount := 0
	for _, d := range delegations {
		if d["status"] == "running" || d["status"] == "stalling" {
			runningCount++
		}
	}
	cp("  " + c.commandsT("agents.background_delegations", map[string]any{"count": runningCount}))
	for _, d := range delegations {
		status := cliStr(mapValueOr(d, "status", "?"), "?")
		goal := cliStr(d["goal"], "")
		line := fmt.Sprintf("    %s · %s · %s", cliStr(mapValueOr(d, "delegation_id", "?"), "?"), status, pystr.Slice(goal, 0, 60))
		switch status {
		case "stalling":
			if text, _, ok := delegationSeconds(d["stalled_after_quiet_seconds"]); ok {
				line += c.commandsT("agents.no_progress", map[string]any{"seconds": text})
			}
		case "running":
			if text, f, ok := delegationSeconds(d["seconds_since_progress"]); ok && f >= 60 {
				line += c.commandsT("agents.quiet", map[string]any{"seconds": text})
			}
		}
		cp(line)
		children, _ := d["children_activity"].([]any)
		for i, raw := range children {
			child, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			doing := c.commandsT("agents.between_turns", nil)
			if tool := child["current_tool"]; cliTruthy(tool) {
				doing = c.commandsT("agents.in_tool", map[string]any{"tool": tool})
			}
			part := "      " + c.commandsT("agents.child_line", map[string]any{"index": i + 1, "api_calls": mapValueOr(child, "api_calls", "?"), "doing": doing})
			if text, _, ok := delegationSeconds(child["seconds_since_activity"]); ok {
				part += c.commandsT("agents.last_activity", map[string]any{"seconds": text})
			}
			cp(part)
		}
	}
}

// Handles /journey through the journey command parser, read only views captured with forced color
func (c *CLI) handleJourneyCommand(cmdOriginal string) {
	parts, err := commands.Shlex(cmdOriginal)
	if err != nil {
		panic(err)
	}
	var args []string
	if len(parts) > 0 {
		args = parts[1:]
	}
	tree := commands.NewCLI()
	tree.DisableHelp("journey")
	var runErr error
	ran := false
	handler := func(cmd *cobra.Command, posArgs []string) error {
		ran = true
		ns := tree.Namespace(cmd, posArgs)
		action := ""
		if words := strings.Fields(cmd.CommandPath()); len(words) > 2 {
			action = words[2]
		}
		if action != "" {
			ns["journey_action"] = action
		} else {
			ns["journey_action"] = nil
		}
		if action == "delete" || action == "edit" {
			_, runErr = memory.JourneyCommand(c.ctx, c.env, ns, c.console.Out)
			return nil
		}
		ns["force_color"] = true
		var buf bytes.Buffer
		_, runErr = memory.JourneyCommand(c.ctx, c.env, ns, &buf)
		cp(strings.TrimRight(buf.String(), "\n"))
		return nil
	}
	for _, path := range []string{"journey", "journey list", "journey delete", "journey edit"} {
		tree.Bind(path, handler)
	}
	var help bytes.Buffer
	tree.Root.SetOut(&help)
	execErr := tree.Execute(append([]string{"journey"}, args...))
	if help.Len() > 0 {
		fmt.Fprint(os.Stdout, strings.ReplaceAll(help.String(), "icebath journey", "/journey"))
	}
	if execErr != nil && !ran {
		var ae *commands.ArgparseError
		if errors.As(execErr, &ae) {
			fmt.Fprintln(os.Stderr, strings.ReplaceAll(ae.Stderr(), "icebath journey", "/journey"))
		}
		return
	}
	if runErr != nil {
		cp("  " + c.commandsT("journey.failed", map[string]any{"error": runErr.Error()}))
	}
}

// Handles /paste, attaching a clipboard image for terminals whose paste never delivers one
func (c *CLI) handlePasteCommand() {
	switch {
	case !hasClipboardImage():
		cp(dimLine(c.commandsT("paste.no_image", nil)))
	case c.tryAttachClipboardImage():
		cp("  " + c.commandsT("paste.attached", map[string]any{"index": len(c.attachedImages)}))
	default:
		cp(dimLine(c.commandsT("paste.extract_failed", nil)))
	}
}

// Handles /copy with an optional response number, OSC 52 over SSH or when native tools fail
func (c *CLI) handleCopyCommand(cmdOriginal string) {
	arg := commandArg(cmdOriginal, false)
	var assistant []map[string]any
	for _, m := range c.conversationHistory {
		if m["role"] == "assistant" {
			assistant = append(assistant, m)
		}
	}
	if len(assistant) == 0 {
		cp("  " + c.commandsT("copy.nothing_yet", nil))
		return
	}
	idx := -1
	if arg != "" {
		n, ok := parsePyInt(arg)
		if !ok {
			cp("  " + c.commandsT("copy.usage", nil))
			return
		}
		idx = n - 1
		if idx < 0 || idx >= len(assistant) {
			cp("  " + c.commandsT("copy.invalid_number", map[string]any{"max": len(assistant)}))
			return
		}
	} else {
		for i := len(assistant) - 1; i >= 0; i-- {
			if assistantCopyText(assistant[i]["content"]) != "" {
				idx = i
				break
			}
		}
		if idx < 0 {
			cp("  " + c.commandsT("copy.nothing_in_responses", nil))
			return
		}
	}
	text := assistantCopyText(assistant[idx]["content"])
	if text == "" {
		cp("  " + c.commandsT("copy.nothing_in_response", nil))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cp("  " + c.commandsT("copy.failed", map[string]any{"error": errorText(r)}))
		}
	}()
	if isRemoteShellSession(os.Getenv) || !writeClipboardText(text) {
		c.writeOSC52Clipboard(text)
		cp("  " + c.commandsT("copy.copied_osc52", map[string]any{"index": idx + 1}))
		return
	}
	cp("  " + c.commandsT("copy.copied", map[string]any{"index": idx + 1}))
}

// Handles /image, attaching a local image file for the next prompt
func (c *CLI) handleImageCommand(cmdOriginal string) {
	rawArgs := ""
	if strings.Contains(cmdOriginal, " ") {
		if parts := pystr.SplitWhitespace(cmdOriginal, 1); len(parts) > 1 {
			rawArgs = pystr.Strip(parts[1])
		}
	}
	if rawArgs == "" {
		cprint("  " + dimANSI + c.commandsT("image.usage", map[string]any{"example": "/path/to/image.png"}) + resetANSI)
		return
	}
	pathToken, remainder := splitPathInput(rawArgs)
	imagePath := resolveAttachmentPath(pathToken)
	if imagePath == "" {
		cp(dimLine(c.commandsT("image.not_found", map[string]any{"path": pathToken})))
		return
	}
	if !imageExtensions[pystr.Lower(filepath.Ext(imagePath))] {
		cp(dimLine(c.commandsT("image.unsupported", map[string]any{"name": filepath.Base(imagePath)})))
		return
	}
	c.attachedImages = append(c.attachedImages, imagePath)
	cp("  " + c.commandsT("image.attached", map[string]any{"name": filepath.Base(imagePath)}))
	if remainder != "" {
		cprint("  " + dimANSI + c.commandsT("image.now_type_prompt", map[string]any{"text": remainder}) + resetANSI)
	}
}

// Handles /tools, the tool list bare, per toolset status for list, and disable or enable with a session reset
func (c *CLI) handleToolsCommand(cmd string) {
	parts := shlexArgs(cmd)
	subcommand := ""
	if len(parts) > 0 {
		subcommand = parts[0]
	}
	if subcommand != "list" && subcommand != "disable" && subcommand != "enable" {
		c.showTools()
		return
	}
	if subcommand == "list" {
		c.runToolsConfig("list", nil)
		return
	}
	names := parts[1:]
	if len(names) == 0 {
		printLines(c.commandsT("tools.usage", map[string]any{"subcommand": subcommand}),
			"  "+c.commandsT("tools.example_toolset", map[string]any{"subcommand": subcommand}),
			"  "+c.commandsT("tools.example_mcp", map[string]any{"subcommand": subcommand}))
		return
	}
	key := "tools.enabling"
	if subcommand == "disable" {
		key = "tools.disabling"
	}
	cp(accentText(c.commandsT(key, map[string]any{"names": strings.Join(names, ", ")})))
	c.runToolsConfig(subcommand, names)
	h := home.Dir(c.ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		panic(err)
	}
	enabled := tools.Default.PlatformTools(c.ctx, tools.ScopeKey(h), cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	c.enabledToolsets = sortedKeys(enabled)
	c.newSession(false, "")
	cp(dimText(c.commandsT("tools.session_reset", nil)))
}

// Runs the tools list, disable or enable action, captured with color and reprinted inside the TUI
func (c *CLI) runToolsConfig(action string, names []string) {
	if c.app == nil {
		if err := tools.ToolsCommandHooks.ToolsAction(c.ctx, c.console, action, "cli", names); err != nil {
			panic(err)
		}
		return
	}
	var buf bytes.Buffer
	con := &cliui.Console{In: c.console.In, Out: &buf, Getenv: c.console.Getenv, StdinTTY: c.console.StdinTTY,
		StdoutTTY: func() bool { return true }, TermSize: c.console.TermSize}
	if err := tools.ToolsCommandHooks.ToolsAction(c.ctx, con, action, "cli", names); err != nil {
		panic(err)
	}
	cp(pystr.Splitlines(buf.String(), false)...)
}

// Handles /autoprune through the shared executor
func (c *CLI) handleAutopruneCommand(cmd string) {
	_, rest, _ := strings.Cut(cmd, " ")
	reply, err := commands.ExecuteCommand(c.ctx, "autoprune", commands.CommandContext{Surface: "cli", Args: rest})
	if err != nil {
		panic(err)
	}
	sayBlock(reply.Text)
}

// Handles /profile, the active profile name and its home directory
func (c *CLI) handleProfileCommand() {
	reply, err := commands.ExecuteCommand(c.ctx, "profile", commands.CommandContext{Surface: "cli"})
	if err != nil {
		panic(err)
	}
	sayBlock("  "+c.commandsT("profile.profile", map[string]any{"profile": reply.Data["profile"]}),
		"  "+c.commandsT("profile.home", map[string]any{"home": reply.Data["home"]}))
}

// Keys of a set in sorted order
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
