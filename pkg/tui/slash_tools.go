package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Text entries of a gateway list answer
func rpcStrings(v any) []string {
	out := []string{}
	items, _ := v.([]any)
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// Tool catalog of a session grouped by toolset with the total underneath
func toolsCatalogText(ctx context.Context, res map[string]any) string {
	lines := []string{}
	sections, _ := res["sections"].([]any)
	for _, rawSection := range sections {
		section, _ := rawSection.(map[string]any)
		lines = append(lines, "  ["+jsValueString(section["name"])+"]")
		rows, _ := section["tools"].([]any)
		for _, rawRow := range rows {
			row, _ := rawRow.(map[string]any)
			lines = append(lines, fmt.Sprintf("    * %-20s - %s", jsValueString(row["name"]), jsValueString(row["description"])))
		}
		lines = append(lines, "")
	}
	total := jsValueString(res["total"])
	return strings.Join(append(lines, "  "+i18n.T(ctx, "cli.tools.total", "", map[string]any{"count": total})), "\n")
}

// Slash command that lists the session tools and enables or disables toolsets and MCP tools
func (a *app) toolsCommands() []slashCommand {
	text := func(key string, kwargs map[string]any) string {
		return commandText(a.ctx, "tools."+key, kwargs)
	}
	return []slashCommand{
		{name: "tools", help: "list tools, or enable and disable toolsets and MCP tools", usage: "[list | enable <name>... | disable <name>...]", suggest: func(string) []string {
			return []string{"list", "enable", "disable"}
		}, run: func(r *slashRun) tea.Cmd {
			parts, err := cards.ShellWords(r.arg)
			if err != nil {
				parts = strings.Fields(r.arg)
			}
			subcommand := ""
			if len(parts) > 0 {
				subcommand = parts[0]
			}
			switch subcommand {
			case "list":
				return tea.Batch(r.ran(), r.rpc("tools.list", map[string]any{"platform": "cli"}, func(res map[string]any) tea.Cmd {
					return a.page(strings.Join(rpcStrings(res["lines"]), "\n"), i18n.T(a.ctx, "cli.tools.header", "", nil))
				}))
			case "enable", "disable":
				names := parts[1:]
				if len(names) == 0 {
					kwargs := map[string]any{"subcommand": subcommand}
					r.sys(strings.Join([]string{text("usage", kwargs), "  " + text("example_toolset", kwargs), "  " + text("example_mcp", kwargs)}, "\n"))
					return nil
				}
				if a.life.guardBusySessionSwitch(tr("slashCmd.core.clear.switchSessions")) {
					return nil
				}
				progress, verb := "enabling", "Enabled"
				if subcommand == "disable" {
					progress, verb = "disabling", "Disabled"
				}
				started := text(progress, map[string]any{"names": strings.Join(names, ", ")})
				r.sys(started)
				reset := func(res map[string]any) tea.Cmd {
					lines := []string{started}
					for _, name := range rpcStrings(res["unknown"]) {
						lines = append(lines, fmt.Sprintf("\u2717 Unknown toolset '%s'", name))
					}
					for _, server := range rpcStrings(res["missing_servers"]) {
						lines = append(lines, fmt.Sprintf("\u2717 MCP server '%s' not found in config", server))
					}
					if changed := rpcStrings(res["changed"]); len(changed) > 0 {
						lines = append(lines, "\u2713 "+verb+": "+strings.Join(changed, ", "))
					}
					a.store.ui.Status = tr("slashCmd.core.clear.forgingSession")
					return a.life.newSession(strings.Join(append(lines, text("session_reset", nil)), "\n"), "", nil)
				}
				params := map[string]any{"action": subcommand, "names": names}
				if r.sid != "" {
					params["session_id"] = r.sid
				}
				return tea.Batch(r.ran(), r.rpc("tools.configure", params, reset))
			}
			if r.sid == "" {
				r.sys(tr("slashCmd.core.status.noActiveSession"))
				return nil
			}
			return tea.Batch(r.ran(), r.rpc("tools.show", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
				if sections, _ := res["sections"].([]any); len(sections) == 0 {
					r.sys(i18n.T(a.ctx, "cli.tools.none_available", "", nil))
					return nil
				}
				return a.page(toolsCatalogText(a.ctx, res), i18n.T(a.ctx, "cli.tools.header", "", nil))
			}))
		}},
	}
}
