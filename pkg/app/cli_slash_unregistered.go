package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Bundle table and the three dispatch steps of an unregistered slash, replaced by the cases that patch them in hermes
var (
	slashSkillBundles   func(ctx context.Context) skills.SkillCommands
	slashRunSkillBundle func(c *CLI, baseCmd string, info map[string]any, userInstruction string)
	slashRunSkill       func(c *CLI, baseCmd string, info map[string]any, rest string)
	slashExpandPrefix   func(c *CLI, cmdOriginal, cmdLower string, skillCommands, skillBundles skills.SkillCommands) bool
)

func init() {
	slashSkillBundles, slashRunSkillBundle = getSkillBundles, (*CLI).runSkillBundleCommand
	slashRunSkill, slashExpandPrefix = (*CLI).runSkillSlashCommand, (*CLI).expandSlashPrefix
}

// Slash input with no built in handler, quick commands then plugins then bundles then skills then prefix expansion
func (c *CLI) processUnregisteredSlash(cmdOriginal, cmdLower string) bool {
	baseCmd := pystr.SplitWhitespace(cmdLower, -1)[0]
	bare := pystr.LstripChars(baseCmd, "/")
	skillBundles := slashSkillBundles(c.ctx)
	quickCommands := config.AsMap(mapValueOr(c.config, "quick_commands", map[string]any{}))
	userArgs := ""
	if n := len([]rune(baseCmd)); n < len([]rune(cmdOriginal)) {
		userArgs = pystr.Strip(string([]rune(cmdOriginal)[n:]))
	}
	if entry, ok := quickCommands[bare]; ok {
		return c.runQuickCommand(baseCmd, entry, userArgs)
	}
	plugin, err := gatewayPluginLookup(c, bare)
	if err != nil {
		var unavailable *CatalogUnavailable
		if errors.As(err, &unavailable) {
			return true
		}
		panic(err)
	}
	gone := false
	skillCommands := sessionSkillCommands(c)
	if (plugin != nil && *plugin) || (plugin == nil && getPluginCmdHandlerNames(c.ctx)[bare]) {
		gone = c.runPluginSlashCommand(baseCmd, userArgs) == pluginGone
		if !gone {
			return true
		}
		skillCommands = sessionSkillCommands(c)
	}
	if info, ok := skillBundles[baseCmd]; ok {
		slashRunSkillBundle(c, baseCmd, info, userArgs)
	} else if info, ok := skillCommands[baseCmd]; ok {
		slashRunSkill(c, baseCmd, info, userArgs)
	} else if !(gone && reportGatewayCommandGone(bare, skillsAsAny(skillCommands), skillsAsAny(skillBundles))) {
		return slashExpandPrefix(c, cmdOriginal, cmdLower, skillCommands, skillBundles)
	}
	return true
}

// Skill command table as a name keyed map for the gone report's prefix check
func skillsAsAny(table skills.SkillCommands) map[string]any {
	out := make(map[string]any, len(table))
	for key, info := range table {
		out[key] = info
	}
	return out
}

// Runs a config quick command, exec through a sanitized shell and alias by dispatching its target
func (c *CLI) runQuickCommand(baseCmd string, raw any, userArgs string) bool {
	qcmd := config.AsMap(raw)
	if qcmd == nil {
		panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'get'", config.PyTypeName(raw)))
	}
	qtype := qcmd["type"]
	if qtype == "alias" {
		target := pystr.Strip(cliStr(mapValueOr(qcmd, "target", ""), ""))
		if target != "" {
			if !strings.HasPrefix(target, "/") {
				target = "/" + target
			}
			return c.redispatchCommand(pystr.Strip(target + " " + userArgs))
		}
		c.consolePrint("[bold red]" + c.t("cli.quick.no_target", map[string]any{"command": baseCmd}) + "[/]")
		return true
	}
	if qtype != "exec" {
		c.consolePrint("[bold red]" + c.t("cli.quick.unsupported_type", map[string]any{"command": baseCmd}) + "[/]")
		return true
	}
	execCmd := mapValueOr(qcmd, "command", "")
	if !cliTruthy(execCmd) {
		c.consolePrint("[bold red]" + c.t("cli.quick.no_command", map[string]any{"command": baseCmd}) + "[/]")
		return true
	}
	reply, err := commands.RunCLIExecQuickCommand(c.ctx, c.env, execCmd)
	if err != nil {
		panic(err)
	}
	switch reply.Style {
	case commands.CLIQuickOutput:
		c.consolePrintRenderable(&pyrich.Text{Plain: reply.Text})
	case commands.CLIQuickDim:
		c.consolePrint("[dim]" + reply.Text + "[/]")
	default:
		c.consolePrint("[bold red]" + reply.Text + "[/]")
	}
	return true
}

