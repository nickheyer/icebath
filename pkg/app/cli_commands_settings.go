package app

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	ansiBold  = "\033[1m"
	ansiGreen = "\033[92m"
	ansiDim   = "\033[2m"
)

var onWords = map[string]bool{"on": true, "enable": true, "true": true, "1": true}

var offWords = map[string]bool{"off": true, "disable": true, "false": true, "0": true}

var busyModes = []string{"queue", "steer", "interrupt"}

var branchCopyKeys = []string{"content", "tool_calls", "tool_call_id", "reasoning", "reasoning_content", "reasoning_details",
	"codex_reasoning_items", "codex_message_items", "timestamp", "data_use"}

// Indented accent line, the standard slash command headline shape
func accentLine(text string) string { return "  " + accentText(text) }

// Saves a busy style choice and reports whether it persisted or stays for the session
func (c *CLI) persistDisplayChoice(key, value, label, note string) {
	if saveConfigValue(c.ctx, key, value) {
		cp(accentLine(c.commandsT("shared.set_saved", map[string]any{"label": label, "value": value})), dimLine(note))
		return
	}
	cp(accentLine(c.commandsT("shared.set_session_only", map[string]any{"label": label, "value": value})))
}

// The argument without scope flags and whether --global was given
func splitScopeFlags(raw string) (string, bool) {
	var kept []string
	global := false
	for _, tok := range strings.Fields(pystr.Lower(pystr.Strip(raw))) {
		if tok == "--global" {
			global = true
		}
		if tok != "--global" && tok != "--session" {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " "), global
}

// Parenthetical tail of a scoped setting change
func (c *CLI) scopeOutcome(explicitGlobal, saved bool) string {
	switch {
	case saved:
		return c.commandsT("shared.scope_saved", nil)
	case explicitGlobal:
		return c.commandsT("shared.scope_save_failed", nil)
	}
	return c.commandsT("shared.scope_session", nil)
}

// Resolves an on, off or status argument, a bare one toggling, false ok for an unknown word
func toggleTarget(arg string, current bool) (status bool, value bool, ok bool) {
	switch {
	case arg == "status" || arg == "?":
		return true, false, true
	case onWords[arg]:
		return false, true, true
	case offWords[arg]:
		return false, false, true
	case arg == "":
		return false, !current, true
	}
	return false, false, false
}

// The title and stay flag of /branch arguments, only a leading --here being a flag
func parseBranchArgs(raw string) (bool, string) {
	text := pystr.Strip(raw)
	head, rest, _ := strings.Cut(text, " ")
	if pystr.Lower(head) == "--here" {
		return true, pystr.Strip(rest)
	}
	return false, text
}

// Handles /branch, forking the session into an independent copy of the full history
func (c *CLI) handleBranchCommand(cmdOriginal string) {
	if c.agentRunning {
		cp("  " + c.commandsT("shared.agent_busy", map[string]any{"command": "/branch"}))
		return
	}
	if len(c.conversationHistory) == 0 {
		cp("  " + c.gatewayT("branch.no_conversation", nil))
		return
	}
	if c.sessionDB == nil {
		cp(c.dbUnavailableLine())
		return
	}
	_, branchName := parseBranchArgs(commandArg(cmdOriginal, false))
	now := time.Now()
	var newSessionID, branchTitle, parentSessionID string
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = asError(r)
			}
		}()
		newSessionID = c.newSessionID(now)
		branchTitle = branchName
		if branchTitle == "" {
			current, _, terr := c.sessionDB.GetSessionTitle(c.ctx, c.sessionID)
			if terr != nil {
				return terr
			}
			if current == "" {
				current = "branch"
			}
			if branchTitle, terr = c.sessionDB.GetNextTitleInLineage(c.ctx, current); terr != nil {
				return terr
			}
		}
		parentSessionID = c.sessionID
		parentPrompt := ""
		if c.agent != nil {
			parentPrompt = c.agent.CachedSystemPrompt()
		}
		if parentPrompt == "" {
			if meta := c.sessionMeta(parentSessionID); meta != nil {
				parentPrompt = cliStr(meta["system_prompt"], "")
			}
		}
		source := os.Getenv("ICEBATH_SESSION_SOURCE")
		if source == "" {
			source = "cli"
		}
		return c.sessionDB.CreateSession(state.NewSession{ID: newSessionID, Source: source, Model: c.model, ParentSessionID: parentSessionID,
			SystemPrompt: parentPrompt, ModelConfig: tools.WithSessionYolo(map[string]any{"max_iterations": c.maxTurns,
				"reasoning_config": c.reasoningConfig, "_branched_from": parentSessionID}, parentSessionID)})
	}()
	if err != nil {
		cp("  " + c.gatewayT("branch.create_failed", map[string]any{"error": err.Error()}))
		return
	}
	c.endCurrentSession("branched")
	func() {
		defer func() { _ = recover() }()
		rows := make([]map[string]any, 0, len(c.conversationHistory))
		for _, msg := range c.conversationHistory {
			row := map[string]any{"role": mapValueOr(msg, "role", "user")}
			toolName := msg["tool_name"]
			if !cliTruthy(toolName) {
				toolName = msg["name"]
			}
			row["tool_name"] = toolName
			if sidecar, ok := msg["api_content"].(string); ok {
				row["api_content"] = sidecar
			} else {
				row["api_content"] = nil
			}
			for _, k := range branchCopyKeys {
				row[k] = msg[k]
			}
			for k, v := range state.MessageIdentity(msg, true) {
				row[k] = v
			}
			rows = append(rows, row)
		}
		chunk := 500
		_, _ = c.sessionDB.AppendMessagesBatch(c.ctx, newSessionID, rows, state.AppendGuards{}, &chunk)
	}()
	func() {
		defer func() { _ = recover() }()
		_, _ = c.sessionDB.SetSessionTitle(c.ctx, newSessionID, branchTitle)
	}()
	tools.TransferSessionYolo(c.sessionID, newSessionID)
	c.sessionID, c.sessionStart, c.pendingTitle = newSessionID, now, ""
	c.resumed = true
	syncProcessSessionID(c.ctx, c.env, newSessionID)
	if c.agent != nil {
		c.agent.SetSessionStart(now)
	}
	c.syncAgentToSession(newSessionID, parentSessionID, "branch")
	msgCount := 0
	for _, m := range c.conversationHistory {
		if m["role"] == "user" {
			msgCount++
		}
	}
	cp("  "+c.commandsTN("branch.branched", msgCount, map[string]any{"title": branchTitle}),
		"  "+c.commandsT("branch.original_session", map[string]any{"session_id": parentSessionID}),
		"  "+c.commandsT("branch.branch_session", map[string]any{"session_id": newSessionID}))
}

