package commands

import (
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Category identifiers in the order the registry introduces them
const (
	CategorySession       = "Session"
	CategoryConfiguration = "Configuration"
	CategoryInfo          = "Info"
	CategoryToolsSkills   = "Tools & Skills"
	CategoryPlugins       = "Plugins"
	CategoryExit          = "Exit"
	CategoryContext       = "Context"
	CategoryBackground    = "Background & Automation"
)

// Meta carries the gateway and desktop fields the Def contract does not hold
type Meta struct {
	GatewayOnly        bool
	GatewayConfigGate  string
	Execute            string
	Desktop            string
	DesktopSubcommands []string
}

// Subgroup is one /help sub heading of the Session category with the names it collects
type Subgroup struct {
	Name  string
	Names []string
}

// IndicatorStyles lists the TUI busy indicator styles the /indicator command accepts
var IndicatorStyles = home.IndicatorStyles

// HelpSessionSubgroups splits the Session category into sub headings for /help
var HelpSessionSubgroups = []Subgroup{
	{Name: CategoryContext, Names: []string{"compress", "compact", "context", "ctx", "status"}},
	{Name: CategoryBackground, Names: []string{
		"bg", "btw", "agents", "tasks", "queue", "q", "steer", "s", "goal", "subgoal", "heartbeat", "hb",
		"refine", "loop", "proactive", "moa", "journey", "learning", "memory-graph"}},
}

// BusyRejectText maps a busy handler key to the refusal shown while the agent runs
var BusyRejectText = map[string]string{
	"model":         "Agent is running — wait or /stop first, then switch models.",
	"codex-runtime": "Agent is running — wait or /stop first, then change runtime.",
	"moa":           "Agent is running — wait or /stop first, then run /moa.",
}

// BusySpecialHandlers lists the busy handler keys that have a mid run variant
var BusySpecialHandlers = []string{"start", "stop", "new", "queue", "steer", "egress", "goal", "loop"}

var builtinMeta = map[string]Meta{}

func def(name, description, category string, meta Meta, d Def) Def {
	d.Name, d.Description, d.Category = name, description, category
	if d.BusyPolicy == "" {
		d.BusyPolicy = BusyReject
	}
	builtinMeta[name] = meta
	return d
}

// Builtin is every core slash command in registry order
var Builtin = []Def{
	def("start", "Acknowledge platform start pings without a reply", CategorySession,
		Meta{GatewayOnly: true}, Def{BusyPolicy: BusyDispatch, BusyHandler: "start"}),
	def("new", "Start a new session (fresh session ID + history)", CategorySession,
		Meta{}, Def{Aliases: []string{"reset"}, ArgsHint: "[name]", BusyPolicy: BusyInterruptThenDispatch, BusyHandler: "new"}),
	def("topic", "Enable or inspect Telegram DM topic sessions", CategorySession,
		Meta{GatewayOnly: true}, Def{ArgsHint: "[off|help|session-id]"}),
	def("clear", "Clear screen and start a new session", CategorySession,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true}),
	def("redraw", "Force a full UI repaint (recovers from terminal drift)", CategorySession,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true}),
	def("history", "Show conversation history", CategorySession, Meta{}, Def{CLIOnly: true}),
	def("save", "Export the current conversation (bare /save shows usage)", CategorySession,
		Meta{}, Def{ArgsHint: "<json|md|html> [filename] [redact]"}),
	def("retry", "Retry the last message (resend to agent)", CategorySession, Meta{}, Def{}),
	def("prompt", "Compose your next prompt in $EDITOR (markdown), then send it", CategorySession,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[initial text]", Aliases: []string{"compose"}}),
	def("undo", "Back up N user turns and re-prompt (default 1)", CategorySession, Meta{}, Def{ArgsHint: "[N]"}),
	def("title", "Set a title for the current session", CategorySession, Meta{}, Def{ArgsHint: "[name]"}),
	def("handoff", "Hand off this session to a messaging platform (Telegram, Discord, etc.)", CategorySession,
		Meta{}, Def{ArgsHint: "<platform>", CLIOnly: true, ArgumentMode: "options"}),
	def("branch", "Branch the current session (new thread on Discord/Telegram/Slack/Matrix; --here stays here)", CategorySession,
		Meta{}, Def{Aliases: []string{"fork"}, ArgsHint: "[--here] [name]"}),
	def("worktree", "Show, list, create, or prune isolated git worktrees", CategorySession,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, ArgsHint: "[new [name]|list|prune [--dry-run]]", Subcommands: []string{"new", "list", "prune"}}),
	def("compress", "Compress conversation context (add 'here [N]' to keep recent N turns; --preview shows what would happen)", CategorySession,
		Meta{}, Def{Aliases: []string{"compact"}, ArgsHint: "[here [N] | focus topic | --preview|--dry-run]"}),
	def("rollback", "List or restore filesystem checkpoints (restores keep your hand-edits; --all overrides)", CategorySession,
		Meta{}, Def{ArgsHint: "[number] [--all]"}),
	def("snapshot", "Create or restore state snapshots of icebath config/state", CategorySession,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, Aliases: []string{"snap"}, ArgsHint: "[create|restore <id>|prune]"}),
	def("export", "Export a profile (config, skills, theme) to a shareable archive", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[profile] [-o output.tar.gz]"}),
	def("import", "Import a shared profile archive as a new profile", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "<archive.tar.gz> [--name <name>]"}),
	def("stop", "Kill all running background processes", CategorySession,
		Meta{}, Def{BusyPolicy: BusyInterruptThenDispatch, BusyHandler: "stop"}),
	def("discard", "Drop a turn lost when icebath restarted so this chat's queue runs again", CategorySession,
		Meta{GatewayOnly: true}, Def{BusyPolicy: BusyDispatch}),
	def("pause", "Pause new work globally (emergency stop); '/pause off' resumes", CategorySession,
		Meta{GatewayOnly: true}, Def{ArgsHint: "[reason | off]", BusyPolicy: BusyDispatch}),
	def("approve", "Approve a pending dangerous command", CategorySession,
		Meta{GatewayOnly: true}, Def{ArgsHint: "[session|always]", BusyPolicy: BusyDispatch}),
	def("deny", "Deny a pending dangerous command (optionally with a reason)", CategorySession,
		Meta{GatewayOnly: true}, Def{ArgsHint: "[all] [reason]", BusyPolicy: BusyDispatch}),
	def("bg", "Run a prompt in a separate background session", CategorySession,
		Meta{}, Def{ArgsHint: "<prompt>", BusyPolicy: BusyDispatch}),
	def("btw", "Ask a side question about the current conversation without interrupting it", CategorySession,
		Meta{}, Def{ArgsHint: "<question>", BusyPolicy: BusyDispatch}),
	def("agents", "Show active agents and running tasks", CategorySession,
		Meta{}, Def{Aliases: []string{"tasks"}, BusyPolicy: BusyDispatch}),
	def("journey", "Open the learning journey timeline", CategorySession,
		Meta{}, Def{Aliases: []string{"learning", "memory-graph"}, CLIOnly: true, ArgsHint: "[list|delete <id>|edit <id>]", Subcommands: []string{"list", "delete", "edit"}}),
	def("queue", "Queue a prompt for the next turn, or list/edit/rm/move/clear queued prompts", CategorySession,
		Meta{}, Def{Aliases: []string{"q"}, ArgsHint: "[<prompt>|list|edit N <prompt>|rm N|move A B|clear]",
			Subcommands: []string{"list", "edit", "rm", "move", "clear", "add"}, BusyPolicy: BusyDispatch, BusyHandler: "queue"}),
	def("steer", "Inject a message after the next tool call without interrupting", CategorySession,
		Meta{}, Def{Aliases: []string{"s"}, ArgsHint: "<prompt>", BusyPolicy: BusyDispatch, BusyHandler: "steer"}),
	def("goal", "Set a standing goal icebath works on across turns until achieved", CategorySession,
		Meta{}, Def{ArgsHint: "[text | -- <text> | draft <text> | show | gate add <cmd> | pause | resume | clear | status | wait <pid> | unwait]",
			ArgumentMode: "mixed", BusyPolicy: BusyDispatch, BusyHandler: "goal"}),
	def("heartbeat", "Set a recurring prompt that re-enters this session when idle", CategorySession,
		Meta{}, Def{Aliases: []string{"hb"}, ArgsHint: "[every <interval> <prompt> | status | pause | resume | clear]",
			Subcommands: []string{"status", "pause", "resume", "clear"}, BusyPolicy: BusyDispatch}),
	def("refine", "Review this conversation now and save lessons to memory/skills", CategorySession,
		Meta{}, Def{ArgsHint: "[focus instructions]"}),
	def("review", "Spawn an independent subagent to review the work just discussed (PR, code, docs)", CategorySession,
		Meta{}, Def{ArgsHint: "[review instructions]"}),
	def("loop", "Re-run a prompt on a recurring interval in this session", CategorySession,
		Meta{}, Def{Aliases: []string{"proactive"},
			ArgsHint:     "[interval] <prompt> [--times N] [--until <condition>] | status | pause | resume | stop",
			ArgumentMode: "mixed", BusyPolicy: BusyDispatch, BusyHandler: "loop"}),
	def("plan", "Write a markdown implementation plan to .icebath/plans/ without executing anything", CategorySession,
		Meta{}, Def{ArgsHint: "[task]"}),
	def("moa", "Run one prompt through the default Mixture of Agents preset, then restore your model", CategorySession,
		Meta{}, Def{ArgsHint: "<prompt>", BusyPolicy: BusyReject, BusyHandler: "moa"}),
	def("subgoal", "Add or manage extra criteria on the active goal", CategorySession,
		Meta{}, Def{ArgsHint: "[text | remove N | clear]", BusyPolicy: BusyDispatch}),
	def("status", "Show session, model, token, and context info", CategorySession,
		Meta{}, Def{BusyPolicy: BusyDispatch}),
	def("egress", "Show Docker egress proxy status", CategorySession,
		Meta{Execute: "egress"}, Def{ArgsHint: "[status]", Subcommands: []string{"status"}, BusyPolicy: BusyDispatch, BusyHandler: "egress"}),
	def("context", "Show detailed context window view with usage gauge, category breakdown, compression stats, and throughput", CategorySession,
		Meta{}, Def{Aliases: []string{"ctx"}, ArgsHint: "[all]", Subcommands: []string{"all"}, BusyPolicy: BusyDispatch}),
	def("whoami", "Show your slash command access (admin / user)", CategoryInfo, Meta{}, Def{}),
	def("profile", "Show active profile name and home directory", CategoryInfo,
		Meta{Execute: "profile"}, Def{BusyPolicy: BusyDispatch}),
	def("sethome", "Set this chat as the home channel", CategorySession,
		Meta{GatewayOnly: true, Desktop: "terminal"}, Def{Aliases: []string{"set-home"}}),
	def("resume", "Resume a previously-named session", CategorySession,
		Meta{}, Def{ArgsHint: "[name]", ArgumentMode: "mixed"}),
	def("sessions", "Browse and resume previous sessions", CategorySession, Meta{}, Def{}),

	def("config", "Show current configuration", CategoryConfiguration, Meta{}, Def{CLIOnly: true}),
	def("model", "Switch model (session-scoped; --global to persist)", CategoryConfiguration,
		Meta{Desktop: "hidden"}, Def{ArgsHint: "[model|reset] [--provider name] [--reasoning level] [--global|--session] [--refresh]",
			BusyPolicy: BusyReject, BusyHandler: "model"}),
	def("codex-runtime", "Toggle codex app-server runtime for OpenAI/Codex models", CategoryConfiguration,
		Meta{}, Def{Aliases: []string{"codex_runtime"}, ArgsHint: "[auto|codex_app_server]", BusyPolicy: BusyReject, BusyHandler: "codex-runtime"}),
	def("personality", "Set a predefined personality", CategoryConfiguration,
		Meta{}, Def{ArgsHint: "[name]", ArgumentMode: "options"}),
	def("statusbar", "Toggle the context/model status bar", CategoryConfiguration,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, Aliases: []string{"sb"}}),
	def("battery", "Toggle a color-coded battery indicator in the status bar", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[on|off|status]", Subcommands: []string{"on", "off", "status"}}),
	def("timestamps", "Toggle [HH:MM] timestamps on messages and /history", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[on|off|status]", Subcommands: []string{"on", "off", "status"}, Aliases: []string{"ts"}}),
	def("diff", "Show git changes in the working directory", CategoryInfo,
		Meta{}, Def{ArgsHint: "[staged|all|session] [--stat] [path...]", Subcommands: []string{"staged", "all", "session"}}),
	def("verbose", "Cycle tool progress display: off -> new -> all -> verbose", CategoryConfiguration,
		Meta{GatewayConfigGate: "display.tool_progress_command", Desktop: "terminal"}, Def{CLIOnly: true, BusyPolicy: BusyDispatch}),
	def("focus", "Toggle focus view — show only your prompt and the final response", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[on|off|status]", Subcommands: []string{"on", "off", "status"}}),
	def("footer", "Toggle gateway runtime-metadata footer on final replies", CategoryConfiguration,
		Meta{Desktop: "terminal"}, Def{ArgsHint: "[on|off|status]", Subcommands: []string{"on", "off", "status"}, BusyPolicy: BusyDispatch}),
	def("yolo", "Toggle YOLO mode (skip all dangerous command approvals)", CategoryConfiguration,
		Meta{}, Def{BusyPolicy: BusyDispatch}),
	def("approvals", "Show or set the persistent dangerous-command approval mode", CategoryConfiguration,
		Meta{}, Def{ArgsHint: "[manual|smart|off]", Subcommands: []string{"manual", "smart", "off"}}),
	def("reasoning", "Manage reasoning effort and display", CategoryConfiguration,
		Meta{}, Def{ArgsHint: "[level|show|hide|full|clamp] [--global]",
			Subcommands: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "show", "hide", "on", "off", "full", "clamp", "--global"}}),
	def("fast", "Fast mode — OpenAI Priority Processing / Anthropic Fast Mode (normal/fast/auto/cold/ultrafast)", CategoryConfiguration,
		Meta{Desktop: "advanced"}, Def{ArgsHint: "[normal|fast|auto|cold|ultrafast|status] [--global]",
			Subcommands: []string{"normal", "fast", "auto", "cold", "ultrafast", "ultra", "status", "on", "off", "--global"}}),
	def("skin", "Show or change the display skin/theme", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[name]", ArgumentMode: "options"}),
	def("indicator", "Pick the TUI busy-indicator style", CategoryConfiguration,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, ArgsHint: "[" + strings.Join(IndicatorStyles, "|") + "]", Subcommands: append([]string(nil), IndicatorStyles...)}),
	def("voice", "Toggle voice mode", CategoryConfiguration,
		Meta{Desktop: "composer-voice"}, Def{ArgsHint: "[on|off|tts|status]", Subcommands: []string{"on", "off", "tts", "status"}}),
	def("wake", "Toggle the 'Hey icebath' wake word listener", CategoryConfiguration,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[on|off|status]", Subcommands: []string{"on", "off", "status"}}),
	def("busy", "Control how messages behave while icebath is working", CategoryConfiguration,
		Meta{Desktop: "terminal"}, Def{ArgsHint: "[queue|steer|interrupt|status]", Subcommands: []string{"queue", "steer", "interrupt", "status"}, BusyPolicy: BusyDispatch}),
	def("autoprune", "Turn automatic pruning of old sessions on or off", CategoryConfiguration,
		Meta{Execute: "autoprune"}, Def{ArgsHint: "[on|off|later|status]", Subcommands: []string{"on", "off", "later", "status"}, BusyPolicy: BusyDispatch, ArgumentMode: "options"}),

	def("tools", "Manage tools: /tools [list|disable|enable] [name...]", CategoryToolsSkills,
		Meta{}, Def{ArgsHint: "[list|disable|enable] [name...]", CLIOnly: true, ArgumentMode: "options"}),
	def("toolsets", "List available toolsets", CategoryToolsSkills, Meta{Desktop: "terminal"}, Def{CLIOnly: true}),
	def("skills", "Search, install, inspect, or manage skills", CategoryToolsSkills,
		Meta{GatewayConfigGate: "skills.write_approval", DesktopSubcommands: []string{"pending", "approve", "reject", "diff", "approval"}},
		Def{CLIOnly: true, Subcommands: []string{"search", "browse", "inspect", "install", "audit", "pending", "approve", "reject", "diff", "approval"}}),
	def("memory", "Review pending memory writes / toggle the approval gate", CategoryToolsSkills,
		Meta{}, Def{ArgsHint: "[pending|approve|reject|approval] [id|on|off]", Subcommands: []string{"pending", "approve", "reject", "approval"}}),
	def("bundles", "List skill bundles (aliases /<name> for multiple skills)", CategoryToolsSkills, Meta{Execute: "bundles"}, Def{}),
	def("pet", "Toggle or adopt a petdex mascot (/pet, /pet list, /pet <slug>)", CategoryToolsSkills,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[toggle|list|scale <n>|<slug>]", Subcommands: []string{"toggle", "list", "scale", "off"}}),
	def("hatch", "Generate a new petdex pet from a description", CategoryToolsSkills,
		Meta{}, Def{CLIOnly: true, Aliases: []string{"generate-pet"}, ArgsHint: "[description]"}),
	def("learn", "Learn a reusable skill from anything you describe (dirs, URLs, this chat, notes)", CategoryToolsSkills,
		Meta{}, Def{ArgsHint: "<what to learn from>"}),
	def("init", "Generate or update AGENTS.md project instructions from a repo scan", CategoryToolsSkills,
		Meta{}, Def{ArgsHint: "[notes]"}),
	def("cron", "Manage scheduled tasks", CategoryToolsSkills,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, ArgsHint: "[subcommand]", Subcommands: []string{"list", "add", "create", "edit", "pause", "resume", "run", "remove"}}),
	def("suggestions", "Review suggested automations (accept/dismiss)", CategoryToolsSkills,
		Meta{}, Def{Aliases: []string{"suggest"}, ArgsHint: "[accept|dismiss N | catalog]", Subcommands: []string{"accept", "dismiss", "catalog", "clear"}}),
	def("blueprint", "Set up an automation from a blueprint template", CategoryToolsSkills,
		Meta{}, Def{Aliases: []string{"bp"}, ArgsHint: "[name] [slot=value ...]"}),
	def("curator", "Background skill maintenance (status, run, pin, archive, list-archived)", CategoryToolsSkills,
		Meta{Desktop: "advanced"}, Def{ArgsHint: "[subcommand]", Subcommands: []string{"status", "run", "pause", "resume", "pin", "unpin", "restore", "list-archived"}}),
	def("kanban", "Multi-profile collaboration board (tasks, links, comments)", CategoryToolsSkills,
		Meta{Desktop: "advanced"}, Def{ArgsHint: "[subcommand]", BusyPolicy: BusyDispatch,
			Subcommands: []string{"init", "boards", "create", "list", "ls", "show", "assign",
				"reclaim", "reassign", "diagnostics", "diag", "link", "unlink",
				"claim", "comment", "complete", "edit", "block", "unblock",
				"archive", "tail", "dispatch", "stats", "notify-subscribe",
				"notify-list", "notify-unsubscribe", "log", "runs",
				"heartbeat", "assignees", "context", "specify", "gc"}}),
	def("reload", "Reload .env variables into the running session", CategoryToolsSkills, Meta{Desktop: "terminal"}, Def{CLIOnly: true}),
	def("reload-mcp", "Reload MCP servers from config", CategoryToolsSkills,
		Meta{Desktop: "advanced"}, Def{Aliases: []string{"reload_mcp"}}),
	def("reload-skills", "Re-scan ~/.icebath/skills/ for newly installed or removed skills", CategoryToolsSkills,
		Meta{Desktop: "advanced"}, Def{Aliases: []string{"reload_skills"}}),
	def("browser", "Connect browser tools to your live Chromium-family browser via CDP", CategoryToolsSkills,
		Meta{}, Def{CLIOnly: true, ArgsHint: "[connect|disconnect|status]", Subcommands: []string{"connect", "disconnect", "status"}}),
	def("plugins", "List installed plugins and their status", CategoryToolsSkills, Meta{Desktop: "terminal"}, Def{CLIOnly: true}),

	def("commands", "Browse all commands and skills (paginated)", CategoryInfo,
		Meta{GatewayOnly: true, Execute: "gateway_commands"}, Def{ArgsHint: "[page]", BusyPolicy: BusyDispatch}),
	def("help", "Show available commands (/help skills lists skill commands, /help <text> filters)", CategoryInfo,
		Meta{Execute: "gateway_help"}, Def{BusyPolicy: BusyDispatch, ArgsHint: "[skills|<filter>]"}),
	def("palette", "Open the fuzzy command palette (also Ctrl+P)", CategoryInfo, Meta{}, Def{CLIOnly: true, BusyPolicy: BusyDispatch}),
	def("restart", "Gracefully restart the gateway after draining active runs", CategorySession,
		Meta{GatewayOnly: true, Desktop: "terminal"}, Def{BusyPolicy: BusyDispatch}),
	def("usage", "Show token usage and rate limits; `reset` redeems a banked Codex limit reset", CategoryInfo,
		Meta{}, Def{ArgsHint: "[reset [--force]]"}),
	def("subscription", "View your Nous plan and change it in the browser", CategoryInfo,
		Meta{}, Def{CLIOnly: true, Aliases: []string{"upgrade"}}),
	def("login", "Sign in with a Nous account (keeps your connectors)", CategoryInfo,
		Meta{Desktop: "settings"}, Def{BusyPolicy: BusyDispatch}),
	def("topup", "Show your Nous balance and manage billing on the portal", CategoryInfo, Meta{}, Def{}),
	def("insights", "Show usage insights and analytics", CategoryInfo, Meta{Desktop: "advanced"}, Def{ArgsHint: "[days]"}),
	def("platforms", "Show gateway/messaging platform status", CategoryInfo,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, Aliases: []string{"gateway"}}),
	def("platform", "Pause, resume, or list a failing gateway platform", CategoryInfo,
		Meta{GatewayOnly: true}, Def{ArgsHint: "<pause|resume|list> [name]"}),
	def("copy", "Copy the last assistant response to clipboard", CategoryInfo,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, ArgsHint: "[number]"}),
	def("paste", "Attach clipboard image from your clipboard", CategoryInfo, Meta{Desktop: "terminal"}, Def{CLIOnly: true}),
	def("image", "Attach a local image file for your next prompt", CategoryInfo,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, ArgsHint: "<path>"}),
	def("update", "Update icebath to the latest version", CategoryInfo, Meta{Desktop: "terminal"}, Def{BusyPolicy: BusyDispatch}),
	def("version", "Show icebath version", CategoryInfo, Meta{Execute: "version"}, Def{Aliases: []string{"v"}, BusyPolicy: BusyDispatch}),
	def("debug", "Upload debug report (system info + logs) and get shareable links", CategoryInfo,
		Meta{}, Def{ArgsHint: "[nous|local]"}),

	def("quit", "Exit the CLI (use --delete to also remove session history)", CategoryExit,
		Meta{Desktop: "terminal"}, Def{CLIOnly: true, Aliases: []string{"exit"}, ArgsHint: "[--delete]"}),
}