// Runs an in process plugin slash command with the clarify panel answering its choices
func (c *CLI) runPluginSlashCommand(baseCmd, userArgs string) pluginRunOutcome {
	if answered := runGatewayPluginCommand(c, pystr.LstripChars(baseCmd, "/"), userArgs); answered != pluginNotRun {
		return answered
	}
	handler, ok := plugins.GetPluginCommandHandler(c.ctx, pystr.LstripChars(baseCmd, "/"))
	if !ok || handler == nil {
		return pluginNotRun
	}
	defer func() {
		if r := recover(); r != nil {
			cprint("\033[1;31m" + c.t("cli.plugin.command_error", map[string]any{"error": errorText(r)}) + resetANSI)
		}
	}()
	ctx := plugins.BindAsker(c.ctx, plugins.ClarifyAsker(c.clarifyCallback))
	raw, err := plugins.InvokePluginCommand(ctx, handler, userArgs, map[string]any{"source": nil, "session_id": c.sessionID,
		"session_key": c.sessionID, "platform": "cli"})
	if err != nil {
		panic(err)
	}
	result, err := plugins.ResolvePluginCommandResult(ctx, raw)
	if err != nil {
		panic(err)
	}
	if pyrepr.Truthy(result) {
		cprint(pyrepr.Str(result))
	}
	return pluginNotRun
}

// Queues a loaded skill message as the next turn
func (c *CLI) queueSkillMessage(msg string) {
	if c.pendingInput != nil {
		c.pendingInput.Put(msg)
	}
}

// Loads every skill of a bundle at once, bundles winning over same named skills
func (c *CLI) runSkillBundleCommand(baseCmd string, info map[string]any, userInstruction string) {
	msg, loaded, missing, ok, err := skillsScope(c.ctx).BuildBundleInvocationMessage(c.ctx, baseCmd, userInstruction, c.sessionID, "")
	if err != nil {
		panic(err)
	}
	if !ok {
		chatConsolePrint("[bold red]" + c.t("cli.skills.bundle_load_failed", map[string]any{"command": baseCmd}) + "[/]")
		return
	}
	c.queueLoadedSkills(msg, c.t("cli.skills.loading_bundle", map[string]any{"name": info["name"], "count": fmt.Sprint(len(loaded))}), missing)
}

// Prints the loading label and any missing names, then queues the message
func (c *CLI) queueLoadedSkills(msg, label string, missing []string) {
	printLines("\n⚡ " + label)
	if len(missing) > 0 {
		chatConsolePrint("[yellow]" + c.t("cli.skills.skipped_missing", map[string]any{"names": strings.Join(missing, ", ")}) + "[/]")
	}
	c.queueSkillMessage(msg)
}

// Loads one skill or a stack of up to five leading skills with the instruction after them
func (c *CLI) runSkillSlashCommand(baseCmd string, info map[string]any, rest string) {
	if runGatewaySkill(c, baseCmd, rest) {
		return
	}
	scope := skillsScope(c.ctx)
	extra, instruction, err := scope.SplitStackedSkillCommands(c.ctx, rest, true)
	if err != nil {
		panic(err)
	}
	if len(extra) > 0 {
		msg, loaded, missing, ok, err := scope.BuildStackedSkillInvocationMessage(c.ctx, append([]string{baseCmd}, extra...), instruction, c.sessionID)
		if err != nil {
			panic(err)
		}
		if !ok {
			chatConsolePrint("[bold red]" + c.t("cli.skills.stacked_load_failed", map[string]any{"command": baseCmd}) + "[/]")
			return
		}
		c.queueLoadedSkills(msg, c.t("cli.skills.loading_stacked", map[string]any{"count": fmt.Sprint(len(loaded)), "names": strings.Join(loaded, ", ")}), missing)
		return
	}
	msg, ok, err := scope.BuildSkillInvocationMessage(c.ctx, baseCmd, rest, c.sessionID, "")
	if err != nil {
		panic(err)
	}
	if ok && msg != "" {
		c.queueLoadedSkills(msg, c.t("cli.skills.loading_skill", map[string]any{"name": info["name"]}), nil)
		return
	}
	chatConsolePrint("[bold red]" + c.t("cli.skills.skill_load_failed", map[string]any{"command": baseCmd}) + "[/]")
}

// Unique prefix expansion against the built in commands, skills and bundles, agreeing with tab completion
func (c *CLI) expandSlashPrefix(cmdOriginal, cmdLower string, skillCommands, skillBundles skills.SkillCommands) bool {
	typed := pystr.SplitWhitespace(cmdLower, -1)[0]
	known := map[string]bool{}
	for k := range commands.Commands(commands.Default) {
		known[k] = true
	}
	for k := range skillCommands {
		known[k] = true
	}
	for k := range skillBundles {
		known[k] = true
	}
	var matches []string
	for k := range known {
		if strings.HasPrefix(k, typed) {
			matches = append(matches, k)
		}
	}
	if len(matches) > 1 {
		if known[typed] {
			matches = []string{typed}
		} else {
			minLen := -1
			for _, m := range matches {
				if n := pystr.Len(m); minLen < 0 || n < minLen {
					minLen = n
				}
			}
			var shortest []string
			for _, m := range matches {
				if pystr.Len(m) == minLen {
					shortest = append(shortest, m)
				}
			}
			if len(shortest) == 1 {
				matches = shortest
			}
		}
	}
	if len(matches) == 1 && matches[0] != typed {
		stripped := []rune(pystr.Strip(cmdOriginal))
		tail := ""
		if n := len([]rune(typed)); n < len(stripped) {
			tail = string(stripped[n:])
		}
		return c.redispatchCommand(matches[0] + tail)
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		cprint(accentANSI.String() + c.t("cli.command.ambiguous", map[string]any{"command": cmdLower}) + resetANSI)
		cprint(dimANSI + c.t("cli.command.did_you_mean_many", map[string]any{"candidates": strings.Join(matches, ", ")}) + resetANSI)
		return true
	}
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	lead, pointer := commands.UnknownCommandLines(cmdLower, names)
	cprint("\033[1;31m" + lead + resetANSI)
	cprint(dimANSI + accentANSI.String() + pointer + resetANSI)
	return true
}