// Personality names as Python lists them, built ins first then user entries in config file order
func (c *CLI) personalityNames() []string {
	return commands.PersonalityNames(c.personalities)
}

// Handles /personality, listing the personalities or setting one and retiring the agent
func (c *CLI) handlePersonalityCommand(cmd string) {
	name := commandArg(cmd, false)
	if name == "" {
		current := ""
		func() {
			defer func() { _ = recover() }()
			raw, err := config.ReadRawConfig(c.ctx, home.Dir(c.ctx))
			if err != nil {
				return
			}
			display, _ := raw["display"].(map[string]any)
			current = commands.NormalizePersonalityName(mapValueOr(display, "personality", ""))
		}()
		marker := "  "
		if current == "" {
			marker = " *"
		}
		printLines("", "+"+strings.Repeat("-", 50)+"+", "|"+pystr.Center(c.commandsT("personality.title", nil), 50)+"|",
			"+"+strings.Repeat("-", 50)+"+", "", " "+marker+pystr.Ljust("none", 12)+" - "+c.commandsT("personality.none_option", nil))
		for _, pname := range c.personalityNames() {
			m := "  "
			if pname == current {
				m = " *"
			}
			printLines(" " + m + pystr.Ljust(pname, 12) + " - " + commands.DescribePersonality(c.personalities[pname], 60))
		}
		printLines("", "  "+c.commandsT("personality.usage", nil), "")
		return
	}
	resolved, prompt, err := commands.ResolvePersonality(name, config.Tree(c.config))
	if err != nil {
		printLines(c.commandsT("personality.unknown", map[string]any{"name": pystr.Lower(name)}),
			"  "+c.commandsT("personality.available", map[string]any{"available": strings.Join(c.personalityNames(), ", ")}))
		return
	}
	saved := commands.PersistPersonality(c.ctx, resolved)
	scope, face := c.commandsT("personality.scope_session", nil), "(^_^)"
	if saved {
		scope, face = c.commandsT("personality.scope_saved", nil), "(^_^)b"
	}
	if resolved == "" {
		c.systemPrompt = ""
		func() {
			defer func() { _ = recover() }()
			raw, err := config.ReadRawConfig(c.ctx, home.Dir(c.ctx))
			if err != nil {
				return
			}
			c.systemPrompt = commands.PromptText(config.Tree(raw).Get("agent.system_prompt"))
		}()
		retireAgent(c)
		printLines(c.commandsT("personality.cleared", map[string]any{"face": face, "scope": scope}), "  "+c.commandsT("personality.cleared_note", nil))
		return
	}
	c.systemPrompt = prompt
	retireAgent(c)
	printLines(c.commandsT("personality.set_to", map[string]any{"face": face, "name": resolved, "scope": scope}), `  "`+ellipsize(prompt, 60)+`"`)
}

