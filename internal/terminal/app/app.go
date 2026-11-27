package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Screen of the background processes of every session; Session processes, of the focused chat's session alone, is the terminal tool's
const Processes = "processes.all"

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	processes := terminalv1connect.NewProcessServiceClient(conn)
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	return []tui.Screen{{
		ID:      Processes,
		Group:   tui.GroupSessions,
		Order:   50,
		Title:   "Background processes",
		Short:   "See and stop every background process",
		Actions: ProcessActions,
		Where:   map[string]string{ProcessesContext: ProcessesWhere},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			return NewRoster(kit, processes, sessions, "").Open()
		},
	}, backendsScreen(backendsConn{backends: terminalv1connect.NewBackendServiceClient(conn), config: configv1connect.NewConfigServiceClient(conn)})}
}
