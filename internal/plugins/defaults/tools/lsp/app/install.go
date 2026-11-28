package app

import (
	"errors"
	"io"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// An install running on a page of the hub: its output as it comes, then what it came to
type installing struct {
	p       *page
	title   string
	request *lspv1.InstallRequest
	view    *cards.Page
	lines   []string
	partial string
	outcome *lspv1.Outcome
	ended   bool
}

// Installs one server on a page that follows its output
func (p *page) install(id string) tea.Cmd {
	return p.run("Install "+id, &lspv1.InstallRequest{Server: id})
}

// Asks before each server icebath can install is downloaded, then installs them on a page that follows the output and ends with what was
// installed and what the others need; with none to download it goes straight to what the others need
func (p *page) installAll(l *cards.List) tea.Cmd {
	n := 0
	for _, srv := range p.status.GetServers() {
		if srv.GetInstallable() {
			n++
		}
	}
	run := func() tea.Cmd { return p.run("Install all servers", &lspv1.InstallRequest{All: true}) }
	if n == 0 {
		return run()
	}
	l.Confirm(cards.Question{Text: "icebath installs " + render.Count(n, "language server") + " it can install by itself. They download into the " +
		"profile's folder, which can take a few minutes.", Verb: "Install"}, run)
	return nil
}

// Runs an install on a page pushed on the hub, carried on to its end when the page is closed: the page then gives way to what the install
// came to, or once it is closed the note says it
func (p *page) run(title string, request *lspv1.InstallRequest) tea.Cmd {
	request.Profile = p.profile
	in := &installing{p: p, title: title, request: request}
	in.view = cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  title,
		Key:    Screen + ":install:" + request.GetServer(),
		Follow: true,
		Lines:  func(int) []string { return in.shown() },
	})
	go in.stream()
	return p.hub.Push(in.view)
}

// Lines of the page: the output so far
func (in *installing) shown() []string {
	var out []string
	for _, line := range append(slices.Clone(in.lines), in.partial) {
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return []string{render.Dim("Starting…", in.p.kit.Env.Theme)}
	}
	return out
}

// Reads the install's events until it ends, the page and the servers kept up to date
func (in *installing) stream() {
	p := in.p
	ctx := p.kit.Env.Ctx
	stream, err := p.service.Install(ctx, in.request)
	if err != nil {
		in.failed("Couldn't install. " + strs.Full(tui.Reason(err)))
		return
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		switch {
		case errors.Is(err, io.EOF):
			in.failed("The install stopped without saying whether it worked.")
			return
		case err != nil:
			if ctx.Err() == nil {
				in.failed("Couldn't install. " + strs.Full(tui.Reason(err)))
			}
			return
		}
		switch kind := event.GetKind().(type) {
		case *lspv1.InstallEvent_ExitCode:
			in.finish()
			return
		case *lspv1.InstallEvent_Outcome:
			p.kit.Apply(func() tea.Cmd {
				in.outcome = kind.Outcome
				return nil
			})
		case *lspv1.InstallEvent_Out:
			p.kit.Apply(func() tea.Cmd {
				parts := strings.Split(in.partial+kind.Out, "\n")
				in.lines, in.partial = append(in.lines, parts[:len(parts)-1]...), parts[len(parts)-1]
				return nil
			})
		case *lspv1.InstallEvent_Err:
			p.kit.Apply(func() tea.Cmd {
				in.lines = append(in.lines, render.Warm(kind.Err, p.kit.Env.Theme))
				return nil
			})
		}
	}
}

// Ends an install that could not say what it came to, the note saying why, and reads the servers again
func (in *installing) failed(text string) {
	p := in.p
	p.kit.Apply(func() tea.Cmd {
		if in.ended {
			return nil
		}
		in.ended = true
		return tea.Batch(p.hub.Fail(text), p.load())
	})
}

// Ends the install with what it came to: in place of its page while the page shows, else in the note; and reads the servers again
func (in *installing) finish() {
	p := in.p
	p.kit.Apply(func() tea.Cmd {
		if in.ended {
			return nil
		}
		in.ended = true
		switch {
		case in.outcome == nil:
			return tea.Batch(p.hub.Fail("The install stopped without saying what it came to."), p.load())
		case p.hub.Holds(in.view) && p.hub.Top() == in.view:
			return tea.Batch(p.hub.Swap(in.result()), p.load())
		}
		return tea.Batch(in.note(), p.load())
	})
}

// Note of what the install came to once its page was closed before it ended: the servers installed, and for one server what it needs, for
// many how many could not be installed
func (in *installing) note() tea.Cmd {
	p, told := in.p, in.outcome
	needs := told.GetNeeds()
	held := 0
	for _, need := range needs {
		held += len(need.GetServers())
	}
	switch {
	case held == 0:
		return p.hub.Done(told.GetDone())
	// A server a person installs themselves did not fail: the note says how
	case !in.request.GetAll() && needs[0].GetByHand():
		return p.hub.Say(needs[0].GetText(), cards.NoteInfo)
	case !in.request.GetAll():
		return p.hub.Fail(needs[0].GetText())
	case told.GetDone() == "":
		return p.hub.Fail("No language server could be installed.")
	}
	return p.hub.Done(told.GetDone() + " " + render.Count(held, "other server") + " could not be installed.")
}

// What the install came to, as the server groups it: the servers installed by name, then each thing the others need, once, with the servers
// it holds back; under them, as rows, the settings that give one of those things another way
func (in *installing) result() *cards.List {
	p, told := in.p, in.outcome
	t := p.kit.Env.Theme
	var keys []string
	for _, need := range told.GetNeeds() {
		if key := need.GetSetting(); key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	// The settings as last read, each paint
	var fixes []*configv1.Setting
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   in.title,
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen + ":installed:" + in.request.GetServer(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			var lines []string
			if done := told.GetDone(); done != "" {
				lines = append(lines, render.Good(done, t))
			}
			// Where each server now installed runs from
			for _, done := range told.GetInstalled() {
				if !done.GetAlready() && done.GetPath() != "" {
					lines = append(lines, render.Dim(done.GetServer()+" runs from "+done.GetPath()+".", t))
				}
			}
			for _, need := range told.GetNeeds() {
				if need.GetByHand() {
					lines = append(lines, need.GetText())
					continue
				}
				lines = append(lines, render.Warm(need.GetText(), t))
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			fixes = nil
			for _, key := range keys {
				if setting := p.options.Setting(key); setting != nil {
					fixes = append(fixes, setting)
				}
			}
			cells := make([][]string, len(fixes))
			for i, setting := range fixes {
				cells[i] = p.options.Cells(setting)
			}
			return cells
		},
		Detail: func(i int) []string { return p.options.Detail(fixes[i], list.Cut(i, 2)) },
		Empty:  func() string { return "" },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(fixes) {
				return nil
			}
			return p.options.Hints(fixes[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":    func(_ *cards.List, i int) tea.Cmd { return p.options.Change(fixes[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd { return p.options.Reset(l, fixes[i]) },
		},
	})
	if len(keys) > 0 {
		list.ToggleDetail()
	}
	p.options.Watch(list)
	return list
}