// Handles /cron, the scheduled task lines of the shared cron handler
func (c *CLI) handleCronCommand(cmd string) {
	lines, err := cron.CronCommandLines(c.ctx, home.Dir(c.ctx), commandArg(cmd, false))
	if err != nil {
		panic(err)
	}
	printLines(lines...)
}

// Handles /blueprint, listing or filling a blueprint, a seed running as the next turn
func (c *CLI) handleBlueprintCommand(cmd string) {
	parts := shlexArgs(cmd)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = pyShlexQuote(p)
	}
	result, err := cron.HandleBlueprintCommand(c.ctx, home.Dir(c.ctx), strings.Join(quoted, " "), nil, "cli")
	if err != nil {
		c.consolePrint(c.commandsT("blueprint.failed", map[string]any{"error": err.Error()}))
		return
	}
	c.consolePrint(result.Text)
	if result.AgentSeed != nil && *result.AgentSeed != "" {
		c.pendingAgentSeed = *result.AgentSeed
	}
}

// Shell quoting of one token as shlex.quote does
func pyShlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Handles /skills, the write approval review words first, then the skills hub
func (c *CLI) handleSkillsCommand(cmd string) {
	fields := strings.Fields(pystr.Strip(cmd))
	var args []string
	if len(fields) > 0 {
		args = fields[1:]
	}
	reviewWords := map[string]bool{"pending": true, "approve": true, "apply": true, "reject": true, "deny": true, "drop": true,
		"diff": true, "approval": true, "mode": true}
	if len(args) > 0 && reviewWords[pystr.Lower(args[0])] {
		out, handled, err := memory.HandlePendingSubcommand(c.ctx, c.env, tools.WriteApprovalSkills, args, memory.PendingOptions{
			ApplySkill: func(payload map[string]any) string { return skills.ApplySkillPending(c.ctx, c.env, payload) },
			SetMode:    func(enabled bool) error { c.saveWriteApproval("skills", enabled); return nil }})
		if err != nil {
			panic(err)
		}
		if handled {
			printLines(out)
			return
		}
	}
	var buf strings.Builder
	console := pyrich.NewConsole(&buf)
	console.Color, console.Terminal, console.Width = true, true, terminalColumns()
	if err := skills.NewHubCLI(skillsScope(c.ctx)).HandleSkillsSlash(c.ctx, cmd, console); err != nil {
		panic(err)
	}
	if text := strings.TrimRight(buf.String(), "\n"); text != "" {
		cp(pystr.Splitlines(text, false)...)
	}
}

// Handles /memory, the pending review and approval gate subcommands
func (c *CLI) handleMemoryCommand(cmd string) {
	fields := strings.Fields(pystr.Strip(cmd))
	var args []string
	if len(fields) > 0 {
		args = fields[1:]
	}
	var store *memory.Store
	if c.agent != nil {
		store = c.agent.MemoryStore()
	}
	if store == nil {
		loaded, err := memory.LoadOnDiskStore(c.ctx, c.env)
		if err != nil {
			panic(err)
		}
		store = loaded
	}
	out, handled, err := memory.HandlePendingSubcommand(c.ctx, c.env, tools.WriteApprovalMemory, args, memory.PendingOptions{MemoryStore: store,
		SetMode: func(enabled bool) error { c.saveWriteApproval("memory", enabled); return nil }})
	if err != nil {
		panic(err)
	}
	if handled {
		printLines(out)
		return
	}
	printLines(c.commandsT("memory.unknown_subcommand", nil))
}

// Persists the write approval toggle of memory or skills
func (c *CLI) saveWriteApproval(subsystem string, enabled bool) {
	saveConfigValue(c.ctx, subsystem+".write_approval", enabled)
}

