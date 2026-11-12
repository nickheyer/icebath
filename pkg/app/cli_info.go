package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds between config.yaml signature reads of the MCP config watcher
const configWatchInterval = 5.0

// Tool progress modes in /verbose cycling order
var toolProgressCycle = []string{"off", "new", "all", "verbose"}

// Raw ANSI style of each tool progress mode label
var toolProgressStyles = map[string]string{"off": cliui.Dim, "new": cliui.Yellow, "all": cliui.Green, "verbose": cliui.Bold + cliui.Green}

// Help category ids mapped to their catalog slugs, other categories printed verbatim
var helpSectionKeys = map[string]string{
	"Session": "session", "Configuration": "configuration", "Tools & Skills": "tools_skills", "Info": "info",
	"Exit": "exit", "Context": "context", "Background & Automation": "background_automation",
}

// Seams the hermes tests replace by patching module attributes, each bound to the real call
var (
	toolDefinitionsFor = func(c *CLI, skipToolSearchAssembly bool) []map[string]any {
		return tools.Default.ToolDefinitions(c.ctx, c.env, tools.DefinitionOptions{Enabled: c.enabledToolsets, Disabled: c.disabledToolsets,
			Quiet: true, SkipToolSearchAssembly: skipToolSearchAssembly})
	}
	toolsetOfTool = func(c *CLI, name string) string {
		return tools.Default.ToolsetForTool(c.ctx, bannerToolEnv(c.ctx).Home, name)
	}
	welcomeBannerPrinter = buildWelcomeBanner
	checkBangApproval    = commands.CheckBangApproval
	runBangCommand       = commands.RunBangCommand
	redeemCodexReset     = provider.RedeemCodexResetCredit
	fetchAccountUsage    = provider.FetchAccountUsage
	reloadSkillsFor      = func(c *CLI) (map[string]any, error) { return skillsScope(c.ctx).ReloadSkills(c.ctx) }
	readAgentUsage       = agentUsageOf
	insightsReport       = memory.CmdInsights
	helpSkillCommands    = sessionSkillCommands
	helpSkillBundles     = func(c *CLI) skills.SkillCommands { return getSkillBundles(c.ctx) }
	skillNotesFor        = (*CLI).skillCollisionNotes
	mcpServerKeys        = mcp.ServerKeys
	mcpShutdown          = shutdownMCPServers
	mcpReprobe           = mcp.ReprobeToolAvailability
	mcpDiscover          = func(c *CLI) ([]string, error) { return mcp.ConnectAll(c.ctx, c.env, nil) }
)

// agentUsageReading is what the banner and /usage read off a live agent
type agentUsageReading struct {
	Model                                                      string
	Calls, Input, Output, Reasoning, Prompt, Completion, Total int
	HasCompressor                                              bool
	LastPromptTokens, ContextLength, Compressions              int
	DisplaySource                                              string
	ConfigContextLength                                        int
	RateLimit                                                  *provider.RateLimitState
}

// Session counters, compressor figures and the last rate limit state of the gateway agent
func agentUsageOf(c *CLI, a *GatewayAgent) agentUsageReading {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := agentUsageReading{Model: a.model, Calls: a.counters["session_api_calls"], Input: a.counters["session_input_tokens"],
		Output: a.counters["session_output_tokens"], Reasoning: a.counters["session_reasoning_tokens"], Prompt: a.counters["session_prompt_tokens"],
		Completion: a.counters["session_completion_tokens"], Total: a.counters["session_total_tokens"], RateLimit: a.rateLimitState,
		HasCompressor: true, LastPromptTokens: a.compressor.lastPromptTokens, ContextLength: a.compressor.contextLength,
		Compressions: a.compressor.compressionCount, DisplaySource: "provider_usage"}
	if a.configContextLength != nil {
		r.ConfigContextLength = *a.configContextLength
	}
	if real := a.compressor.lastRealPromptTokens; real != nil && a.compressor.lastPromptTokens > 0 && a.compressor.lastPromptTokens != *real {
		r.DisplaySource = "local_estimate"
	}
	return r
}

// Python str.format of a constant spec, a malformed spec raising as in Python
func pyFmt(spec string, kw map[string]any) string {
	out, err := i18n.PyFormat(spec, kw)
	if err != nil {
		panic(err)
	}
	return out
}

// Prints one line on the standard output stream
func infoPrint(text string) { fmt.Fprintln(console().Out, text) }

// Prints a renderable through the prompt safe console once the prompt is live, else straight to the console
func (c *CLI) consolePrintRenderable(r pyrich.Renderable) {
	if c.app != nil {
		defaultChatConsole.PrintRenderable(r)
		return
	}
	(&pyrich.Console{Out: c.console.Out, Color: c.console.ShouldUseColor(), Terminal: c.console.StdoutIsTTY(), Width: terminalColumns()}).PrintRenderable(r)
}

// Clears the screen and homes the cursor on a terminal that is not dumb, as rich Console.clear
func (c *CLI) clearConsole() {
	if term := strings.ToLower(os.Getenv("TERM")); !c.console.StdoutIsTTY() || term == "dumb" || term == "unknown" {
		return
	}
	fmt.Fprint(c.console.Out, "\x1b[2J\x1b[H")
}

// Localized tool progress line of a mode, empty for an unknown mode
func toolProgressLabel(ctx context.Context, mode string) string {
	style, ok := toolProgressStyles[mode]
	if !ok {
		return ""
	}
	return style + i18n.T(ctx, "cli.verbose.label_"+mode, "", nil) + cliui.Reset + " — " + i18n.T(ctx, "cli.verbose.detail_"+mode, "", nil)
}

// Once, always and cancel rows of the /reload-mcp confirmation in the active language
func reloadMCPChoices(ctx context.Context) []slashConfirmChoice {
	t := func(key string) string { return i18n.T(ctx, key, "", nil) }
	return []slashConfirmChoice{
		{Value: "once", Label: t("cli.reload_mcp.choice_once"), Desc: t("cli.reload_mcp.choice_once_hint")},
		{Value: "always", Label: t("cli.reload_mcp.choice_always"), Desc: t("cli.reload_mcp.choice_always_hint")},
		{Value: "cancel", Label: t("cli.reload_mcp.choice_cancel"), Desc: t("cli.reload_mcp.choice_cancel_hint")},
	}
}

// Localized /help heading of a known category, any other category verbatim
func helpSectionTitle(ctx context.Context, category string) string {
	if slug, ok := helpSectionKeys[category]; ok {
		return i18n.T(ctx, "cli.help.section_"+slug, "", nil)
	}
	return category
}

// Prints the three line plus and dash box around a centered title
func asciiBox(title string, width int) {
	pad := width - utf8.RuneCountInString(title)
	infoPrint("+" + strings.Repeat("-", width) + "+")
	infoPrint("|" + strings.Repeat(" ", max(0, pad/2)) + title + strings.Repeat(" ", max(0, pad-pad/2)) + "|")
	infoPrint("+" + strings.Repeat("-", width) + "+")
}

