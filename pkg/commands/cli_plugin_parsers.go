package commands

import (
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/spf13/cobra"
)

// Positionals plugin setups declared on commands no CLI has attached yet
var pendingPluginPositionals sync.Map

// Annotation naming the parser a plugin command asks the attaching CLI to build
const pluginParserKey = "plugin_parser"

// Parsers of plugin owned top level commands, built on the command a loaded plugin attaches
var pluginParsers = map[string]func(c *CLI, cmd *cobra.Command){
	"import-agent": (*CLI).buildImportAgent,
	"claw":         (*CLI).buildClaw,
	"mcp-serve":    (*CLI).buildMCPServe,
}

// Asks the CLI that attaches a plugin command to build the named parser on it
func UsePluginParser(parser any, name string) {
	cmd, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'add_argument'", pyrepr.TypeName(parser)))
	}
	if _, known := pluginParsers[name]; !known {
		panic("commands: no plugin parser named " + name)
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[pluginParserKey] = name
}

// Declares an optional positional with choices and a default on a plugin command, as add_argument(name, nargs="?", choices=..., default=...)
func PluginOptionalChoice(parser any, name, def string, choices ...string) {
	cmd, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'add_argument'", pyrepr.TypeName(parser)))
	}
	specs, _ := pendingPluginPositionals.Load(cmd)
	list, _ := specs.([]posSpec)
	pendingPluginPositionals.Store(cmd, append(list, pOptChoice(name, def, choices...)))
}

// Builds the requested parser and adopts declared positionals, every subparser running the command's own handler
func (c *CLI) buildPluginParser(cmd *cobra.Command) {
	c.adoptPluginPositionals(cmd)
	build := pluginParsers[cmd.Annotations[pluginParserKey]]
	if build == nil {
		return
	}
	build(c, cmd)
	c.argparseAbbrev(cmd)
	inheritPluginHandler(cmd, cmd.RunE)
}

// Moves the positionals a plugin setup declared on the command or its subcommands into this CLI
func (c *CLI) adoptPluginPositionals(cmd *cobra.Command) {
	if specs, ok := pendingPluginPositionals.LoadAndDelete(cmd); ok {
		c.pos(cmd, specs.([]posSpec)...)
	}
	for _, sub := range cmd.Commands() {
		c.adoptPluginPositionals(sub)
	}
}

// Gives every subcommand without a handler the handler of the plugin command, argparse set_defaults inheritance
func inheritPluginHandler(cmd *cobra.Command, run func(*cobra.Command, []string) error) {
	for _, sub := range cmd.Commands() {
		if sub.RunE == nil && sub.Run == nil {
			sub.RunE = run
		}
		inheritPluginHandler(sub, run)
	}
}

// Subparser of a plugin command whose handler the plugin command supplies
func pluginSubparser(parent *cobra.Command, name, short, long string, aliases ...string) *cobra.Command {
	cmd := &cobra.Command{Use: name, Short: short, Long: long, Aliases: aliases, SilenceUsage: true, SilenceErrors: true}
	parent.AddCommand(cmd)
	return cmd
}

// Default dirs the import-agent auto detection checks, as its help lists them
const importDirsHint = "~/.openclaw, ~/.claude, ~/.codex or ~/.cursor"

// Adds the OpenClaw only flags, defaults filled for claw migrate and left unset for import-agent
func openclawMigrateFlags(c *CLI, cmd *cobra.Command, defaults bool) {
	preset, conflict := "", ""
	if defaults {
		preset, conflict = "full", "skip"
	}
	sf(cmd, "preset", "", preset, "OpenClaw migration preset (default: full). Neither preset imports secrets — pass --migrate-secrets to include API keys.")
	c.choice(cmd, "preset", "user-data", "full")
	bf(cmd, "migrate-secrets", "", "OpenClaw: include allowlisted secrets (TELEGRAM_BOT_TOKEN, API keys, etc.). Required even under --preset full.")
	bf(cmd, "no-backup", "", "OpenClaw: skip the pre-migration zip snapshot of the Icebath home (by default one restore-point archive is written to its backups/ dir before apply; restorable with 'icebath import').")
	sf(cmd, "workspace-target", "", "", "OpenClaw: absolute path to copy workspace instructions into")
	sf(cmd, "skill-conflict", "", conflict, "OpenClaw: how to handle skill name conflicts (default: skip)")
	c.choice(cmd, "skill-conflict", "skip", "overwrite", "rename")
}