// Queues msg as the next normal user turn, an acknowledgement painted first on the turn channel
func (c *CLI) queuePromptTurn(msg, command, ack string) {
	if ack != "" {
		cliVisiblePrint("\n" + ack)
	}
	if c.pendingInput != nil {
		c.pendingInput.Put(msg)
		return
	}
	printLines("  " + c.commandsT("shared.needs_active_session", map[string]any{"command": command}))
}

// Handles /learn, distilling a reusable skill from what the user describes
func (c *CLI) handleLearnCommand(cmd string) {
	request := commandArg(cmd, false)
	key := "learn.from_conversation"
	if request != "" {
		key = "learn.from_description"
	}
	c.queuePromptTurn(memory.BuildLearnPrompt(request), "/learn", c.commandsT(key, nil))
}

// Handles /plan, a markdown implementation plan written without execution
func (c *CLI) handlePlanCommand(cmd string) {
	task := commandArg(cmd, false)
	ack := c.commandsT("plan.from_conversation", nil)
	if task != "" {
		ack = c.commandsT("plan.planning", map[string]any{"task": ellipsize(task, 80)})
	}
	c.queuePromptTurn(provider.BuildPlanPrompt(task), "/plan", ack)
}

// Handles /skin, showing the skins or switching and persisting one
func (c *CLI) handleSkinCommand(cmd string) {
	newSkin := commandArg(cmd, true)
	listings := config.ListSkins(c.ctx)
	if newSkin == "" {
		current := config.GetActiveSkinName(c.ctx)
		printLines("\n  "+c.commandsT("skin.current", map[string]any{"name": current}), "  "+c.commandsT("skin.available_header", nil))
		for _, s := range listings {
			marker := "  "
			if pyrepr.Str(s.Name) == current {
				marker = " ●"
			}
			source := ""
			if s.Source == "user" {
				source = " (" + s.Source + ")"
			}
			printLines("   " + marker + " " + pyrepr.Str(s.Name) + source + " — " + pyrepr.Str(s.Description))
		}
		printLines("\n  "+c.commandsT("skin.usage", nil), "  "+c.commandsT("skin.custom_hint", map[string]any{"dir": home.Display(home.Dir(c.ctx))})+"\n")
		return
	}
	available := map[string]bool{}
	for _, s := range listings {
		available[pyrepr.Str(s.Name)] = true
	}
	if !available[newSkin] {
		printLines("  "+c.commandsT("skin.unknown", map[string]any{"name": newSkin}),
			"  "+c.commandsT("skin.available", map[string]any{"names": strings.Join(sortedKeys(available), ", ")}))
		return
	}
	config.SetActiveSkin(c.ctx, newSkin)
	accentANSI.Reset()
	saved := ""
	if saveConfigValue(c.ctx, "display.skin", newSkin) {
		saved = c.commandsT("skin.saved_suffix", nil)
	}
	printLines("  "+c.commandsT("skin.set_to", map[string]any{"name": newSkin, "saved": saved}), "  "+c.commandsT("skin.banner_note", nil))
	if c.applyTUISkinStyle() {
		printLines("  " + c.commandsT("skin.colors_updated", nil))
	}
}

// Opens the editor on a temp markdown file and returns the saved text with comment lines removed
func (c *CLI) composeInEditor(initialText string) (string, error) {
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		editor = "nano"
		if modalGOOS == "windows" {
			editor = "notepad"
		}
	}
	f, err := os.CreateTemp("", "icebath_prompt_*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	content := c.commandsT("prompt_compose.editor_header", nil) + "\n\n" + initialText
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	argv, err := commands.Shlex(editor)
	if err != nil {
		return "", nil
	}
	argv = append(argv, path)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	var kept []string
	for _, line := range pystr.Splitlines(text, false) {
		if !strings.HasPrefix(line, "#!") {
			kept = append(kept, line)
		}
	}
	return pystr.Strip(strings.Join(kept, "\n")), nil
}

// Handles /prompt, composing the next prompt in the editor and queueing it as the next turn
func (c *CLI) handlePromptComposeCommand(cmdOriginal string) {
	parts := pystr.SplitWhitespace(pystr.Strip(cmdOriginal), 1)
	initial := ""
	if len(parts) > 1 {
		initial = parts[1]
	}
	composed, err := c.composeInEditor(initial)
	if err != nil {
		cp(dimLine(c.commandsT("prompt_compose.editor_failed", map[string]any{"error": err.Error()})))
		return
	}
	if composed == "" {
		cp(dimLine(c.commandsT("prompt_compose.empty", nil)))
		return
	}
	c.pendingAgentSeed = composed
}

