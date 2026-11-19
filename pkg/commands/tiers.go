package commands

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// CLI command paths outside the core and the tier that carries each, a top level name or a parent path and subcommand
var CLITiers = map[string]string{
	"acp": linktier.Extras, "backup": linktier.Extras, "import": linktier.Extras, "browser": linktier.Extras, "vault": linktier.Extras,
	"computer-use": linktier.Extras, "cron": linktier.Extras, "kanban": linktier.Extras, "lsp": linktier.Extras, "pets": linktier.Extras,
	"update": linktier.Extras, "update-takeover": linktier.Extras, "update-finish": linktier.Extras, "update-restart-recovery": linktier.Extras,
	"update-completion": linktier.Extras, "venv-sync": linktier.Extras, "source-completion": linktier.Extras, "import-health": linktier.Extras,
	"pm": linktier.Extras, "security": linktier.Extras, "import-agent": linktier.Extras, "claw": linktier.Extras,
	"slack": linktier.Plugins, "whatsapp": linktier.Plugins, "whatsapp-cloud": linktier.Plugins, "send": linktier.Plugins,
	"camofox": linktier.Plugins, "google_chat": linktier.Plugins, "mcp-serve": linktier.Plugins, "meet": linktier.Extras,
	"photon": linktier.Plugins, "spotify": linktier.Plugins, "teams-pipeline": linktier.Plugins,
	"pairing": linktier.Plugins, "gateway setup": linktier.Plugins, "project bind-board": linktier.Extras,
	"egress": linktier.Plugins,
}

// Top level commands a plugin adds once it is active, by command, the plugin key and whether it ships bundled
var pluginCommandOwners = map[string]struct {
	plugin  string
	bundled bool
}{
	"camofox": {"camofox", false}, "teams-pipeline": {"teams_pipeline", true}, "meet": {"google_meet", true},
}

// Line naming the plugin that adds an inactive top level command and the command that turns it on, empty for other names
func pluginCommandOwnerLine(value string) string {
	owner, ok := pluginCommandOwners[value]
	if !ok || !linktier.Linked(CLITiers[value]) {
		return ""
	}
	verb := "install"
	if owner.bundled {
		verb = "enable"
	}
	return fmt.Sprintf("`icebath %s` comes from the '%s' plugin, which is not active. Turn it on with: `icebath plugins %s %s`.", value, owner.plugin, verb, owner.plugin)
}

// Flags of a CLI command path that belong to a tier outside the core
var cliFlagTiers = map[string]map[string]string{
	"project create": {"board": linktier.Extras},
}

// Help texts naming the features of a tier and the text a binary without that tier prints in their place
var cliTierTexts = []struct{ tier, old, new string }{
	{linktier.Plugins, "Manage the messaging gateway (Telegram, Discord, WhatsApp, Weixin, and more)", "Manage the session gateway the terminal UI and webhooks run through"},
	{linktier.Plugins, "Messaging gateway management", "Session gateway management"},
	{linktier.Plugins, "Run messaging gateway", "Run the session gateway"},
	{linktier.Plugins, "Enable, disable, or list tools for CLI, Telegram, Discord, etc.", "Enable, disable, or list tools for the CLI and webhooks."},
	{linktier.Plugins, "Filter by platform (cli, telegram, discord, etc.)", "Filter by platform (cli, webhook, etc.)"},
	{linktier.Plugins, "Platform to simulate (cli, telegram, discord, ...). Default: cli", "Platform to simulate (cli, webhook). Default: cli"},
	{linktier.Extras, "Emergency stop: pause cron/kanban dispatch and new gateway turns", "Emergency stop: pause new gateway turns"},
	{linktier.Extras, "Halts NEW work only — cron dispatch, kanban dispatch, and new gateway turns — until", "Halts NEW work only — new gateway turns — until"},
	{linktier.Extras, "Read or set a profile's description (used by the kanban orchestrator)", "Read or set a profile's description"},
	{linktier.Extras, "They group sessions and, when bound to a kanban board, give tasks a deterministic worktree + branch convention.", "They group sessions."},
	{linktier.Extras, "Filter by component: gateway, agent, tools, cli, cron, gui", "Filter by component: gateway, agent, tools, cli, gui"},
	{linktier.Extras, "agent.log / errors.log / gateway.log / mcp-stderr.log / update.log", "agent.log / errors.log / gateway.log / mcp-stderr.log"},
	{linktier.Extras, "Log to view: agent (default), errors, gateway, mcp, update, or", "Log to view: agent (default), errors, gateway, mcp, or"},
	{linktier.Extras, "per configured tool backend (Firecrawl/FAL/browser/MCP/TTS/STT)", "per configured tool backend (MCP and the web backends)"},
}