// Builds the agent-importers import-agent parser
func (c *CLI) buildImportAgent(ia *cobra.Command) {
	c.pos(ia, pOptChoice("agent", "", "openclaw", "claude-code", "codex", "cursor", "grok-bot", "openbot").withHelp("Which agent to import from (default: auto-detect "+importDirsHint+")"))
	sf(ia, "source", "", "", "Path to the agent's config directory (default: "+importDirsHint+"); for grok-bot, the export file the Bot wrote (or a folder of them, or - to read a pasted reply from stdin); for openbot, its user data folder (the one holding openbot.db)")
	bf(ia, "dry-run", "", "Preview only — stop after showing what would be imported")
	bf(ia, "overwrite", "", "Overwrite existing Icebath items on name conflicts (default: skip)")
	bf(ia, "yes", "y", "Skip confirmation prompts")
	bf(ia, "sync", "", "Re-import every previously imported source whose files changed since the last import (registered in ICEBATH_HOME/import-sync.json). Runs without prompts; combine with --dry-run to preview.")
	bf(ia, "from-setup", "", "")
	hide(ia, "from-setup")
	openclawMigrateFlags(c, ia, false)
	bf(ia, "activate-routines", "", "Grok Bot / OpenBot: create the imported cron jobs active (default: paused, so they don't run alongside the originals until you turn those off)")
	af(ia, "bot", "", "Grok Bot / OpenBot: import only this Bot or agent, by name (repeatable)")
}

// Builds the agent-importers claw parser with its migrate and cleanup subparsers
func (c *CLI) buildClaw(claw *cobra.Command) {
	subDest(claw, "claw_action")
	migrate := pluginSubparser(claw, "migrate", "Migrate from OpenClaw to Icebath (same as 'icebath import-agent openclaw')",
		"Import settings, memories, skills, and API keys from an OpenClaw installation. Always shows a preview before making changes.")
	sf(migrate, "source", "", "", "Path to OpenClaw directory (default: ~/.openclaw)")
	bf(migrate, "dry-run", "", "Preview only — stop after showing what would be migrated")
	bf(migrate, "overwrite", "", "Overwrite existing files (default: refuse to apply when the plan has conflicts)")
	openclawMigrateFlags(c, migrate, true)
	bf(migrate, "yes", "y", "Skip confirmation prompts")
	cleanup := pluginSubparser(claw, "cleanup", "Archive leftover OpenClaw directories after migration",
		"Scan for and archive leftover OpenClaw directories to prevent state fragmentation", "clean")
	sf(cleanup, "source", "", "", "Path to a specific OpenClaw directory to clean up")
	bf(cleanup, "dry-run", "", "Preview what would be archived without making changes")
	bf(cleanup, "yes", "y", "Skip confirmation prompts")
}

// Builds the mcp-serve parser with its verbose, tools and accept-hooks flags
func (c *CLI) buildMCPServe(serve *cobra.Command) {
	bf(serve, "verbose", "v", "Enable verbose logging on stderr")
	bf(serve, "tools", "", "Also expose Icebath's web, browser, vision, image generation, skills and TTS tools to the "+
		"MCP client, under this profile's credentials (off by default; never terminal, file or "+
		"kanban tools)")
	bf(serve, "accept-hooks", "", "With --tools: run the shell hooks in config.yaml without the first-use consent check")
}