// Tool name to toolset id map in tool order, tools of unavailable toolsets added when absent
func toolsetMap(toolDefs []map[string]any, availability map[string]any, toolsetFor func(string) string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, def := range toolDefs {
		fn, _ := def["function"].(map[string]any)
		name, _ := fn["name"].(string)
		var ts any
		if s := toolsetFor(name); s != "" {
			ts = s
		}
		out.Set(name, ts)
	}
	unavailable, _ := bannerListItems(availability["unavailable_toolsets"])
	for _, item := range unavailable {
		names, _, _ := bannerDictGet(item, "tools")
		list, _ := bannerListItems(names)
		id, hasID, _ := bannerDictGet(item, "id")
		if !hasID {
			if name, hasName, _ := bannerDictGet(item, "name"); hasName {
				id = name
			} else {
				id = ""
			}
		}
		for _, n := range list {
			key, _ := n.(string)
			if _, ok := out.Get(key); !ok {
				out.Set(key, id)
			}
		}
	}
	return out
}

// Catalog line of one added or removed skill, the description after a colon when present
func skillLine(item map[string]any) string {
	name, _ := item["name"].(string)
	desc, _ := item["description"].(string)
	if desc != "" {
		return "    - " + name + ": " + desc
	}
	return "    - " + name
}

// BannerPlan is what the welcome banner draws, a nil Banner meaning the compact banner
type BannerPlan struct {
	Store         *gatewaycli.GatewaySessionStore
	Inventory     map[string]any
	ContextLength *int
	Banner        *welcomeBanner
}

// Plans the welcome banner, compact on a narrow terminal, with display.compact or a gateway session with no snapshot yet
func bannerPlan(c *CLI) BannerPlan {
	store := c.gatewayStore()
	var inventory map[string]any
	if store != nil {
		inventory = store.BannerInventory()
	}
	var ctxLen *int
	configCtxLen := 0
	if c.agent != nil {
		reading := readAgentUsage(c, c.agent)
		n := reading.ContextLength
		ctxLen = &n
		configCtxLen = reading.ConfigContextLength
	}
	if c.compact || terminalColumns() < 80 || (store != nil && inventory == nil) {
		return BannerPlan{Store: store, Inventory: inventory, ContextLength: ctxLen}
	}
	cwd, ok := os.LookupEnv("TERMINAL_CWD")
	if !ok {
		cwd, _ = os.Getwd()
	}
	kw := &welcomeBanner{Model: c.model, Cwd: cwd, EnabledToolsets: c.enabledToolsets, SessionID: c.sessionID, Provider: c.provider}
	if ctxLen != nil {
		kw.ContextLength = *ctxLen
		kw.ContextPinned = agent.IsContextPinned(*ctxLen, configCtxLen)
	}
	return BannerPlan{Store: store, Inventory: inventory, ContextLength: ctxLen, Banner: kw}
}

// Main's banner from the gateway session's snapshot, its canonical id shown as the short label
func (c *CLI) showGatewayBanner(console *pyrich.Console, inventory map[string]any, banner welcomeBanner) error {
	toolsetOf, _ := inventory["get_toolset_for_tool"].(func(string) any)
	banner.Tools = inventory["tools"]
	banner.ToolsetForTool = func(name string) string { return strOrEmpty(toolsetOf(name)) }
	banner.Availability = inventory["availability"]
	banner.SkillsByCategory = inventory["skills_by_category"]
	if cwd := strOrEmpty(inventory["cwd"]); cwd != "" {
		banner.Cwd = cwd
	}
	if n, ok := pyIntValue(inventory["context_length"]); ok && n != 0 {
		banner.ContextLength = n
	}
	banner.SessionID = gatewaycli.SessionLabel(c.sessionID)
	return welcomeBannerPrinter(c.ctx, console, banner)
}

// Main's tool availability warning lines as the gateway that resolved this session's tools computed them
func (c *CLI) printGatewayToolWarnings(lines []any) {
	if len(lines) == 0 {
		return
	}
	c.consolePrint("")
	for _, line := range lines {
		c.consolePrint(pyrepr.Str(line))
	}
}

// Rich console the welcome banner prints on, the CLI console at the terminal width
func (c *CLI) bannerConsole() *pyrich.Console {
	return &pyrich.Console{Out: c.console.Out, Color: c.console.ShouldUseColor(), Terminal: c.console.StdoutIsTTY(), Width: terminalColumns()}
}

// Prints the welcome banner, the startup warnings, the low context guidance and the project skills line
func (c *CLI) showBanner() {
	c.clearConsole()
	plan := bannerPlan(c)
	onGateway := plan.Store != nil
	if plan.Banner == nil {
		c.consolePrint(buildCompactBanner())
		c.showStatus()
	} else {
		snapshot := loadBannerSnapshot(c.ctx, c.enabledToolsets)
		if plan.Inventory != nil {
			if err := c.showGatewayBanner(c.bannerConsole(), plan.Inventory, *plan.Banner); err != nil {
				panic(err)
			}
		} else if snapshot != nil {
			c.deferToolWarnings = true
			tsMap, _ := snapshot.Get("toolset_map")
			kw := *plan.Banner
			kw.Tools, _ = snapshot.Get("tools")
			kw.ToolsetForTool = func(name string) string {
				v, _, _ := bannerDictGet(tsMap, name)
				s, _ := v.(string)
				return s
			}
			kw.Availability, _ = snapshot.Get("availability")
			kw.SkillsByCategory, _ = snapshot.Get("skills_by_category")
			if err := welcomeBannerPrinter(c.ctx, c.bannerConsole(), kw); err != nil {
				panic(err)
			}
			go c.refreshBannerSnapshot()
		} else {
			toolDefs := toolDefinitionsFor(c, false)
			availability := computeToolsetAvailability(c.ctx, c.enabledToolsets)
			kw := *plan.Banner
			kw.Tools, kw.Availability = toolDefs, availability
			if err := welcomeBannerPrinter(c.ctx, c.bannerConsole(), kw); err != nil {
				panic(err)
			}
			c.saveBannerSnapshotQuietly(toolDefs, availability, "banner snapshot save failed")
		}
	}
	if warnings, isList := plan.Inventory["tool_availability_warnings"].([]any); isList {
		c.printGatewayToolWarnings(warnings)
	} else if !onGateway && os.Getenv("ICEBATH_DEFER_AGENT_STARTUP") != "1" {
		if c.deferToolWarnings {
			go c.showToolAvailabilityWarnings()
		} else {
			c.showToolAvailabilityWarnings()
		}
	}
	if ctxLen := plan.ContextLength; ctxLen != nil && *ctxLen != 0 && *ctxLen < provider.MinimumContextLength {
		c.printLowContextWarning(*ctxLen)
	}
	if provider.IsNousHermesNonAgentic(c.model) {
		c.consolePrint("")
		c.consolePrint("[bold yellow]" + c.t("cli.banner.hermes_models_not_agentic", nil) + "[/]")
		c.consolePrint("[dim]   " + c.t("cli.banner.hermes_models_lack_tools", nil) + "[/]")
		c.consolePrint("[dim]   " + c.t("cli.banner.hermes_models_switch_hint", nil) + "[/]")
	}
	c.printProjectSkillsNotice()
	c.consolePrint("")
}

