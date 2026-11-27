package app

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the background process list
const processesContext = "terminal.processes"

var processKeys = []keys.Action{
	{ID: "terminal.processes.read", Context: processesContext, Label: "read", Doc: "page the output of the background process", Default: []string{"enter"}},
	{ID: "terminal.processes.kill", Context: processesContext, Label: "kill", Doc: "kill the background process", Default: []string{"x"}},
}

// Screen of the background processes of the session the focused chat shows: listed, paged, killed
func processScreen(conn *connect.Client) tui.Screen {
	service := terminalv1connect.NewProcessServiceClient(conn)
	return tui.Screen{
		ID:      "processes",
		Title:   "Processes",
		Short:   "List the background processes of the session, read their output or kill one",
		Slash:   []string{"processes", "ps"},
		Usage:   "[id|kill <id>]",
		Actions: processKeys,
		Where:   map[string]string{processesContext: "background processes"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			session := ""
			if shown, ok := kit.Focused().(tui.Sessioned); ok {
				session = shown.Session()
			}
			if session == "" {
				return wm.Notify("No background processes in this session.", wm.Info)
			}
			p := &roster{kit: kit, service: service, session: session}
			switch words := strings.Fields(argument); {
			case len(words) == 0:
				return p.open()
			case len(words) == 2 && words[0] == "kill":
				return p.kill(words[1], nil)
			case len(words) == 1:
				return p.read(words[0])
			}
			return wm.Notify("Usage: /processes [id|kill <id>]", wm.Warn)
		},
	}
}

// Background processes of one session in a list
type roster struct {
	kit     *tui.Kit
	service terminalv1connect.ProcessServiceClient
	session string
	rows    []*terminalv1.Process
	loaded  bool
	err     error
}

func (p *roster) open() tea.Cmd {
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Processes",
		Glyph:   p.kit.Env.Theme.Glyphs.Jobs,
		Key:     "terminal:processes",
		Context: processesContext,
		Primary: "terminal.processes.read",
		Columns: []render.Column{{Title: "ID"}, {Title: "State"}, {Title: "Command", Flex: 3}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, process := range p.rows {
				rows[i] = []string{process.GetId(), state(process), oneLine(process.GetCommand())}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(p.rows) {
				return nil
			}
			return []string{lastLine(p.rows[i].GetOutputTail())}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "No background processes in this session.") },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"terminal.processes.read": func(_ *cards.List, i int) tea.Cmd { return p.page(p.rows[i]) },
			"terminal.processes.kill": func(l *cards.List, i int) tea.Cmd { return p.kill(p.rows[i].GetId(), l) },
		},
	})
	return tea.Batch(p.kit.WM.ShowOverlay(p.kit.Hub("terminal:processes", list)), p.load(nil))
}

// Reads the session's processes again, the list repainted once they arrive
func (p *roster) load(list *cards.List) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &terminalv1.ListRequest{SessionId: p.session})
		return func() tea.Cmd {
			p.rows, p.err, p.loaded = response.GetProcesses(), err, true
			if list != nil {
				list.Refresh()
			}
			return nil
		}
	})
}

// How a listed process stands: how long it runs, or how it ended
func state(process *terminalv1.Process) string {
	switch s := status(process); s {
	case "running":
		return fmt.Sprintf("running · %ds", process.GetUptimeSeconds())
	case "killed", "lost":
		return s
	default:
		if process.GetHasExitCode() {
			return fmt.Sprintf("%s · exit %d", s, process.GetExitCode())
		}
		return s
	}
}

// Pages the newest output of the process
func (p *roster) page(process *terminalv1.Process) tea.Cmd {
	tail := process.GetOutputTail()
	if strings.TrimSpace(tail) == "" {
		return wm.Notify(process.GetId()+" has written no output yet", wm.Info)
	}
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	page := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title: process.GetId() + " · " + compact(oneLine(process.GetCommand()), 40),
		Key:   "terminal:processes:" + process.GetId(),
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, w), "\n")...)
			}
			return out
		},
	})
	return p.kit.WM.ShowOverlay(p.kit.Hub("terminal:processes:"+process.GetId(), page))
}

// Pages the newest output of the session's process of the id, read anew
func (p *roster) read(id string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &terminalv1.ListRequest{SessionId: p.session})
		return func() tea.Cmd {
			if err != nil {
				return wm.Notify("Could not list the processes: "+tui.Reason(err), wm.Error)
			}
			for _, process := range response.GetProcesses() {
				if process.GetId() == id || strings.HasPrefix(process.GetId(), id) {
					return p.page(process)
				}
			}
			return wm.Notify(id+" is no longer a process of this session", wm.Warn)
		}
	})
}

// Kills the session's process of the id, the list read again when one is open
func (p *roster) kill(id string, list *cards.List) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Kill(p.kit.Env.Ctx, &terminalv1.KillRequest{Id: id, SessionId: p.session})
		return func() tea.Cmd {
			refresh := func() tea.Cmd {
				if list == nil {
					return nil
				}
				return p.load(list)
			}
			switch {
			case err != nil:
				return wm.Notify("Could not kill "+id+": "+tui.Reason(err), wm.Error)
			case response.GetStatus() == "killed":
				return tea.Batch(refresh(), wm.Notify("Killed "+id, wm.Success))
			case response.GetStatus() == "already_exited":
				return tea.Batch(refresh(), wm.Notify(id+" had already exited", wm.Info))
			case response.GetStatus() == "not_found":
				return tea.Batch(refresh(), wm.Notify(id+" is no longer a process of this session", wm.Warn))
			}
			return wm.Notify("Could not kill "+id+": "+response.GetError(), wm.Error)
		}
	})
}