// ExecuteKeys lists every registry owned executor key the app must bind
var ExecuteKeys = []string{"version", "egress", "profile", "bundles", "autoprune", "gateway_help", "gateway_commands"}

// Install registers every builtin command into a registry
func Install(r *Registry) {
	for _, d := range Builtin {
		r.Register(d)
	}
}

func init() { Install(Default) }

// MetaOf returns the side table entry for a canonical name or alias
func MetaOf(name string) Meta {
	if m, ok := builtinMeta[name]; ok {
		return m
	}
	if d := Default.Lookup(name); d != nil {
		return builtinMeta[d.Name]
	}
	return Meta{}
}

// Resolve maps a typed name or alias, leading slash optional, to its definition
func Resolve(name string) *Def {
	return Default.Lookup(strings.TrimLeft(strings.ToLower(name), "/"))
}

// All lists registry definitions in builtin order followed by extra registrations sorted
func All(r *Registry) []*Def {
	seen := map[string]bool{}
	out := make([]*Def, 0, len(Builtin))
	for _, b := range Builtin {
		if d := r.Lookup(b.Name); d != nil && d.Name == b.Name {
			out = append(out, d)
			seen[d.Name] = true
		}
	}
	for _, n := range r.Names() {
		if !seen[n] {
			out = append(out, r.Lookup(n))
		}
	}
	return out
}