// Recomputes the tool panel snapshot in the background so a stale replay heals on the next launch
func (c *CLI) refreshBannerSnapshot() {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("banner snapshot refresh failed: %v", r))
		}
	}()
	toolDefs := toolDefinitionsFor(c, false)
	availability := computeToolsetAvailability(c.ctx, c.enabledToolsets)
	saveBannerSnapshot(c.ctx, toolDefs, c.enabledToolsets, availability, toolsetMap(toolDefs, availability, func(n string) string { return toolsetOfTool(c, n) }))
}

// Persists the banner snapshot, a failure only logged at debug
func (c *CLI) saveBannerSnapshotQuietly(toolDefs []map[string]any, availability map[string]any, failure string) {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("%s: %v", failure, r))
		}
	}()
	saveBannerSnapshot(c.ctx, toolDefs, c.enabledToolsets, availability, toolsetMap(toolDefs, availability, func(n string) string { return toolsetOfTool(c, n) }))
}

// Prints the context too low warning with the fix hint of the endpoint kind
func (c *CLI) printLowContextWarning(ctxLen int) {
	minimum := provider.MinimumContextLength
	c.consolePrint("")
	c.consolePrint("[yellow]" + c.t("cli.banner.context_too_low", map[string]any{"tokens": pystr.GroupThousands(int64(ctxLen))}) + "[/]")
	c.consolePrint("[dim]   " + c.t("cli.banner.context_minimum", map[string]any{"tokens": pystr.GroupThousands(int64(minimum))}) + "[/]")
	baseURL := c.baseURL
	raw := baseURL
	if !strings.Contains(baseURL, "://") {
		raw = "//" + baseURL
	}
	port, hasPort := 0, false
	if u, err := pyurl.Parse(raw); err == nil {
		if p, ok, perr := u.Port(); perr == nil && ok {
			port, hasPort = p, true
		}
	}
	var fix string
	switch {
	case hasPort && port == 11434:
		fix = c.t("cli.banner.fix_ollama", map[string]any{"tokens": strconv.Itoa(minimum)})
	case strings.Contains(mustBaseURLHostname(baseURL), "ollama"):
		fix = c.t("cli.banner.fix_ollama", map[string]any{"tokens": strconv.Itoa(minimum)})
	case hasPort && port == 1234:
		fix = c.t("cli.banner.fix_lm_studio", nil)
	case provider.IsLocalEndpoint(baseURL):
		fix = c.t("cli.banner.fix_local_server", map[string]any{"tokens_k": strconv.Itoa(minimum / 1000), "tokens": strconv.Itoa(minimum)})
	default:
		fix = c.t("cli.banner.fix_config", nil)
	}
	c.consolePrint("[dim]   " + fix + "[/]")
}

// Lowercased hostname of a base URL, the urlparse ValueError raised as in Python
func mustBaseURLHostname(baseURL string) string {
	host, err := pyurl.BaseURLHostname(baseURL)
	if err != nil {
		panic(err)
	}
	return host
}

// Prints the trusted project skill count or the untrusted project root hint, any failure only logged
func (c *CLI) printProjectSkillsNotice() {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("project skills banner notice failed: %v", r))
		}
	}()
	scope := skillsScope(c.ctx)
	if dirs := scope.ProjectSkillsDirs(); len(dirs) > 0 {
		n := 0
		for _, d := range dirs {
			n += len(skills.IterSkillIndexFiles(d, "SKILL.md"))
		}
		if n != 0 {
			c.consolePrint("[dim]" + c.t("cli.banner.project_skills_loaded", map[string]any{"count": strconv.Itoa(n)}) + "[/]")
		}
		return
	}
	if root, n := scope.UntrustedProjectSkillsRoot(); root != "" {
		c.consolePrint("[yellow]" + c.t("cli.banner.project_skills_untrusted", map[string]any{"count": strconv.Itoa(n), "root": root}) + "[/]")
	}
}

// Reports whether the model in use supports the fast service tier
func (c *CLI) fastCommandAvailable() bool {
	model := c.model
	if a := c.agent; a != nil && a.Model() != "" {
		model = a.Model()
	}
	return provider.ModelSupportsFastMode(model)
}

// Reports whether a slash command applies to the current model
func (c *CLI) commandAvailable(slashCommand string) bool {
	if slashCommand == "/fast" {
		return c.fastCommandAvailable()
	}
	return true
}

// helpRowEntry is one command and description row of a help section
type helpRowEntry struct{ cmd, desc string }

// Prints one padded command row of the help listing in the accent color
func helpRow(cmd, desc string, width int) {
	defaultChatConsole.Print(pyFmt("    [bold {accent}]{cmd:<{width}}[/] [dim]-[/] {desc}",
		map[string]any{"accent": accentHex(), "cmd": cmd, "width": width, "desc": pyrich.Escape(desc)}))
}

// Notes of skills a built in command shadows, the gateway's notice on its session, else this process's scan
func (c *CLI) skillCollisionNotes() []string {
	return gatewaySkillNotes(c, c.localSkillCollisionNotes)
}

// Notes of skills a built in command shadows from this process's own skill scan
func (c *CLI) localSkillCollisionNotes() []string {
	var notes []string
	for _, s := range skillsScope(c.ctx).FindAllSkills(false) {
		name, _ := s["name"].(string)
		if note, ok := skills.SkillCommandCollisionNote(name); ok && note != "" {
			notes = append(notes, note)
		}
	}
	return notes
}

