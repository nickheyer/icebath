package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of a server's page: an act, or one of its options
type line struct {
	act     *act
	setting *configv1.Setting
}

// Opens the page of a server over the page of the area: its state as a sentence, then installing it when icebath can, turning it off or
// on, its command, variables and start options, and for one you added its file types, project files, language name, description and
// removing it
func (p *page) page(id string) tea.Cmd {
	t := p.kit.Env.Theme
	var lines []line
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   id,
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen + ":" + id,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if srv := p.server(id); srv != nil {
				return p.stateLines(srv)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			lines = p.lines(id)
			cells := make([][]string, len(lines))
			for i, l := range lines {
				if l.setting != nil {
					cells[i] = p.options.Cells(l.setting)
					continue
				}
				cells[i] = []string{l.act.title, "", ""}
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(lines) {
				return nil
			}
			if l := lines[i]; l.setting != nil {
				return p.options.Detail(l.setting, view.Cut(i, 2))
			}
			return []string{lines[i].act.about}
		},
		Empty: func() string { return "No language server is called " + id + " now." },
		// Back in view, it reads its server again and goes on following it
		OnFocus: func(*cards.List) tea.Cmd { return tea.Batch(p.load(), p.watch()) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(lines) {
				return nil
			}
			if l := lines[i]; l.setting != nil {
				return p.options.Hints(l.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: lines[i].act.verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if lines[i].setting != nil {
					return p.options.Change(lines[i].setting)
				}
				return lines[i].act.run(l)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if lines[i].setting != nil {
					return p.options.Reset(l, lines[i].setting)
				}
				return nil
			},
		},
	})
	view.ToggleDetail()
	p.options.Watch(view)
	p.views.Add(view)
	return p.hub.Push(view)
}

// The state of a server as a sentence, then in the warm colour the folders it was kept out of
func (p *page) stateLines(srv *lspv1.Server) []string {
	t := p.kit.Env.Theme
	var said string
	switch srv.GetState() {
	case lspconfig.Running:
		said = "Running in " + strings.Join(srv.GetRunning(), ", ") + "."
	case lspconfig.Failed:
		said = "Failed in " + strings.Join(srv.GetFailed(), ", ") + ". " + retry(srv)
	case lspconfig.Installed:
		said = "Installed. It runs from " + srv.GetPath() + "."
	case lspconfig.Off:
		said = "Off, so it never starts."
	default:
		said = "Not installed. " + srv.GetInstall()
	}
	lines := []string{said}
	if skipped := srv.GetSkipped(); len(skipped) > 0 {
		lines = append(lines, render.Warm("It was kept out of "+strings.Join(skipped, ", ")+", which is not trusted.", t))
	}
	return lines
}

// Rows of a server's page as it stands now
func (p *page) lines(id string) []line {
	srv := p.server(id)
	if srv == nil {
		return nil
	}
	var out []line
	if srv.GetInstallable() && srv.GetState() == lspconfig.NotInstalled {
		out = append(out, line{act: &act{"Install", "Installs it now, its output shown as it goes.", "install",
			func(*cards.List) tea.Cmd { return p.install(id) }}})
	}
	toggle := &act{"Turn off", "It never starts, and the files it checks are not checked.", "turn off", func(*cards.List) tea.Cmd { return p.turn(srv, false) }}
	if srv.GetState() == lspconfig.Off {
		toggle = &act{"Turn on", "It starts again the next time a file it checks is edited.", "turn on", func(*cards.List) tea.Cmd { return p.turn(srv, true) }}
	}
	out = append(out, line{act: toggle})
	prefix := lspconfig.Servers + "." + id + "."
	fields := []string{lspconfig.Command, lspconfig.Env, lspconfig.Init}
	if srv.GetAdded() {
		fields = append(fields, lspconfig.Extensions, lspconfig.RootMarkers, lspconfig.LanguageID, lspconfig.Description)
	}
	for _, field := range fields {
		if setting := p.options.Setting(prefix + field); setting != nil {
			out = append(out, line{setting: setting})
		}
	}
	if srv.GetAdded() {
		out = append(out, line{act: &act{"Remove", "Removes it from icebath. The program it runs stays on this computer.", "remove",
			func(l *cards.List) tea.Cmd { return p.forget(l, srv) }}})
	}
	return out
}

// Turns a server off or on
func (p *page) turn(srv *lspv1.Server, on bool) tea.Cmd {
	return tui.Call(p.hub, func() (*lspv1.TurnResponse, error) {
		return p.service.Turn(p.kit.Env.Ctx, &lspv1.TurnRequest{Profile: p.profile, Server: srv.GetId(), On: on})
	}, func(*lspv1.TurnResponse) tea.Cmd {
		told := srv.GetId() + " is now off."
		if on {
			told = srv.GetId() + " is now on."
		}
		return tea.Batch(p.hub.Done(told), p.load(), p.options.Load(nil))
	})
}