// Slash commands outside the core and the tier that carries each
var SlashTiers = linktier.SlashTiers

// Slash command descriptions naming the features of a tier and the description a binary without that tier shows
var slashTierTexts = []struct{ tier, name, key, text string }{
	{linktier.Plugins, "branch", "core_description", "Branch the current session (--here stays here)"},
}

// Locale key under slash.<name> of the description a binary without the command's tier text shows, false when the usual one applies
func slashTierDescriptionKey(name string) (string, bool) {
	for _, t := range slashTierTexts {
		if t.name == name && !linktier.Linked(t.tier) {
			return t.key, true
		}
	}
	return "", false
}

// ApplySlashTierTexts rewrites the builtin slash descriptions naming a feature of an unlinked tier, run once before any entry
func ApplySlashTierTexts() {
	for _, t := range slashTierTexts {
		if linktier.Linked(t.tier) {
			continue
		}
		for i := range Builtin {
			if Builtin[i].Name == t.name {
				Builtin[i].Description = t.text
			}
		}
		Default.mu.Lock()
		if d := Default.defs[t.name]; d != nil {
			d.Description = t.text
		}
		Default.mu.Unlock()
	}
}

// Whether a slash command name or alias belongs to a tier this binary links
func SlashLinked(name string) bool {
	tier, ok := SlashTiers[strings.TrimLeft(strings.ToLower(name), "/")]
	return !ok || linktier.Linked(tier)
}

// Refusal of a typed slash command whose tier this binary leaves out
func SlashTierRefusal(name string) (string, bool) {
	bare := strings.TrimLeft(strings.ToLower(name), "/")
	return linktier.Refusal("/"+bare, SlashTiers[bare])
}

// Refusal of a CLI command path whose tier this binary leaves out
func CLITierRefusal(path string) (string, bool) {
	return linktier.Refusal("icebath "+path, CLITiers[path])
}

// Direct subcommand of a command by name
func childNamed(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}

// Hides every command path and flag of an unlinked tier, makes typing one name the build that has it, and rewords help naming such a tier
func (c *CLI) applyTiers(root *cobra.Command) {
	for path, tier := range CLITiers {
		msg, refused := linktier.Refusal("icebath "+path, tier)
		if !refused {
			continue
		}
		words := strings.Fields(path)
		parent := root
		for _, w := range words[:len(words)-1] {
			if parent = childNamed(parent, w); parent == nil {
				break
			}
		}
		if parent == nil {
			continue
		}
		name := words[len(words)-1]
		cmd := childNamed(parent, name)
		if cmd == nil {
			cmd = &cobra.Command{Use: name}
			parent.AddCommand(cmd)
		}
		cmd.RemoveCommand(cmd.Commands()...)
		refuse := func(*cobra.Command, []string) error { return &ExitError{Code: 1, Msg: msg} }
		cmd.Hidden, cmd.DisableFlagParsing, cmd.Args, cmd.RunE = true, true, cobra.ArbitraryArgs, refuse
		c.handlers[path] = refuse
		if parent == root {
			BuiltinSubcommands[name] = true
		}
	}
	for path, flags := range cliFlagTiers {
		cmd := root
		for _, w := range strings.Fields(path) {
			if cmd = childNamed(cmd, w); cmd == nil {
				break
			}
		}
		if cmd == nil {
			continue
		}
		c.refuseTierFlags(cmd, flags)
	}
	applyTierTexts(root)
}

