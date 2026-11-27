package app

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the processes list
const processesContext = "tui.processes"

var actions = []keys.Action{
	{ID: "tui.processes.read", Context: processesContext, Label: "read", Doc: "page the output of the background process", Default: []string{"enter"}},
	{ID: "tui.processes.kill", Context: processesContext, Label: "kill", Doc: "kill the background process", Default: []string{"x"}},
}

// Processes that ended stay in the list this long
const retain = 60

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	processes := terminalv1connect.NewProcessServiceClient(conn)
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	return []tui.Screen{{
		ID:      "processes",
		Title:   "Background processes",
		Short:   "The processes the agent's commands left running: read their output or kill them",
		Actions: actions,
		Where:   map[string]string{processesContext: "background processes"},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			return (&roster{kit: kit, processes: processes, sessions: sessions}).open()
		},
	}, backendsScreen(backendsConn{backends: terminalv1connect.NewBackendServiceClient(conn), config: configv1connect.NewConfigServiceClient(conn)})}
}

type roster struct {
	kit       *tui.Kit
	processes terminalv1connect.ProcessServiceClient
	sessions  sessionsv1connect.SessionServiceClient
	hub       *cards.Hub
	list      *cards.List
	rows      []*terminalv1.Process
	titles    map[string]string
	loaded    bool
	err       error
}

func (r *roster) open() tea.Cmd {
	r.list = cards.NewList(r.kit.Env, cards.ListSpec{
		Title:   "Processes",
		Glyph:   r.kit.Env.Theme.Glyphs.Jobs,
		Key:     "tui:processes",
		Context: processesContext,
		Primary: "tui.processes.read",
		Columns: []render.Column{{Title: "ID"}, {Title: "Session"}, {Title: "State"}, {Title: "Command", Flex: 3}},
		Yield:   []int{1},
		Rows: func() [][]string {
			rows := make([][]string, len(r.rows))
			for i, p := range r.rows {
				rows[i] = []string{p.GetId(), r.session(p), r.state(p), collapse(p.GetCommand())}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(r.rows) {
				return nil
			}
			return []string{verdict(r.rows[i])}
		},
		Empty:   func() string { return tui.Waiting(r.loaded, r.err, "No background processes.") },
		OnFocus: func(*cards.List) tea.Cmd { return r.load() },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.processes.read": func(_ *cards.List, i int) tea.Cmd { return r.read(r.rows[i]) },
			"tui.processes.kill": func(_ *cards.List, i int) tea.Cmd { return r.kill(r.rows[i].GetId()) },
		},
	})
	r.list.ToggleDetail()
	r.hub = r.kit.Hub("processes", r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load())
}

// Reads the processes and the titles of their sessions, running ones first, those that ended a while ago left out
func (r *roster) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.processes.List(ctx, &terminalv1.ListRequest{})
		titles := map[string]string{}
		if listed, err := r.sessions.List(ctx, &sessionsv1.ListRequest{}); err == nil {
			for _, session := range listed.GetSessions() {
				titles[session.GetId()] = session.GetTitle()
			}
		}
		return func() tea.Cmd {
			r.err, r.loaded, r.titles = err, true, titles
			r.rows = slices.DeleteFunc(response.GetProcesses(), func(p *terminalv1.Process) bool {
				return p.GetStatus() != "running" && p.GetSinceExitSeconds() > retain
			})
			slices.SortStableFunc(r.rows, func(a, b *terminalv1.Process) int {
				if a.GetStatus() != b.GetStatus() && (a.GetStatus() == "running" || b.GetStatus() == "running") {
					if a.GetStatus() == "running" {
						return -1
					}
					return 1
				}
				if a.GetStatus() == "running" {
					return cmp.Compare(b.GetUptimeSeconds(), a.GetUptimeSeconds())
				}
				return cmp.Compare(a.GetSinceExitSeconds(), b.GetSinceExitSeconds())
			})
			r.list.Refresh()
			return nil
		}
	})
}

func (r *roster) session(p *terminalv1.Process) string {
	return render.Dim(cmp.Or(r.titles[p.GetSessionId()], p.GetSessionId()), r.kit.Env.Theme)
}

// How the process stands: running, or how it ended
func status(p *terminalv1.Process) string {
	if p.GetStatus() == "running" {
		return "running"
	}
	switch p.GetCompletionReason() {
	case "failed_start":
		return "failed"
	case "killed", "lost":
		return p.GetCompletionReason()
	}
	if p.GetHasExitCode() && p.GetExitCode() != 0 {
		return "failed"
	}
	return "done"
}

// State of the process in a few words, coloured by how it stands
func (r *roster) state(p *terminalv1.Process) string {
	t := r.kit.Env.Theme
	switch s := status(p); s {
	case "running":
		return render.Warm(fmt.Sprintf("running · %ds", p.GetUptimeSeconds()), t)
	case "killed", "lost":
		return render.Dim(s, t)
	case "failed":
		return render.Bad(exit(p, s), t)
	default:
		return render.Good(exit(p, s), t)
	}
}

func exit(p *terminalv1.Process, s string) string {
	if !p.GetHasExitCode() {
		return s
	}
	return fmt.Sprintf("%s · exit %d", s, p.GetExitCode())
}

// The last output while it runs, otherwise how it ended and how long ago
func verdict(p *terminalv1.Process) string {
	if p.GetStatus() == "running" {
		if line := lastLine(p.GetOutputTail()); line != "" {
			return "last: " + line
		}
		return "starting"
	}
	return fmt.Sprintf("%s · %ds ago", exit(p, status(p)), p.GetSinceExitSeconds())
}

var whitespace = regexp.MustCompile(`\s+`)

func collapse(text string) string {
	return strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
}

// Last line of the output with text on it
func lastLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := collapse(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// Pages the output of the process
func (r *roster) read(p *terminalv1.Process) tea.Cmd {
	if strings.TrimSpace(p.GetOutputTail()) == "" {
		return wm.Notify(p.GetId()+" has written no output yet", wm.Info)
	}
	command := []rune(collapse(p.GetCommand()))
	title := p.GetId() + " · " + string(command[:min(len(command), 40)])
	output := strings.Split(strings.TrimRight(p.GetOutputTail(), "\n"), "\n")
	page := cards.NewPage(r.kit.Env, cards.PageSpec{
		Title:  title,
		Key:    "tui:processes:" + p.GetId(),
		Follow: true,
		Lines: func(w int) []string {
			var lines []string
			for _, line := range output {
				lines = append(lines, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
			}
			return lines
		},
	})
	return r.hub.Push(page)
}

// Kills the process and reads the list again
func (r *roster) kill(id string) tea.Cmd {
	return tui.Call(r.hub, func() (*terminalv1.KillResponse, error) {
		return r.processes.Kill(r.kit.Env.Ctx, &terminalv1.KillRequest{Id: id})
	}, func(response *terminalv1.KillResponse) tea.Cmd {
		switch response.GetStatus() {
		case "killed":
			return tea.Batch(r.load(), wm.Notify("Killed "+id, wm.Success))
		case "already_exited":
			return tea.Batch(r.load(), wm.Notify(id+" had already exited", wm.Info))
		case "not_found":
			return tea.Batch(r.load(), wm.Notify(id+" is no longer a background process", wm.Warn))
		}
		return wm.Notify("Could not kill "+id+": "+response.GetError(), wm.Error)
	})
}
