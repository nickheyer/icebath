package app

import (
	"strconv"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(*connect.Client) []tui.Screen {
	return []tui.Screen{
		{
			ID:    "terminal",
			Title: "Terminal",
			Short: "Open a shell in this directory",
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
			Title: "Terminal tab",
			Short: "Open a shell as a tab of this window",
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenTab(kit.Terminal()) },
		},
	}
}
