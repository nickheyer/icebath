package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	terminalapp "github.com/nickheyer/icebath/internal/terminal/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the background processes of the focused chat's session; Background processes, of every session, is the terminal's own
const Processes = "processes"

// Screen of the background processes of the session the focused chat shows: listed, their output read, killed
func processScreen(conn *connect.Client) tui.Screen {
	processes := terminalv1connect.NewProcessServiceClient(conn)
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	return tui.Screen{
		ID:      Processes,
		Group:   tui.GroupSessions,
		Order:   40,
		Title:   "Session processes",
		Short:   "See and stop this session's background processes",
		Slash:   []string{"processes", "ps"},
		Usage:   "[id|kill <id>]",
		Actions: terminalapp.ProcessActions,
		Where:   map[string]string{terminalapp.ProcessesContext: terminalapp.ProcessesWhere},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			shown, chat := kit.Focused().(tui.Sessioned)
			if !chat {
				return wm.Notify("Focus a chat first. Session processes shows the background processes of the chat in focus.", wm.Info)
			}
			session := shown.Session()
			// A chat nothing was sent in has no session, so it runs nothing
			if session == "" {
				return wm.Notify("This chat has no background processes yet. Send a message first.", wm.Info)
			}
			roster := terminalapp.NewRoster(kit, processes, sessions, session)
			switch words := strings.Fields(argument); {
			case len(words) == 0:
				return roster.Open()
			case len(words) == 2 && words[0] == "kill":
				return roster.Kill(words[1])
			case len(words) == 1 && words[0] != "kill":
				return roster.Read(words[0])
			}
			return wm.Notify("/processes takes an id to read its output, or kill and an id, such as /processes kill proc_ab12cd.", wm.Warn)
		},
	}
}