// Prints the categorized help, the full skill list for /help skills, or the rows matching a query
func (c *CLI) showHelp(arg string) {
	arg = pystr.Strip(arg)
	skillCommands := helpSkillCommands(c)
	if l := pystr.Lower(arg); l == "skills" || l == "skill" {
		if len(skillCommands) > 0 {
			cprint("\n  ⚡ " + boldANSI + c.t("cli.help.skill_commands", nil) + resetANSI + " (" +
				c.t("cli.help.n_installed", map[string]any{"count": strconv.Itoa(len(skillCommands))}) + "):")
			for _, cmd := range sortedMapKeys(skillCommands) {
				desc, _ := skillCommands[cmd]["description"].(string)
				helpRow(cmd, desc, 22)
			}
		} else {
			cprint("\n  " + c.t("cli.help.no_skill_commands", nil))
		}
		for _, note := range skillNotesFor(c) {
			cprint("    " + dimANSI + "⚠ " + note + resetANSI)
		}
		cprint("")
		return
	}
	query := pystr.Lower(arg)
	defaultHeader := c.t("cli.help.header", nil)
	header := config.GetActiveHelpHeader(c.ctx, defaultHeader)
	if header = pystr.Strip(header); header == "" {
		header = defaultHeader
	}
	if r := []rune(header); len(r) > 55 {
		header = string(r[:55])
	}
	cprint("\n" + boldANSI + "+" + strings.Repeat("-", 55) + "+" + resetANSI)
	cprint(boldANSI + "|" + pyFmt("{h:^55}", map[string]any{"h": header}) + "|" + resetANSI)
	cprint(boldANSI + "+" + strings.Repeat("-", 55) + "+" + resetANSI)
	section := func(title string, rows []helpRowEntry) {
		printed := false
		for _, row := range rows {
			if !c.commandAvailable(row.cmd) {
				continue
			}
			if query != "" && !strings.Contains(pystr.Lower(row.cmd), query) && !strings.Contains(pystr.Lower(row.desc), query) {
				continue
			}
			if !printed {
				cprint("\n  " + boldANSI + "── " + helpSectionTitle(c.ctx, title) + " ──" + resetANSI)
				printed = true
			}
			helpRow(row.cmd, row.desc, 15)
		}
	}
	var categories []string
	byCategory := map[string][]helpRowEntry{}
	for _, d := range commands.All(commands.Default) {
		if commands.MetaOf(d.Name).GatewayOnly {
			continue
		}
		desc := d.Describe(c.ctx)
		if _, seen := byCategory[d.Category]; !seen {
			categories = append(categories, d.Category)
			byCategory[d.Category] = nil
		}
		rowDesc := desc
		if d.ArgsHint != "" {
			rowDesc = c.t("cli.help.usage_suffix", map[string]any{"description": desc, "command": d.Name, "args": d.ArgsHint})
		}
		rows := append(byCategory[d.Category], helpRowEntry{"/" + d.Name, rowDesc})
		for _, alias := range d.Aliases {
			rows = append(rows, helpRowEntry{"/" + alias, c.t("cli.help.alias_suffix", map[string]any{"description": desc, "command": d.Name})})
		}
		byCategory[d.Category] = rows
	}
	for _, category := range categories {
		if category != "Session" {
			section(category, byCategory[category])
			continue
		}
		subOf := map[string]string{}
		for _, sg := range commands.HelpSessionSubgroups {
			for _, n := range sg.Names {
				subOf["/"+n] = sg.Name
			}
		}
		buckets := map[string][]helpRowEntry{}
		for _, row := range byCategory[category] {
			sub, ok := subOf[row.cmd]
			if !ok {
				sub = "Session"
			}
			buckets[sub] = append(buckets[sub], row)
		}
		section("Session", buckets["Session"])
		for _, sg := range commands.HelpSessionSubgroups {
			section(sg.Name, buckets[sg.Name])
		}
	}
	if query != "" {
		var matched []string
		for _, cmd := range sortedMapKeys(skillCommands) {
			desc, _ := skillCommands[cmd]["description"].(string)
			if strings.Contains(pystr.Lower(cmd), query) || strings.Contains(pystr.Lower(desc), query) {
				matched = append(matched, cmd)
			}
		}
		if len(matched) > 0 {
			cprint("\n  ⚡ " + boldANSI + c.t("cli.help.skill_commands", nil) + resetANSI + " (" + c.t("cli.help.matching_query", map[string]any{"query": arg}) + "):")
			for _, cmd := range matched {
				desc, _ := skillCommands[cmd]["description"].(string)
				helpRow(cmd, desc, 22)
			}
		}
	} else if len(skillCommands) > 0 {
		cprint("\n  ⚡ " + boldANSI + c.t("cli.help.skill_commands", nil) + resetANSI + ": " +
			c.t("cli.help.n_installed", map[string]any{"count": strconv.Itoa(len(skillCommands))}) + " — " +
			dimANSI + "/help skills" + resetANSI + " " + c.t("cli.help.to_list_them", nil))
	}
	if bundles := helpSkillBundles(c); len(bundles) > 0 && query == "" {
		cprint("\n  ▣ " + boldANSI + c.t("cli.help.skill_bundles", nil) + resetANSI + " (" +
			c.t("cli.help.n_installed", map[string]any{"count": strconv.Itoa(len(bundles))}) + "):")
		for _, cmd := range sortedMapKeys(bundles) {
			info := bundles[cmd]
			list, _ := bannerListItems(info["skills"])
			count := strconv.Itoa(len(list))
			desc, _ := info["description"].(string)
			if desc == "" {
				desc = c.t("cli.help.load_n_skills", map[string]any{"count": count})
			}
			defaultChatConsole.Print(pyFmt("    [bold {accent}]{cmd:<22}[/] [dim]-[/] {desc} [dim]({n})[/]",
				map[string]any{"accent": accentHex(), "cmd": cmd, "desc": pyrich.Escape(desc), "n": c.t("cli.help.n_skills", map[string]any{"count": count})}))
		}
	}
	if rawQuick := c.config["quick_commands"]; pyrepr.Truthy(rawQuick) && query == "" {
		quick, ok := rawQuick.(map[string]any)
		if !ok {
			panic(fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(rawQuick)))
		}
		cprint("\n  ⚡ " + boldANSI + c.t("cli.help.quick_commands", nil) + resetANSI + " (" +
			c.t("cli.help.n_configured", map[string]any{"count": strconv.Itoa(len(quick))}) + "):")
		for _, name := range sortedMapKeys(quick) {
			qcmd, ok := quick[name].(map[string]any)
			if !ok {
				panic(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(quick[name])))
			}
			desc, hasDesc := qcmd["description"]
			if !hasDesc {
				desc = qcmd["type"]
				if desc == nil {
					desc = ""
				}
			}
			text, ok := desc.(string)
			if !ok {
				panic(fmt.Errorf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(desc)))
			}
			helpRow("/"+name, text, 22)
		}
	}
	if query != "" {
		cprint("\n  " + dimANSI + c.t("cli.help.filtered_by", map[string]any{"query": arg}) + resetANSI + "\n")
		return
	}
	cprint("\n  " + dimANSI + c.t("cli.help.tip_line", nil) + resetANSI)
	cprint("  " + dimANSI + c.t("cli.help.multiline_hint", nil) + resetANSI)
	cprint("  " + dimANSI + c.t("cli.help.draft_editor_hint", nil) + resetANSI)
	if hostinfo.IsTermux() {
		cprint("  " + dimANSI + c.t("cli.help.attach_image_hint", map[string]any{"path": termuxExampleImagePath("cat.png")}) + resetANSI + "\n")
	} else {
		cprint("  " + dimANSI + c.t("cli.help.paste_image_hint", nil) + resetANSI + "\n")
	}
}