// Prints the current configuration framed in ASCII sections
func (c *CLI) showConfig() {
	terminalEnv := envOr("TERMINAL_ENV", "local")
	terminalCwdValue := terminalCwd()
	terminalTimeout := envOr("TERMINAL_TIMEOUT", "60")
	h := home.Dir(c.ctx)
	configPath := filepath.Join(h, "config.yaml")
	if exists, _ := config.ConfigExists(c.ctx, configPath); !exists {
		configPath = filepath.Join(executableDir(), "cli-config.yaml")
	}
	configStatus := c.t("cli.config.not_found", nil)
	if exists, _ := config.ConfigExists(c.ctx, configPath); exists {
		configStatus = c.t("cli.config.loaded", nil)
	}
	if backend, err := config.GetConfigBackend(c.ctx); err == nil && !backend.SupportsFileTooling() {
		configPath, configStatus = backend.Describe(c.ctx, h), ""
	}
	displayKey := c.apiKey
	apiKeyDisplay := c.t("cli.config.api_key_not_set", nil)
	if label := provider.CredentialLabel(displayKey); label != "" {
		apiKeyDisplay = label
	} else if pystr.Len(displayKey) > 12 {
		apiKeyDisplay = pystr.Slice(displayKey, 0, 8) + "..." + pystr.Slice(displayKey, pystr.Len(displayKey)-4, pystr.Len(displayKey))
	}
	title := c.t("cli.config.title", nil)
	width := 50
	pad := width - pystr.Len(title)
	unset := c.t("cli.config.value_not_set", nil)
	toolsets := "all"
	if len(c.enabledToolsets) > 0 {
		toolsets = strings.Join(c.enabledToolsets, ", ")
	}
	type row struct {
		key   string
		value string
	}
	terminalRows := []row{{"cli.config.label_environment", terminalEnv}}
	if terminalEnv == "ssh" {
		terminalRows = append(terminalRows, row{"cli.config.label_ssh_target",
			envOr("TERMINAL_SSH_USER", unset) + "@" + envOr("TERMINAL_SSH_HOST", unset) + ":" + envOr("TERMINAL_SSH_PORT", "22")})
	}
	terminalRows = append(terminalRows, row{"cli.config.label_working_dir", terminalCwdValue}, row{"cli.config.label_timeout", terminalTimeout + "s"})
	verbose := "False"
	if c.verbose {
		verbose = "True"
	}
	sections := []struct {
		key  string
		rows []row
	}{
		{"cli.config.section_model", []row{{"cli.config.label_model", c.model}, {"cli.config.label_base_url", c.baseURL}, {"cli.config.label_api_key", apiKeyDisplay}}},
		{"cli.config.section_terminal", terminalRows},
		{"cli.config.section_agent", []row{{"cli.config.label_max_turns", fmt.Sprint(c.maxTurns)}, {"cli.config.label_toolsets", toolsets}, {"cli.config.label_verbose", verbose}}},
		{"cli.config.section_session", []row{{"cli.config.label_started", pydatetime.Strftime(c.sessionStart, "%Y-%m-%d %H:%M:%S")},
			{"cli.config.label_config_file", configPath + " " + configStatus}}},
	}
	printLines("", "+"+strings.Repeat("-", width)+"+", "|"+strings.Repeat(" ", max(pad/2, 0))+title+strings.Repeat(" ", max(pad-pad/2, 0))+"|", "+"+strings.Repeat("-", width)+"+")
	for _, section := range sections {
		printLines("", "  -- "+c.t(section.key, nil)+" --")
		labelWidth := 0
		for _, r := range section.rows {
			labelWidth = max(labelWidth, pystr.Len(c.t(r.key, nil)))
		}
		for _, r := range section.rows {
			printLines("  " + pystr.Ljust(c.t(r.key, nil), labelWidth) + " " + r.value)
		}
	}
	printLines("")
}

// Dispatches an internal re entry such as a quick command alias or prefix expansion, counted once
func (c *CLI) redispatchCommand(text string) bool {
	c.slashRedispatch = true
	return c.processCommand(text)
}
