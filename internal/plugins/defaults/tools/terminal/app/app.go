package app

import (
	"strconv"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

type screens struct{}

func Register(registry *app.Registry, views *toolsapp.Registry) {
	registry.Register(screens{})
	views.Register(docks{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	return []tui.Screen{
		processScreen(conn),
		{
			ID:    "terminal",
			Group: tui.GroupApp,
			Order: 20,
			Title: "Terminal",
			Short: "Open a shell in this folder",
			Slash: []string{"terminal"},
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenHere(kit.Terminal()) },
			Follows: map[string]func(*tui.Kit, string) tea.Cmd{
				terminalconfig.Shell: func(kit *tui.Kit, value string) tea.Cmd {
					var shell string
					yaml.Unmarshal([]byte(value), &shell)
					return kit.Set(cards.TerminalShell, shell)
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
			Title: "Terminal tab",
			Short: "Open a shell in a new tab of this window",
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenTab(kit.Terminal()) },
		},
	}
}