// Keys of a string keyed map in sorted order
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Prints every tool of the full catalog grouped by toolset with its first description sentence
func (c *CLI) showTools() {
	toolsetFor := func(name string) string { return toolsetOfTool(c, name) }
	toolDefs, remoteToolsets, remote, err := gatewaySessionTools(c.ctx)
	if err != nil {
		panic(err)
	}
	if remote {
		toolsetFor = func(name string) string { return strOrEmpty(remoteToolsets[name]) }
	} else {
		toolDefs = toolDefinitionsFor(c, true)
	}
	if len(toolDefs) == 0 {
		infoPrint(c.t("cli.tools.none_available", nil))
		return
	}
	infoPrint("")
	asciiBox(c.t("cli.tools.header", nil), 78)
	infoPrint("")
	type toolRow struct{ name, desc string }
	sorted := slices.Clone(toolDefs)
	toolName := func(def map[string]any) string {
		fn, _ := def["function"].(map[string]any)
		name, _ := fn["name"].(string)
		return name
	}
	sort.SliceStable(sorted, func(i, j int) bool { return toolName(sorted[i]) < toolName(sorted[j]) })
	byToolset := map[string][]toolRow{}
	for _, def := range sorted {
		name := toolName(def)
		toolset := toolsetFor(name)
		if toolset == "" {
			toolset = "unknown"
		}
		fn, _ := def["function"].(map[string]any)
		desc, _ := fn["description"].(string)
		desc, _, _ = strings.Cut(desc, "\n")
		if i := strings.Index(desc, ". "); i >= 0 {
			desc = desc[:i+1]
		}
		byToolset[toolset] = append(byToolset[toolset], toolRow{name, desc})
	}
	for _, toolset := range sortedMapKeys(byToolset) {
		infoPrint("  [" + toolset + "]")
		for _, row := range byToolset[toolset] {
			infoPrint(pyFmt("    * {name:<20} - {desc}", map[string]any{"name": row.name, "desc": row.desc}))
		}
		infoPrint("")
	}
	infoPrint("  " + c.t("cli.tools.total", map[string]any{"count": strconv.Itoa(len(toolDefs))}))
	infoPrint("")
}

// Prints every toolset with the enabled ones starred
func (c *CLI) showToolsets() {
	infoPrint(strings.Join(toolsetsLines(c.ctx, c.enabledToolsets), "\n"))
}

// Login name of the user as getpass.getuser resolves it, the environment first then the account database, swapped by tests
var getpassUser = func() (string, error) {
	for _, name := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// Prints the slash command access of the local CLI surface
func (c *CLI) handleWhoamiCommand() {
	name, err := getpassUser()
	if err != nil || name == "" {
		name = "?"
	}
	label := func(key string) string { return pyFmt("{v:<15}", map[string]any{"v": c.t(key, nil)}) }
	infoPrint("")
	infoPrint("  " + label("cli.whoami.you_label") + " " + c.t("cli.whoami.you_value", nil))
	infoPrint("  " + label("cli.whoami.user_label") + " " + name)
	infoPrint("  " + label("cli.whoami.tier_label") + " " + c.t("cli.whoami.tier_value", nil))
	infoPrint("  " + label("cli.whoami.slash_commands_label") + " " + c.t("cli.whoami.slash_commands_value", nil))
	infoPrint("")
}

// Reports a slash command of the named set typed without images while the agent runs
func (c *CLI) busyInlineCommand(text string, hasImages bool, names ...string) bool {
	if text == "" || hasImages || !looksLikeSlashCommand(text) {
		return false
	}
	if running, _ := c.busyState(); !running {
		return false
	}
	words := pystr.SplitWhitespace(text, 1)
	if len(words) == 0 {
		return false
	}
	cmd := commands.Resolve(pystr.LstripChars(pystr.Lower(words[0]), "/"))
	if cmd == nil {
		return false
	}
	return slices.Contains(names, cmd.Name)
}

// Reports a steer or queue command to dispatch at once while the agent runs
func (c *CLI) shouldHandleSteerCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "steer", "queue")
}

// Reports a background or side question command to dispatch while the agent runs
func (c *CLI) shouldHandleBackgroundCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "bg", "btw")
}

// Reports a stop command to dispatch while the agent runs
func (c *CLI) shouldHandleStopCommandInline(text string, hasImages bool) bool {
	return c.busyInlineCommand(text, hasImages, "stop")
}

// Runs a bang shell submission without a model turn, false when not one or bang mode is off
func (c *CLI) handleBangShell(text string) bool {
	if !commands.IsBangCommand(text) || !commands.BangShellEnabled() {
		return false
	}
	command := commands.ParseBangCommand(text)
	if command == "" {
		c.consolePrint("[dim]" + commands.BangUsageHint + "[/]")
		return true
	}
	if approval := checkBangApproval(c.ctx, c.env, command); !approval.Approved {
		message := approval.Message
		if message == "" {
			reason := approval.Description
			if reason == "" {
				reason = c.t("cli.shell.flagged_as_dangerous", nil)
			}
			message = c.t("cli.shell.command_denied", map[string]any{"reason": reason})
		}
		c.consolePrint("[bold red]" + pyrich.Escape(message) + "[/]")
		return true
	}
	code := runBangCommand(c.ctx, c.env, command, commands.ResolveBangCwd(c.env, c.sessionID), commands.BangDefaultTimeout,
		func(line string) { c.consolePrintRenderable(&pyrich.Text{Plain: line}) })
	if code != 0 {
		c.consolePrint("[dim]" + c.t("cli.shell.exited", map[string]any{"code": strconv.Itoa(code)}) + "[/]")
	}
	return true
}

// Prints the messaging platforms the profile's gateway config enables
func (c *CLI) showGatewayStatus() {
	infoPrint(strings.Join(platformsLines(c.ctx), "\n"))
}

// Prints one discovery tip in the skin's dim banner colour, any failure printing nothing
func (c *CLI) printRandomTip() {
	defer func() { _ = recover() }()
	tip := getRandomTip(c.ctx)
	tipColor := "#B8860B"
	if skin := config.GetActiveSkin(c.ctx); skin != nil {
		tipColor = skin.GetColor("banner_dim", "#B8860B")
	}
	c.consolePrint("[dim " + tipColor + "]" + c.t("cli.tip_line", map[string]any{"tip": tip}) + "[/]")
}

// Cycles the tool progress mode off, new, all, verbose and releases focus view on the way
func (c *CLI) toggleVerbose() {
	idx := slices.Index(toolProgressCycle, c.toolProgressMode)
	if idx < 0 {
		idx = 2
	}
	c.toolProgressMode = toolProgressCycle[(idx+1)%len(toolProgressCycle)]
	if c.focusViewEnabled {
		c.focusViewEnabled = false
		c.focusSavedToolProgress = ""
		c.focusHiddenLines = 0
		c.focusLastCountedTool = ""
		func() {
			defer func() { _ = recover() }()
			SaveConfigValue(c.ctx, FocusConfigKey, false)
		}()
	}
	if c.agent != nil {
		c.agent.SetReasoningCallback(c.currentReasoningCallback())
		c.agent.SetToolProgressMode(c.toolProgressMode)
	}
	cprint(toolProgressLabel(c.ctx, c.toolProgressMode))
}

// Dispatches bare /usage to the usage view and /usage reset to the Codex reset credit redemption
func (c *CLI) handleUsageCommand(cmdOriginal string) {
	parts := pystr.SplitWhitespace(cmdOriginal, -1)
	var args []string
	for _, p := range parts[min(1, len(parts)):] {
		args = append(args, pystr.Lower(p))
	}
	if len(args) > 0 && args[0] == "reset" {
		c.usageReset(slices.Contains(args[1:], "--force"))
		return
	}
	if len(args) > 0 {
		infoPrint("  " + c.t("cli.usage.unknown_subcommand", map[string]any{"args": strings.Join(parts[1:], " ")}))
		return
	}
	c.showUsage()
}

