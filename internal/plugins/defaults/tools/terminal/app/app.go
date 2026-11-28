package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

type screens struct{}

func Register(registry *app.Registry, views *toolsapp.Registry) {
	registry.Register(screens{})
	views.Register(docks{})
	views.Register(lines{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	return []tui.Screen{
		processScreen(conn),
		{
			ID:    "terminal",
			Group: tui.GroupApp,
			Order: 20,
			Title: "New terminal",
			Short: "Open a shell in a new window",
			Slash: []string{"terminal"},
			Pickers: map[string]tui.Picker{
				terminalconfig.PickShell: {Open: pickShell},
			},
			Open: func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenHere(kit.Terminal()) },
			Follows: map[string]func(*tui.Kit, string) tea.Cmd{
				terminalconfig.Shell: func(kit *tui.Kit, value string) tea.Cmd {
					var shell string
					yaml.Unmarshal([]byte(value), &shell)
					// A command that starts with ~ starts a program of the home folder, as everywhere else a path is read
					return kit.Set(cards.TerminalShell, fsx.ExpandHome(strings.TrimSpace(shell)))
				},
				terminalconfig.Scrollback: func(kit *tui.Kit, value string) tea.Cmd {
					rows := cards.DefaultScrollback
					yaml.Unmarshal([]byte(value), &rows)
					return kit.Set(cards.TerminalScrollback, strconv.Itoa(rows))
				},
			},
		},
		{
			ID:    "terminal.tab",
			Group: tui.GroupApp,
			Order: 30,
			Title: "New terminal tab",
			Short: "Open a shell in a new tab of this window",
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenTab(kit.Terminal()) },
		},
	}
}