// Hides the flags of unlinked tiers on one command
func (c *CLI) refuseTierFlags(cmd *cobra.Command, flags map[string]string) {
	for name, tier := range flags {
		if fl := cmd.Flags().Lookup(name); fl != nil && !linktier.Linked(tier) {
			fl.Hidden = true
		}
	}
}

// Refusal of a run that sets a flag of a tier this binary leaves out, nil otherwise
func tierFlagRefusal(path string, cmd *cobra.Command) error {
	for name, tier := range cliFlagTiers[path] {
		if msg, refused := linktier.Refusal("icebath "+path+" --"+name, tier); refused && cmd.Flags().Changed(name) {
			return &ExitError{Code: 1, Msg: msg}
		}
	}
	return nil
}

// Whether a flag of a command belongs to a tier this binary leaves out
func cliFlagTierHidden(cmd *cobra.Command, opts []string) bool {
	flags := cliFlagTiers[apPath(cmd)]
	for _, o := range opts {
		if tier, ok := flags[strings.TrimLeft(o, "-")]; ok && !linktier.Linked(tier) {
			return true
		}
	}
	return false
}

// Rewrites every help text of the tree and the parser table that names a feature of an unlinked tier
func applyTierTexts(root *cobra.Command) {
	var replace []string
	for _, t := range cliTierTexts {
		if !linktier.Linked(t.tier) {
			replace = append(replace, t.old, t.new)
		}
	}
	if len(replace) == 0 {
		return
	}
	r := strings.NewReplacer(replace...)
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		cmd.Short, cmd.Long = r.Replace(cmd.Short), r.Replace(cmd.Long)
		if epilog := cmd.Annotations["epilog"]; epilog != "" {
			cmd.Annotations["epilog"] = dropTierEpilogLines(r.Replace(epilog))
		}
		cmd.LocalFlags().VisitAll(func(fl *pflag.Flag) { fl.Usage = r.Replace(fl.Usage) })
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
	for _, p := range argparseTree {
		for i := range p.Acts {
			p.Acts[i].Help = r.Replace(p.Acts[i].Help)
			for j := range p.Acts[i].Sub {
				p.Acts[i].Sub[j].Help = r.Replace(p.Acts[i].Sub[j].Help)
			}
		}
	}
}

// Epilog examples that show the output of a feature of a tier and that tier
var cliTierEpilogPrefixes = []struct{ tier, prefix string }{
	{linktier.Extras, "icebath logs update "},
}

// Epilog without the example lines that run a command of an unlinked tier or show its output
func dropTierEpilogLines(epilog string) string {
	lines := strings.Split(epilog, "\n")
	kept := lines[:0]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "icebath" && cliTierHidden(fields[1]) {
			continue
		}
		if tierEpilogExample(strings.TrimSpace(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Whether a CLI command path belongs to a tier this binary leaves out
func cliTierHidden(path string) bool {
	tier, ok := CLITiers[path]
	return ok && !linktier.Linked(tier)
}

// Subparser action of a parser without the subcommands of tiers this binary leaves out
func apDropTierChoices(path string, a apAction) apAction {
	prefix := ""
	if path != "" {
		prefix = path + " "
	}
	choices := make([]string, 0, len(a.Choices))
	for _, name := range a.Choices {
		if !cliTierHidden(prefix + name) {
			choices = append(choices, name)
		}
	}
	subs := make([]apChoice, 0, len(a.Sub))
	for _, s := range a.Sub {
		if !cliTierHidden(prefix + s.Name) {
			subs = append(subs, s)
		}
	}
	a.Choices, a.Sub = choices, subs
	return a
}

// Whether an epilog line shows the output of a feature of an unlinked tier
func tierEpilogExample(line string) bool {
	for _, e := range cliTierEpilogPrefixes {
		if strings.HasPrefix(line, e.prefix) && !linktier.Linked(e.tier) {
			return true
		}
	}
	return false
}