// Provider, base URL or API key of the live agent, the CLI's own when the agent has none
func (c *CLI) agentOrSelf(name string) string {
	var fromAgent, own string
	switch name {
	case "provider":
		own = c.provider
		if c.agent != nil {
			fromAgent = c.agent.Provider()
		}
	case "base_url":
		own = c.baseURL
		if c.agent != nil {
			fromAgent = c.agent.BaseURL()
		}
	case "api_key":
		own = c.apiKey
	}
	if fromAgent != "" {
		return fromAgent
	}
	return own
}

// Redeems one banked Codex rate limit reset credit, refused off the openai-codex provider
func (c *CLI) usageReset(force bool) {
	if pystr.Lower(pystr.Strip(c.agentOrSelf("provider"))) != "openai-codex" {
		infoPrint("  " + c.t("cli.usage.reset_wrong_provider", nil))
		infoPrint("  " + c.t("cli.usage.reset_switch_hint", nil))
		return
	}
	infoPrint("  " + c.t("cli.usage.checking_reset_credits", nil))
	done := make(chan provider.CodexResetRedeemResult, 1)
	baseURL, apiKey := c.agentOrSelf("base_url"), c.agentOrSelf("api_key")
	go func() { done <- redeemCodexReset(c.ctx, baseURL, apiKey, force) }()
	select {
	case result := <-done:
		infoPrint("  " + result.Message)
	case <-time.After(45 * time.Second):
		infoPrint("  " + c.t("cli.usage.codex_timeout", nil))
		<-done
	}
}

// Prints the context window grid and category table of the live agent, the per item costs with all
func (c *CLI) showContextBreakdown(cmdOriginal string) {
	if connection := gatewayConnectionFor(c); connection != nil {
		c.showGatewayContextBreakdown(connection)
		return
	}
	infoPrint("  " + c.t("cli.shared.no_active_agent", nil))
}

// Main's context grid and table over the gateway agent's own breakdown of the session's prompt, tools and transcript
func (c *CLI) showGatewayContextBreakdown(connection *gatewaycli.GatewayConnection) {
	raw, err := connection.Call(c.ctx, "session.context_breakdown", map[string]any{"session_id": c.sessionID})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		infoPrint("  " + c.t("cli.context.compute_failed", map[string]any{"error": err.Error()}))
		return
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	if categories, _ := reply["categories"].([]any); len(categories) == 0 {
		infoPrint("  " + c.t("cli.shared.no_active_agent", nil))
		return
	}
	var payload agent.ContextBreakdown
	var sources []agent.ContextFileSource
	encoded, err := json.Marshal(reply)
	if err == nil {
		err = json.Unmarshal(encoded, &payload)
	}
	if files, isList := reply["context_files"].([]any); err == nil && isList {
		if encoded, err = json.Marshal(files); err == nil {
			err = json.Unmarshal(encoded, &sources)
		}
	}
	if err != nil {
		panic(err)
	}
	fileLines := agent.RenderContextFileLines(sources)
	model := payload.Model
	if model == "" {
		model = c.model
	}
	infoPrint("")
	infoPrint("  " + c.t("cli.context.header", map[string]any{"model": model}))
	infoPrint("")
	lines := agent.RenderContextBreakdownLines(c.ctx, payload, nil, true)
	if len(fileLines) > 0 {
		lines = append(append(lines, ""), fileLines...)
	}
	for _, line := range lines {
		infoPrint("  " + line)
	}
	infoPrint("")
}

// Prints rate limits, session token usage, account limits and the Nous balance, then applies the verbose log levels
func (c *CLI) showUsage() {
	creditsOr := func(fallback string) {
		shown := c.printAccountLimits()
		if c.printNousCreditsBlock() {
			c.printUsageCTA()
		} else if !shown {
			infoPrint(fallback)
		}
	}
	if c.agent == nil {
		creditsOr(c.t("cli.shared.no_active_agent", nil))
		return
	}
	r := readAgentUsage(c, c.agent)
	if r.Calls == 0 {
		creditsOr(c.t("cli.usage.no_api_calls", nil))
		return
	}
	if r.RateLimit.HasData() {
		infoPrint("")
		infoPrint(provider.FormatRateLimitDisplay(r.RateLimit, time.Now()))
		infoPrint("")
	}
	if !r.HasCompressor {
		panic(fmt.Errorf("'NoneType' object has no attribute 'last_prompt_tokens'"))
	}
	lastPrompt := max(0, r.LastPromptTokens)
	ctxLen := r.ContextLength
	pct := 0.0
	if ctxLen != 0 {
		pct = min(100, float64(lastPrompt)/float64(ctxLen)*100)
	}
	elapsed := provider.FormatDurationCompact(time.Since(c.sessionStart).Seconds())
	labelRow := func(key, value string) {
		infoPrint("  " + pyFmt("{v:<26}", map[string]any{"v": c.t(key, nil)}) + " " + value)
	}
	num := func(n int) string { return pyFmt("{v:>10,}", map[string]any{"v": n}) }
	infoPrint("  " + c.t("cli.usage.header_session", nil))
	infoPrint("  " + strings.Repeat("─", 40))
	labelRow("cli.usage.label_model", r.Model)
	labelRow("cli.usage.label_input_tokens", num(r.Input))
	labelRow("cli.usage.label_output_tokens", num(r.Output))
	if r.Reasoning != 0 {
		labelRow("cli.usage.label_reasoning_subset", num(r.Reasoning))
	}
	labelRow("cli.usage.label_prompt_tokens_total", num(r.Prompt))
	labelRow("cli.usage.label_completion_tokens", num(r.Completion))
	labelRow("cli.usage.label_total_tokens", num(r.Total))
	labelRow("cli.usage.label_api_calls", num(r.Calls))
	labelRow("cli.usage.label_session_duration", pyFmt("{v:>10}", map[string]any{"v": elapsed}))
	infoPrint("  " + strings.Repeat("─", 40))
	mark := ""
	if r.DisplaySource != "provider_usage" {
		mark = "~"
	}
	pinSuffix := agent.ContextPinSuffix(ctxLen, r.ConfigContextLength)
	short := func(key string) string { return pyFmt("{v:<17}", map[string]any{"v": c.t(key, nil)}) }
	infoPrint("  " + short("cli.usage.label_current_context") + " " + mark + pystr.GroupThousands(int64(lastPrompt)) + " / " +
		pystr.GroupThousands(int64(ctxLen)) + " (" + mark + strconv.FormatFloat(pct, 'f', 0, 64) + "%)" + pinSuffix)
	infoPrint("  " + short("cli.usage.label_messages") + " " + strconv.Itoa(len(c.conversationHistory)))
	infoPrint("  " + short("cli.usage.label_compressions") + " " + strconv.Itoa(r.Compressions))
	c.printAccountLimits()
	if c.printNousCreditsBlock() {
		c.printUsageCTA()
	}
	if c.verbose {
		logging.GetLogger("").SetLevel(logging.LevelDebug)
		for _, noisy := range []string{"openai", "openai._base_client", "httpx", "httpcore", "asyncio", "hpack", "grpc", "modal"} {
			logging.GetLogger(noisy).SetLevel(logging.LevelWarning)
		}
	} else {
		logging.GetLogger("").SetLevel(logging.LevelInfo)
	}
}

