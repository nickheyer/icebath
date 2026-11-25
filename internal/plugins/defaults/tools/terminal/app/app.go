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
	return []tui.Screen{{
		ID:    "terminal",
		Title: "Terminal",
		Short: "A shell in this directory, tiled beside the sessions",
		Open: func(kit *tui.Kit) tea.Cmd {
			s.terminals++
			return kit.WM.OpenHere(cards.NewTerminal(kit.Env, s.terminals, kit.Env.WorkDir()))
		},
	}}
}