// Handles /focus, the display only reduced output riding the tool progress suppression
func (c *CLI) handleFocusCommand(cmdOriginal string) {
	current := c.focusViewEnabled
	action, target := resolveFocusArg(commandArg(cmdOriginal, false), current)
	if action == "usage" {
		cp("  " + c.commandsT("focus.usage", nil))
		return
	}
	var restoreSource any = c.toolProgressMode
	if current {
		restoreSource = c.focusSavedToolProgress
	}
	restoreMode := normalizeToolProgressMode(restoreSource, "all")
	if action == "status" {
		head, tail, _ := strings.Cut(formatFocusStatus(current, restoreMode), "\n")
		label, rest, _ := strings.Cut(head, ":")
		stateColor := ansiDim
		if current {
			stateColor = ansiGreen
		}
		line := "  " + ansiBold + label + ":" + resetANSI + stateColor + rest + resetANSI
		if tail != "" {
			line += "\n" + ansiDim + "  " + pystr.Strip(tail) + resetANSI
		}
		cp(line)
		return
	}
	want := target != nil && *target
	if want == current {
		cp("  " + formatFocusToggleMessage(current, restoreMode))
		return
	}
	if want {
		c.focusSavedToolProgress = restoreMode
		c.setToolProgressMode(FocusToolProgressMode)
	} else {
		c.setToolProgressMode(restoreMode)
		c.focusSavedToolProgress = ""
	}
	c.focusViewEnabled = want
	c.focusHiddenLines = 0
	saveConfigValue(c.ctx, FocusConfigKey, want)
	message := formatFocusToggleMessage(want, restoreMode)
	for _, word := range []string{"enabled", "disabled"} {
		if strings.Contains(message, word) {
			colour := ansiDim
			if want {
				colour = ansiGreen
			}
			message = strings.Replace(message, word, colour+word+resetANSI, 1)
			break
		}
	}
	cp("  " + message)
}

// Sets the live tool progress mode on the CLI and the agent
func (c *CLI) setToolProgressMode(mode string) {
	normalized := normalizeToolProgressMode(mode, "all")
	c.toolProgressMode = normalized
	if c.agent != nil {
		func() {
			defer func() { _ = recover() }()
			c.agent.SetToolProgressMode(normalized)
		}()
	}
}

// Handles /approvals, showing or persisting the profile wide approval mode
func (c *CLI) handleApprovalsCommand(cmdOriginal string) {
	parts := pystr.SplitWhitespace(pystr.Strip(cmdOriginal), 1)
	requested := ""
	if len(parts) > 1 {
		requested = parts[1]
	}
	result, err := commands.RunApprovalModeCommand(c.ctx, home.Dir(c.ctx), c.env, requested)
	if err != nil {
		panic(err)
	}
	cp("  " + result.Message)
}

// Shared /footer and /timestamps flow of status query, usage error or save and report
func (c *CLI) toggleSetting(arg string, current bool, usage, statusLine, configKey, label, failed string) {
	status, value, ok := toggleTarget(arg, current)
	stateWord := func(on bool) string {
		if on {
			return c.commandsT("shared.state_on", nil)
		}
		return c.commandsT("shared.state_off", nil)
	}
	switch {
	case status:
		cp("  " + ansiBold + label + ":" + resetANSI + " " + stateWord(current) + statusLine)
	case !ok:
		cp("  " + c.commandsT("shared.usage", map[string]any{"usage": usage}))
	case saveConfigValue(c.ctx, configKey, value):
		colour := ansiDim
		if value {
			colour = ansiGreen
		}
		cp("  " + label + ": " + colour + stateWord(value) + resetANSI)
	default:
		cp("  " + c.commandsT("toggle.save_failed", map[string]any{"setting": failed}))
	}
}

// Handles /footer, toggling or inspecting the runtime footer
func (c *CLI) handleFooterCommand(cmdOriginal string) {
	h := home.Dir(c.ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		panic(err)
	}
	footer, _ := cfg.Get("display.runtime_footer").(map[string]any)
	fields := cliStrList(footer["fields"])
	if len(fields) == 0 {
		fields = []string{"model", "context_pct", "cwd"}
	}
	c.toggleSetting(commandArg(cmdOriginal, true), cliTruthy(mapValueOr(footer, "enabled", false)), c.commandsT("footer.usage", nil),
		"\n  "+c.commandsT("footer.fields", map[string]any{"fields": strings.Join(fields, ", ")}),
		"display.runtime_footer.enabled", c.commandsT("footer.label", nil), "runtime_footer")
}