// Prints the provider account limits block of /usage with a ten second fetch cap, true when anything printed
func (c *CLI) printAccountLimits() bool {
	providerName := c.agentOrSelf("provider")
	if providerName == "" {
		return false
	}
	done := make(chan *provider.AccountUsageSnapshot, 1)
	opts := provider.AccountUsageFetchOptions{BaseURL: c.agentOrSelf("base_url"), APIKey: c.agentOrSelf("api_key")}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- nil
			}
		}()
		done <- fetchAccountUsage(c.ctx, providerName, opts)
	}()
	var snapshot *provider.AccountUsageSnapshot
	select {
	case snapshot = <-done:
	case <-time.After(10 * time.Second):
		<-done
	}
	lines := provider.RenderAccountUsageLines(snapshot, false)
	if len(lines) == 0 {
		return false
	}
	infoPrint("")
	for _, line := range lines {
		infoPrint("  " + line)
	}
	return true
}

// Shows the session history insights report for --days N or a bare N and an optional --source
func (c *CLI) showInsights(command string) {
	parts := pystr.SplitWhitespace(command, -1)
	days, source := 30, ""
	for i := 1; i < len(parts); {
		switch {
		case parts[i] == "--days" && i+1 < len(parts):
			n, ok := pyint.Parse(parts[i+1])
			if !ok {
				infoPrint("  " + c.t("gateway.insights.invalid_days", map[string]any{"value": parts[i+1]}))
				return
			}
			days = int(n.Int64())
			i += 2
		case parts[i] == "--source" && i+1 < len(parts):
			source = parts[i+1]
			i += 2
		default:
			if isPyDigit(parts[i]) {
				n, ok := pyint.Parse(parts[i])
				if !ok {
					panic(fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(parts[i])))
				}
				days = int(n.Int64())
			}
			i++
		}
	}
	insightsReport(c.ctx, console().Out, days, source)
}

// Runs fn and turns a raised panic into its error, as a Python except Exception clause
func pyExcept(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s", panicText(r))
		}
	}()
	return fn()
}

// Reports a non empty string of Unicode digit characters, as str.isdigit
func isPyDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Watches mcp_servers in config.yaml every five seconds and reloads MCP, or only notifies when auto reload is off
func (c *CLI) checkConfigMCPChanges() {
	now := monotonic()
	if now-c.tui.lastConfigCheck < configWatchInterval {
		return
	}
	c.tui.lastConfigCheck = now
	cfgPath := home.ConfigPath(home.Dir(c.ctx))
	sig, err := config.ConfigVersionOrNone(c.ctx, cfgPath)
	if err != nil {
		panic(err)
	}
	if sig == nil || reflect.DeepEqual(sig, c.tui.configSig) {
		return
	}
	c.tui.configSig = sig
	doc, err := config.ReadConfigDoc(c.ctx, cfgPath)
	if err != nil {
		return
	}
	newCfg, ok := doc.(map[string]any)
	if !ok {
		if pyrepr.Truthy(doc) {
			panic(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(doc)))
		}
		newCfg = map[string]any{}
	}
	rawMCP := newCfg["mcp_servers"]
	if !pyrepr.Truthy(rawMCP) {
		rawMCP = map[string]any{}
	}
	newMCP := config.Expand(rawMCP, config.EnvRefLookup)
	if pyrepr.Equal(newMCP, c.tui.configMCPServers) {
		return
	}
	auto := true
	if mcpCfg, ok := newCfg["mcp"].(map[string]any); ok {
		if v, present := mcpCfg["auto_reload_on_config_change"]; present {
			auto = pyrepr.Truthy(v)
		}
	}
	c.tui.configMCPServers = newMCP
	if !auto {
		infoPrint("")
		infoPrint(c.t("cli.mcp_watch.changed_reload_skipped", nil))
		infoPrint("   " + c.t("cli.mcp_watch.not_applied_run", nil))
		infoPrint("     /reload-mcp")
		infoPrint("   " + c.t("cli.mcp_watch.cache_note", nil))
		return
	}
	infoPrint("")
	infoPrint(c.t("cli.mcp_watch.changed_reloading", nil))
	go c.reloadMCPEntry()
}

// Runs the MCP reload, the instance override when a test installed one
func (c *CLI) reloadMCPEntry() {
	if c.reloadMCPHook != nil {
		c.reloadMCPHook()
		return
	}
	c.reloadMCP()
}

// Asks once, always or cancel behind approvals.mcp_reload_confirm and reloads MCP under the busy indicator
func (c *CLI) confirmAndReloadMCP(cmdOriginal string) {
	choice := gatedConfirm(c, "reload-mcp", "mcp_reload_confirm", gatedConfirmPrompt{
		Title: c.t("cli.reload_mcp.title", nil), Detail: c.t("cli.reload_mcp.detail", nil), Choices: reloadMCPChoices(c.ctx),
		Unchanged: c.t("cli.reload_mcp.unchanged", nil), AlwaysMsg: c.t("cli.reload_mcp.always_msg", nil), OnceVerb: c.t("cli.reload_mcp.once_verb", nil)})
	if choice == "" {
		return
	}
	if err := c.busyCommand(c.slowCommandStatus(cmdOriginal), true, c.reloadMCPEntry); err != nil {
		panic(err)
	}
}

// Python str of a connection key, the bare name or the scope and name tuple repr
func serverKeyText(k mcp.ServerKey) string {
	if !k.Scoped() {
		return k.Name
	}
	return "(" + pyrepr.StrRepr(k.Scope) + ", " + pyrepr.StrRepr(k.Name) + ")"
}

// Sorted comma joined connection keys, the TypeError Python raises for scoped keys returned
func joinServerKeys(keys []mcp.ServerKey) (string, error) {
	sorted := slices.Clone(keys)
	scoped, bare := 0, 0
	for _, k := range sorted {
		if k.Scoped() {
			scoped++
		} else {
			bare++
		}
	}
	if scoped > 0 && bare > 0 {
		return "", fmt.Errorf("'<' not supported between instances of 'str' and 'tuple'")
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Scope != sorted[j].Scope {
			return sorted[i].Scope < sorted[j].Scope
		}
		return sorted[i].Name < sorted[j].Name
	})
	names := make([]string, len(sorted))
	for i, k := range sorted {
		if k.Scoped() {
			return "", fmt.Errorf("sequence item %d: expected str instance, tuple found", i)
		}
		names[i] = k.Name
	}
	return strings.Join(names, ", "), nil
}

// Disconnects every MCP server, rereads config, reconnects, refreshes the agent tools and notes the change for the model
func (c *CLI) reloadMCP() {
	defer func() {
		if r := recover(); r != nil {
			infoPrint("  " + c.t("gateway.reload_mcp.failed", map[string]any{"error": panicText(r)}))
		}
	}()
	if connection := gatewayConnectionFor(c); connection != nil {
		c.reloadGatewayMCP(connection)
		return
	}
	if err := c.reloadMCPLocal(); err != nil {
		infoPrint("  " + c.t("gateway.reload_mcp.failed", map[string]any{"error": err.Error()}))
	}
}

