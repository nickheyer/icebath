package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Tool catalog of a session grouped by toolset with the total underneath
func toolsCatalogText(ctx context.Context, res tuigateway.ToolsShowResult) string {
	lines := []string{}
	for _, section := range res.Sections {
		lines = append(lines, "  ["+section.Name+"]")
		for _, row := range section.Tools {
			lines = append(lines, fmt.Sprintf("    * %-20s - %s", row.Name, row.Description))
		}
		lines = append(lines, "")
	}
	return strings.Join(append(lines, "  "+i18n.T(ctx, "cli.tools.total", "", map[string]any{"count": strconv.FormatInt(res.Total, 10)})), "\n")
}

// Toolsets the launch fixed for every session this terminal UI creates on the shared gateway, none when it follows the profile
func (a *app) launchToolsets() []string {
	var names []string
	for _, name := range strings.Split(a.gw.creationEnv()("ICEBATH_TUI_TOOLSETS"), ",") {
		if name = trimJS(name); name != "" {
			names = append(names, name)
		}
	}
	return names
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
				return tea.Batch(r.ran(), ask(r.rpc, r.sys, tuigateway.ToolsList, tuigateway.ToolsListParams{Platform: ptr("cli")}, func(res *tuigateway.ToolsListResult) tea.Cmd {
					if res == nil {
						return nil
					}
					return a.page(strings.Join(res.Lines, "\n"), i18n.T(a.ctx, "cli.tools.header", "", nil))
				}))
			case "enable", "disable":
				names := parts[1:]
				if len(names) == 0 {
					kwargs := map[string]any{"subcommand": subcommand}
					r.sys(strings.Join([]string{text("usage", kwargs), "  " + text("example_toolset", kwargs), "  " + text("example_mcp", kwargs)}, "\n"))
					return nil
				}
				if pinned := a.launchToolsets(); len(pinned) > 0 {
					r.sys(tr(txt.ToolsLaunchToolsets, strings.Join(pinned, ", ")))
					return nil
				}
				if a.life.guardBusySessionSwitch(tr(txt.SlashCmdCoreClearSwitchSessions)) {
					return nil
				}
				progress, changedKey := "enabling", txt.ToolsEnabled
				if subcommand == "disable" {
					progress, changedKey = "disabling", txt.ToolsDisabled
				}
				started := text(progress, map[string]any{"names": strings.Join(names, ", ")})
				r.sys(started)
				reset := func(res *tuigateway.ToolsConfigureResult) tea.Cmd {
					if res == nil {
						return nil
					}
					if len(res.LaunchToolsets) > 0 {
						r.sys(tr(txt.ToolsLaunchToolsets, strings.Join(res.LaunchToolsets, ", ")))
						return nil
					}
					var lines []string
					for _, name := range res.Unknown {
						lines = append(lines, tr(txt.ToolsUnknownToolset, name))
					}
					for _, server := range res.MissingServers {
						lines = append(lines, tr(txt.ToolsMissingServer, server))
					}
					if len(res.Changed) > 0 {
						lines = append(lines, tr(changedKey, strings.Join(res.Changed, ", ")))
					}
					if !res.Reset {
						if len(lines) > 0 {
							r.sys(strings.Join(lines, "\n"))
						}
						return nil
					}
					a.store.ui.Status = tr(txt.SlashCmdCoreClearForgingSession)
					return a.life.newSession(strings.Join(append(append([]string{started}, lines...), text("session_reset", nil)), "\n"), "", nil)
				}
				params := tuigateway.ToolsConfigureParams{Action: tuigateway.ToolsAction(subcommand), Names: names, SessionID: optionalText(r.sid)}
				return tea.Batch(r.ran(), ask(r.rpc, r.sys, tuigateway.ToolsConfigure, params, reset))
			}
			if r.sid == "" {
				r.sys(tr(txt.SlashCmdCoreStatusNoActiveSession))
				return nil
			}
			return tea.Batch(r.ran(), ask(r.rpc, r.sys, tuigateway.ToolsShow, tuigateway.SessionScopedParams{SessionID: &r.sid}, func(res *tuigateway.ToolsShowResult) tea.Cmd {
				if res == nil {
					return nil
				}
				if len(res.Sections) == 0 {
					r.sys(i18n.T(a.ctx, "cli.tools.none_available", "", nil))
					return nil
				}
				return a.page(toolsCatalogText(a.ctx, *res), i18n.T(a.ctx, "cli.tools.header", "", nil))
			}))
		}},
	}
}
