package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

type screens struct {
	terminals int
}

func Register(registry *app.Registry) {
	registry.Register(new(screens))
}

func (s *screens) Screens(*connect.Client) []tui.Screen {
	shell := func(kit *tui.Kit) *cards.Terminal {
		s.terminals++
		return cards.NewTerminal(kit.Env, s.terminals, kit.Env.WorkDir())
	}
	return []tui.Screen{
		{
			ID:       "terminal",
			Title:    "Terminal",
			Short:    "A shell in this directory, tiled beside the sessions",
			Commands: []string{"terminal.new"},
			Open:     func(kit *tui.Kit) tea.Cmd { return kit.WM.OpenHere(shell(kit)) },
		},
		{
			ID:       "terminal.tab",
			Title:    "Terminal tab",
			Short:    "A shell as a tab of the window in focus",
			Commands: []string{"terminal.tab"},
			Open:     func(kit *tui.Kit) tea.Cmd { return kit.WM.OpenTab(shell(kit)) },
		},
	}
}