// Reloads MCP on the gateway that owns the servers and the session's agent, then counts the session's tools
func (c *CLI) reloadGatewayMCP(connection *gatewaycli.GatewayConnection) {
	if _, running := c.busyState(); !running {
		infoPrint(c.t("cli.reload_mcp.reloading", nil))
	}
	raw, err := connection.Call(c.ctx, "reload.mcp", map[string]any{"session_id": c.sessionID, "confirm": true})
	var snapshot any
	if err == nil {
		snapshot, err = connection.Call(c.ctx, "session.resume", map[string]any{"session_id": c.sessionID})
	}
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		infoPrint("  " + c.t("gateway.reload_mcp.failed", map[string]any{"error": err.Error()}))
		return
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	infoPrint(pystr.Rstrip("  " + strOrEmpty(result["message"])))
	connection.State(c.sessionID).AdoptSnapshot(snapshot)
	snap, _ := pyjson.Plain(snapshot).(map[string]any)
	info, _ := snap["info"].(map[string]any)
	count, ok := pyIntValue(info["tool_count"])
	if !ok {
		count = 0
		groups, _ := info["tools"].(map[string]any)
		for _, names := range groups {
			list, _ := names.([]any)
			count += len(list)
		}
	}
	infoPrint("  " + c.t("cli.reload_mcp.agent_updated", map[string]any{"count": strconv.Itoa(count)}))
}

// Body of the MCP reload, the first failure returned for the failed line
func (c *CLI) reloadMCPLocal() error {
	oldServers := mcpServerKeys()
	if _, running := c.busyState(); !running {
		infoPrint(c.t("cli.reload_mcp.reloading", nil))
	}
	mcpShutdown(c.ctx)
	mcpReprobe()
	newTools, err := mcpDiscover(c)
	if err != nil {
		return err
	}
	connected := mcpServerKeys()
	in := func(set []mcp.ServerKey, k mcp.ServerKey) bool { return slices.Contains(set, k) }
	var added, removed, reconnected []mcp.ServerKey
	for _, k := range connected {
		if in(oldServers, k) {
			reconnected = append(reconnected, k)
		} else {
			added = append(added, k)
		}
	}
	for _, k := range oldServers {
		if !in(connected, k) {
			removed = append(removed, k)
		}
	}
	buckets := map[string][]mcp.ServerKey{"Added": added, "Removed": removed, "Reconnected": reconnected}
	for _, lk := range [][2]string{{"Reconnected", "cli.reload_mcp.reconnected"}, {"Added", "cli.reload_mcp.added"}, {"Removed", "cli.reload_mcp.removed"}} {
		if len(buckets[lk[0]]) == 0 {
			continue
		}
		names, err := joinServerKeys(buckets[lk[0]])
		if err != nil {
			return err
		}
		infoPrint("  " + c.t(lk[1], map[string]any{"names": names}))
	}
	if len(connected) == 0 {
		infoPrint("  " + c.t("gateway.reload_mcp.none_connected", nil))
	} else {
		infoPrint("  " + c.t("cli.reload_mcp.tools_available", map[string]any{"tools": strconv.Itoa(len(newTools)), "servers": strconv.Itoa(len(connected))}))
	}
	var changeParts []string
	for _, label := range []string{"Added", "Removed", "Reconnected"} {
		if len(buckets[label]) == 0 {
			continue
		}
		names, err := joinServerKeys(buckets[label])
		if err != nil {
			return err
		}
		changeParts = append(changeParts, label+" servers: "+names)
	}
	toolSummary := "No MCP tools available"
	if len(newTools) > 0 {
		toolSummary = strconv.Itoa(len(newTools)) + " MCP tool(s) now available"
	}
	changeDetail := ""
	if len(changeParts) > 0 {
		changeDetail = strings.Join(changeParts, ". ") + ". "
	}
	c.conversationHistory = append(c.conversationHistory, map[string]any{"role": "user",
		"content": "[IMPORTANT: MCP servers have been reloaded. " + changeDetail + toolSummary + ". The tool list for this conversation has been updated accordingly.]"})
	infoPrint("  " + c.t("cli.reload_mcp.agent_updated", map[string]any{"count": "0"}))
	return nil
}

// List items of a reload result entry as dicts, any other shape skipped as Python iteration would fail
func reloadItems(v any) []map[string]any {
	var out []map[string]any
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				panic(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item)))
			}
			out = append(out, m)
		}
	}
	return out
}

// Rescans skills, prints what changed and queues a one shot note for the next user message
func (c *CLI) reloadSkills() {
	defer func() {
		if r := recover(); r != nil {
			infoPrint("  " + c.t("gateway.reload_skills.failed", map[string]any{"error": panicText(r)}))
		}
	}()
	if _, running := c.busyState(); !running {
		infoPrint(c.t("cli.reload_skills.reloading", nil))
	}
	var result map[string]any
	if view := gatewayProcessView(); view != nil {
		raw, err := view.connection.Call(c.ctx, "skills.reload", map[string]any{"session_id": view.sessionID()})
		if err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			infoPrint("  " + c.t("gateway.reload_skills.failed", map[string]any{"error": err.Error()}))
			return
		}
		reply, _ := pyjson.Plain(raw).(map[string]any)
		result, _ = reply["result"].(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		invalidateGatewayCatalog(c)
	} else {
		var err error
		if result, err = reloadSkillsFor(c); err != nil {
			infoPrint("  " + c.t("gateway.reload_skills.failed", map[string]any{"error": err.Error()}))
			return
		}
	}
	added, removed := reloadItems(result["added"]), reloadItems(result["removed"])
	total := result["total"]
	if total == nil {
		total = 0
	}
	totalLine := "  " + c.t("cli.reload_skills.total", map[string]any{"count": pyrepr.Str(total)})
	if len(added) == 0 && len(removed) == 0 {
		infoPrint("  " + c.t("gateway.reload_skills.no_new", nil))
		infoPrint(totalLine)
		return
	}
	if len(added) > 0 {
		infoPrint("  " + c.t("cli.reload_skills.added_header", nil))
		for _, item := range added {
			infoPrint("  " + skillLine(item))
		}
	}
	if len(removed) > 0 {
		infoPrint("  " + c.t("cli.reload_skills.removed_header", nil))
		for _, item := range removed {
			infoPrint("  " + skillLine(item))
		}
	}
	infoPrint(totalLine)
	sections := []string{"[USER INITIATED SKILLS RELOAD:"}
	if len(added) > 0 {
		sections = append(sections, "", "Added Skills:")
		for _, item := range added {
			sections = append(sections, skillLine(item))
		}
	}
	if len(removed) > 0 {
		sections = append(sections, "", "Removed Skills:")
		for _, item := range removed {
			sections = append(sections, skillLine(item))
		}
	}
	sections = append(sections, "", "Use skills_list to see the updated catalog.]")
	c.pendingSkillsReloadNote = strings.Join(sections, "\n")
}
