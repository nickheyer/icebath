//go:build !icebath_core && !icebath_noextras

package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Adds /cron, /blueprint and /suggestions to the slash dispatch table
func init() {
	slashDispatch["cron"] = withArg((*CLI).handleCronCommand)
	slashDispatch["blueprint"] = withArg((*CLI).handleBlueprintCommand)
	slashDispatch["suggestions"] = withArg((*CLI).handleSuggestionsCommand)
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

// Handles /suggestions through the shared handler with the local platform as the CLI origin
func (c *CLI) handleSuggestionsCommand(cmd string) {
	args := strings.Join(shlexArgs(cmd), " ")
	output, err := cron.HandleSuggestionsCommand(c.ctx, args, nil, "cli")
	if err != nil {
		output = c.commandsT("suggestions.failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	c.consolePrint(output)
}
