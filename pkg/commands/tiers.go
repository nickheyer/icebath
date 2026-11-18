package commands

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/spf13/cobra"
)

// Top level CLI commands outside the core and the tier that carries each
var CLITiers = map[string]string{
	"acp": linktier.Extras, "backup": linktier.Extras, "import": linktier.Extras, "browser": linktier.Extras, "vault": linktier.Extras,
	"computer-use": linktier.Extras, "cron": linktier.Extras, "kanban": linktier.Extras, "lsp": linktier.Extras, "pets": linktier.Extras,
	"update": linktier.Extras, "update-takeover": linktier.Extras, "update-finish": linktier.Extras, "update-restart-recovery": linktier.Extras,
	"update-completion": linktier.Extras, "venv-sync": linktier.Extras, "source-completion": linktier.Extras, "import-health": linktier.Extras,
	"pm": linktier.Extras, "security": linktier.Extras, "import-agent": linktier.Extras, "claw": linktier.Extras,
	"slack": linktier.Plugins, "whatsapp": linktier.Plugins, "whatsapp-cloud": linktier.Plugins, "send": linktier.Plugins,
	"camofox": linktier.Plugins, "google_chat": linktier.Plugins, "mcp-serve": linktier.Plugins, "meet": linktier.Plugins,
	"photon": linktier.Plugins, "spotify": linktier.Plugins, "teams-pipeline": linktier.Plugins,
}

// Slash commands outside the core and the tier that carries each
var SlashTiers = map[string]string{
	"voice": linktier.Extras, "wake": linktier.Extras, "pet": linktier.Extras, "hatch": linktier.Extras, "cron": linktier.Extras,
	"blueprint": linktier.Extras, "suggestions": linktier.Extras, "kanban": linktier.Extras, "browser": linktier.Extras,
	"update": linktier.Extras, "rooms": linktier.Extras,
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

// Hides every top level command of an unlinked tier and makes typing one name the build that has it
func (c *CLI) applyTiers(root *cobra.Command) {
	present := map[string]*cobra.Command{}
	for _, sub := range root.Commands() {
		present[sub.Name()] = sub
	}
	for name, tier := range CLITiers {
		msg, refused := linktier.Refusal("icebath "+name, tier)
		if !refused {
			continue
		}
		cmd := present[name]
		if cmd == nil {
			cmd = &cobra.Command{Use: name}
			root.AddCommand(cmd)
		}
		cmd.RemoveCommand(cmd.Commands()...)
		refuse := func(*cobra.Command, []string) error { return &ExitError{Code: 1, Msg: msg} }
		cmd.Hidden, cmd.DisableFlagParsing, cmd.Args, cmd.RunE = true, true, cobra.ArbitraryArgs, refuse
		c.handlers[name] = refuse
		BuiltinSubcommands[name] = true
	}
}

// Whether a top level command name belongs to a tier this binary leaves out
func cliTierHidden(name string) bool {
	tier, ok := CLITiers[name]
	return ok && !linktier.Linked(tier)
}

// Root subparser action without the commands of tiers this binary leaves out
func apDropTierChoices(a apAction) apAction {
	choices := make([]string, 0, len(a.Choices))
	for _, name := range a.Choices {
		if !cliTierHidden(name) {
			choices = append(choices, name)
		}
	}
	subs := make([]apChoice, 0, len(a.Sub))
	for _, s := range a.Sub {
		if !cliTierHidden(s.Name) {
			subs = append(subs, s)
		}
	}
	a.Choices, a.Sub = choices, subs
	return a
}