// PluginEntries lists registered plugin commands sorted by name
func PluginEntries(r *Registry) []*Def {
	var out []*Def
	for _, n := range r.Names() {
		if d := r.Lookup(n); d != nil && d.Plugin != "" {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PluginDescription returns a plugin command description or the default wording
func PluginDescription(d *Def) string {
	if strings.TrimSpace(d.Description) == "" {
		return "Run /" + d.Name
	}
	return d.Description
}

var pipeSubsRe = regexp.MustCompile(`[a-z]+(?:\|[a-z]+)+`)

// Subcommands maps /name to its explicit subcommands or the pipe pattern found in the hint
func Subcommands(r *Registry) map[string][]string {
	out := map[string][]string{}
	for _, d := range All(r) {
		if len(d.Subcommands) > 0 {
			out["/"+d.Name] = append([]string(nil), d.Subcommands...)
		}
	}
	for _, d := range All(r) {
		if _, ok := out["/"+d.Name]; ok || d.ArgsHint == "" {
			continue
		}
		if m := pipeSubsRe.FindString(d.ArgsHint); m != "" {
			out["/"+d.Name] = strings.Split(m, "|")
		}
	}
	return out
}

var proseHints = []string{"<prompt>", "[text", "instructions", "[interval]", "<what"}

// InferArgumentMode returns the composer mode declared on the command or inferred from its hint
func InferArgumentMode(d *Def) string {
	switch d.ArgumentMode {
	case "options", "text", "mixed":
		return d.ArgumentMode
	}
	hint := strings.TrimSpace(d.ArgsHint)
	if len(d.Subcommands) > 0 {
		lower := strings.ToLower(hint)
		for _, token := range proseHints {
			if hint != "" && strings.Contains(lower, token) {
				return "mixed"
			}
		}
		return "options"
	}
	if hint != "" {
		return "text"
	}
	return ""
}

// DesktopMeta returns the commands.catalog wire shape for one command
func DesktopMeta(d *Def) map[string]any {
	m := MetaOf(d.Name)
	meta := map[string]any{"argument_mode": nilIfEmpty(InferArgumentMode(d)), "desktop": nilIfEmpty(m.Desktop)}
	if m.DesktopSubcommands != nil {
		meta["desktop_subcommands"] = append([]string(nil), m.DesktopSubcommands...)
	}
	return meta
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DesktopSurfaceRegistry maps every /name and alias to its desktop disposition, empty when offered
func DesktopSurfaceRegistry() map[string]string {
	out := map[string]string{}
	for _, d := range Builtin {
		m := builtinMeta[d.Name]
		out["/"+d.Name] = m.Desktop
		for _, a := range d.Aliases {
			out["/"+a] = m.Desktop
		}
	}
	return out
}

// GatewayKnownCommands lists every name and alias the gateway dispatches, config gated ones included
func GatewayKnownCommands() map[string]bool {
	out := map[string]bool{}
	for _, d := range Builtin {
		m := builtinMeta[d.Name]
		if d.CLIOnly && m.GatewayConfigGate == "" {
			continue
		}
		out[d.Name] = true
		for _, a := range d.Aliases {
			out[a] = true
		}
	}
	return out
}

// IsGatewayKnownCommand reports whether a name is a builtin or registered plugin gateway command
func IsGatewayKnownCommand(r *Registry, name string) bool {
	if name == "" {
		return false
	}
	if GatewayKnownCommands()[name] {
		return true
	}
	for _, p := range PluginEntries(r) {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ActiveSessionBypassCommands lists canonical names whose busy policy is not reject
func ActiveSessionBypassCommands() map[string]bool {
	out := map[string]bool{}
	for _, d := range Builtin {
		if d.BusyPolicy != BusyReject {
			out[d.Name] = true
		}
	}
	return out
}

// IsInterruptThenDispatch reports whether a name resolves to an interrupt first command
func IsInterruptThenDispatch(name string) bool {
	d := Resolve(name)
	return d != nil && d.BusyPolicy == BusyInterruptThenDispatch
}

// ShouldBypassActiveSession reports whether a name resolves to any command dispatched mid run
func ShouldBypassActiveSession(name string) bool {
	return name != "" && Resolve(name) != nil
}

// UsageSuffix formats a description with its usage hint
func UsageSuffix(description, name, argsHint string) string {
	return description + " (usage: /" + name + " " + argsHint + ")"
}

// AliasFor formats an alias row description
func AliasFor(description, name string) string {
	return description + " (alias for /" + name + ")"
}

// Row is one /command and description pair of a help listing
type Row struct {
	Command     string
	Description string
}

// Group is one help category with its rows in registry order
type Group struct {
	Category string
	Rows     []Row
}

// CommandsByCategory groups CLI visible commands and aliases by category in registry order
func CommandsByCategory(r *Registry) []Group {
	var groups []Group
	index := map[string]int{}
	for _, d := range All(r) {
		if MetaOf(d.Name).GatewayOnly {
			continue
		}
		i, ok := index[d.Category]
		if !ok {
			i = len(groups)
			index[d.Category] = i
			groups = append(groups, Group{Category: d.Category})
		}
		desc := d.Description
		if d.ArgsHint != "" {
			desc = UsageSuffix(d.Description, d.Name, d.ArgsHint)
		}
		groups[i].Rows = append(groups[i].Rows, Row{"/" + d.Name, desc})
		for _, a := range d.Aliases {
			groups[i].Rows = append(groups[i].Rows, Row{"/" + a, AliasFor(d.Description, d.Name)})
		}
	}
	return groups
}

// Commands maps every CLI visible /command and alias to its description
func Commands(r *Registry) map[string]string {
	out := map[string]string{}
	for _, g := range CommandsByCategory(r) {
		for _, row := range g.Rows {
			out[row.Command] = row.Description
		}
	}
	return out
}

// CommandNames lists every CLI visible /command and alias sorted
func CommandNames(r *Registry) []string {
	flat := Commands(r)
	out := make([]string, 0, len(flat))
	for k := range flat {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