// Handles /timestamps, toggling or inspecting message timestamps
func (c *CLI) handleTimestampsCommand(cmdOriginal string) {
	arg := commandArg(cmdOriginal, true)
	current := c.showTimestamps
	if status, value, ok := toggleTarget(arg, current); ok && !status {
		c.showTimestamps = value
	}
	c.toggleSetting(arg, current, c.commandsT("timestamps.usage", nil), "", "display.timestamps", c.commandsT("timestamps.label", nil), "timestamps")
}

// Handles /busy, what Enter does while the agent works
func (c *CLI) handleBusyCommand(cmd string) {
	arg := commandArg(cmd, true)
	usage := dimLine(c.commandsT("busy.usage", nil))
	isMode := func(m string) bool {
		for _, b := range busyModes {
			if b == m {
				return true
			}
		}
		return false
	}
	if arg == "" || arg == "status" {
		mode := c.busyInputMode
		if !isMode(mode) {
			mode = "interrupt"
		}
		cp(accentLine(c.commandsT("busy.status", map[string]any{"mode": c.busyInputMode})),
			dimLine(c.commandsT("busy.enter_while_busy", map[string]any{"behavior": c.commandsT("busy.short_"+mode, nil)})), usage)
		return
	}
	if !isMode(arg) {
		cp(dimLine(c.commandsT("shared.unknown_argument", map[string]any{"arg": arg})), usage)
		return
	}
	c.busyInputMode = arg
	c.persistDisplayChoice("display.busy_input_mode", arg, c.commandsT("busy.label", nil), c.commandsT("busy.long_"+arg, nil))
}

// Handles /indicator, the TUI busy indicator style
func (c *CLI) handleIndicatorCommand(cmd string) {
	display, _ := c.config["display"].(map[string]any)
	current := mapValueOr(display, "tui_status_indicator", home.DefaultIndicatorStyle)
	arg := commandArg(cmd, true)
	usage := dimLine(c.commandsT("indicator.usage", map[string]any{"styles": strings.Join(home.IndicatorStyles, "|")}))
	if arg == "" || arg == "status" {
		cp(accentLine(c.commandsT("indicator.status", map[string]any{"style": current})), usage)
		return
	}
	known := false
	for _, s := range home.IndicatorStyles {
		known = known || s == arg
	}
	if !known {
		cp(dimLine(c.commandsT("indicator.unknown_style", map[string]any{"arg": arg})), usage)
		return
	}
	if display == nil {
		display = map[string]any{}
		c.config["display"] = display
	}
	display["tui_status_indicator"] = arg
	c.persistDisplayChoice("display.tui_status_indicator", arg, c.commandsT("indicator.label", nil), c.commandsT("indicator.note", nil))
}

// Handles /update, confirming then leaving the session to relaunch as the update command
func (c *CLI) handleUpdateCommand() bool {
	h := home.Dir(c.ctx)
	if home.ManagedSystem(c.ctx, h) != "" {
		printLines("  ✗ " + config.FormatManagedMessage(c.ctx, h, c.commandsT("update.managed_action", nil)))
		return false
	}
	choices := []slashConfirmChoice{{"once", c.commandsT("update.choice_update", nil), c.commandsT("update.choice_update_desc", nil)},
		{"cancel", c.commandsT("update.choice_cancel", nil), c.commandsT("update.choice_cancel_desc", nil)}}
	raw, ok := c.promptTextInputModal(c.commandsT("update.title", nil), c.commandsT("update.detail", nil), choices, 120)
	if !ok || c.normalizeSlashConfirmChoice(raw, choices) != "once" {
		printLines("  " + c.commandsT("update.cancelled", nil))
		return false
	}
	sayBlock("  " + c.commandsT("update.launching", nil))
	c.pendingRelaunch = []string{"update"}
	return true
}

// Handles /update from the dispatch table, false leaving the REPL once a relaunch is confirmed
func (c *CLI) cmdUpdate(string) bool { return !c.handleUpdateCommand() }

// Handles /skills from the dispatch table under the slow command status
func (c *CLI) cmdSkills(cmdOriginal string) {
	_ = c.busyCommand(c.slowCommandStatus(cmdOriginal), true, func() { c.handleSkillsCommand(cmdOriginal) })
}
