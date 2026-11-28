package app

import (
	"errors"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of the page of a place: using it, setting it up, a thing checked, an option, or the row of its advanced options
type placeRow struct {
	use, setup bool
	check      *terminalv1.Check
	setting    *configv1.Setting
	advanced   bool
}

// Opens the page of a place over the first view: its state as a sentence, Use it, Set up, what was checked with what to do about a failed
// check, then its own options
func (p *sandbox) placePage(name string) tea.Cmd {
	place := p.find(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var shown []placeRow
	var view *cards.List
	// A view opened over the page, whose change the checks are read again for once it closes
	opened := false
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title: place.GetTitle(),
		Glyph: t.Glyphs.Terminal,
		OnFocus: func(*cards.List) tea.Cmd {
			if !opened || p.hub.Top() != view {
				return nil
			}
			opened = false
			return p.load(nil)
		},
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.placeState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.placeRows(name)
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = p.placeCells(name, row)
			}
			return cells
		},
		Group: func(i int) string {
			switch row := shown[i]; {
			case row.check != nil:
				return "What was checked"
			case row.setting != nil, row.advanced:
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string { return p.placeDetail(name, shown[i], view.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "This place to run commands is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch row := shown[i]; {
			case row.use:
				return []wm.Hint{{Action: "hub.use", Label: "use"}}
			case row.setup, row.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			case row.check != nil && fixable(row.check):
				return []wm.Hint{{Action: "hub.use", Label: "fix"}}
			case row.setting != nil:
				return p.options.Hints(row.setting)
			}
			return nil
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.use:
					if now := p.find(name); now != nil {
						return p.use(now)
					}
				case row.setup:
					return p.prepare(name)
				case row.check != nil && fixable(row.check):
					if setting := p.options.Setting(row.check.GetSetting()); setting != nil {
						opened = true
						return p.options.Change(setting)
					}
				case row.setting != nil:
					opened = true
					return p.options.Change(row.setting)
				case row.advanced:
					if _, _, open := p.placeOptions(name); open != nil {
						opened = true
						return open()
					}
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(nil) },
		},
		Free: map[string]bool{"hub.refresh": true},
	})
	view.ToggleDetail()
	p.watch(view)
	p.opened = true
	return p.hub.Push(view)
}

// Whether a failed check names the setting that fixes it, which enter on it opens
func fixable(check *terminalv1.Check) bool { return !check.GetOk() && check.GetSetting() != "" }

// First lines of the page of a place: how it stands as a sentence, and that the environment wins while it names another place
func (p *sandbox) placeState(name string) []string {
	place := p.find(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var lines []string
	switch {
	case place.GetReady() && place.GetSelected():
		lines = append(lines, render.Good("Ready. The agent's commands run here.", t))
	case place.GetReady():
		lines = append(lines, render.Good("Ready.", t))
	case place.GetMissing():
		lines = append(lines, render.Bad(place.GetState()+". "+strs.Full(place.GetWhy()), t))
	default:
		lines = append(lines, render.Warm(place.GetState()+". "+strs.Full(place.GetWhy()), t))
	}
	if p.variable != "" && !place.GetSelected() {
		lines = append(lines, render.Warm(p.variable+" names another place, which wins over what you choose here while it is set.", t))
	}
	return lines
}

// Rows of the page of a place
func (p *sandbox) placeRows(name string) []placeRow {
	place := p.find(name)
	if place == nil {
		return nil
	}
	var rows []placeRow
	if !place.GetSelected() {
		rows = append(rows, placeRow{use: true})
	}
	// Setting up a place this computer can't run does nothing but fail, so it is offered once the place can run
	if place.GetPrepares() && !place.GetMissing() {
		rows = append(rows, placeRow{setup: true})
	}
	for _, check := range place.GetChecks() {
		rows = append(rows, placeRow{check: check})
	}
	shown, advanced, _ := p.placeOptions(name)
	for _, setting := range shown {
		rows = append(rows, placeRow{setting: setting})
	}
	if advanced != nil {
		rows = append(rows, placeRow{advanced: true})
	}
	return rows
}

// Options of a place, those of its own section alone, and the cells of the row of its advanced ones with what it opens; the advanced ones
// in place, no row, while they are few
func (p *sandbox) placeOptions(name string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	place := p.find(name)
	if place == nil || place.GetSection() == "" {
		return nil, nil, nil
	}
	everyday, rare := p.options.Own(place.GetSection())
	if cells, open, ok := p.options.OwnAdvanced(place.GetSection()); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

func (p *sandbox) placeCells(name string, row placeRow) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.use:
		return []string{"Use it", "", ""}
	case row.setup:
		return []string{"Set up", opens, ""}
	case row.check != nil:
		state := render.Good(row.check.GetState(), t)
		if !row.check.GetOk() {
			state = render.Warm(row.check.GetState(), t)
		}
		return []string{row.check.GetLabel(), "", state}
	case row.setting != nil:
		return p.options.Cells(row.setting)
	}
	_, advanced, _ := p.placeOptions(name)
	return advanced
}

// Lines under the rows of the page of a place about the row in hand: what using it and setting it up do, what was found of a check and what
// to do about a failed one, what an option does
func (p *sandbox) placeDetail(name string, row placeRow, cut bool) []string {
	place := p.find(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	switch {
	case row.use:
		return []string{"Run the agent's commands on " + place.GetTitle() + " from the next one on."}
	case row.setup:
		return []string{place.GetReadies(), render.Dim("What it does shows on a page of its own as it goes.", t)}
	case row.check != nil:
		var lines []string
		// What the first line of the page says already is not said again
		if detail := row.check.GetDetail(); detail != "" && detail != place.GetWhy() {
			lines = append(lines, detail)
		}
		if fix := row.check.GetFix(); fix != "" {
			lines = append(lines, render.Warm(fix, t))
		}
		return lines
	case row.setting != nil:
		return p.options.Detail(row.setting, cut)
	}
	return []string{"Settings of " + place.GetTitle() + " that few people change."}
}

// Gets the place ready on a page that shows how it goes, carried on to its end when the page is closed; how it ended is the last line of
// the page while it shows, and the note of the view under it once it closes. The page is read again once it ends
func (p *sandbox) prepare(name string) tea.Cmd {
	place := p.find(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	title := place.GetTitle()
	var lines []string
	out := ended{}
	view := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  "Set up",
		Key:    Screen + ":setup:" + name,
		Follow: true,
		Lines: func(int) []string {
			if len(lines) == 0 && !out.done {
				return []string{render.Dim("Starting to set up "+title+"…", t)}
			}
			return lines
		},
		Closed: func(*cards.Page) tea.Cmd { return out.close(p.hub) },
	})
	finish := func(text string, failed bool) {
		p.kit.Apply(func() tea.Cmd {
			if out.done {
				return nil
			}
			if failed {
				lines = append(lines, "", render.Bad(text, t))
			} else {
				lines = append(lines, "", render.Good(text, t))
			}
			return tea.Batch(p.load(nil), out.end(p.hub, text, failed))
		})
	}
	failed := func(reason string) { finish(title+" is not set up. "+strs.Full(reason), true) }
	ctx := p.kit.Env.Ctx
	go func() {
		stream, err := p.service.Prepare(ctx, &terminalv1.PrepareRequest{Backend: name, Profile: p.profile})
		if err != nil {
			failed(tui.Reason(err))
			return
		}
		defer stream.Close()
		for {
			response, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish(title+" is set up.", false)
				return
			case err != nil:
				if ctx.Err() == nil {
					failed(tui.Reason(err))
				}
				return
			case response.GetDone() && response.GetError() != "":
				failed(response.GetError())
				return
			case response.GetDone():
				finish(title+" is set up.", false)
				return
			}
			p.kit.Apply(func() tea.Cmd {
				lines = append(lines, strings.Split(strings.TrimRight(response.GetLine(), "\n"), "\n")...)
				return nil
			})
		}
	}()
	return p.hub.Push(view)
}

// How the work of a page that follows its output ended, said as the note of the view under it once the page is no longer shown
type ended struct {
	done, closed, failed bool
	note                 string
}

// The work ended: said at once when its page is gone, else kept for when it closes
func (e *ended) end(hub *cards.Hub, note string, failed bool) tea.Cmd {
	e.done, e.note, e.failed = true, note, failed
	if e.closed {
		return e.say(hub)
	}
	return nil
}

// The page closed: says how the work ended when it did
func (e *ended) close(hub *cards.Hub) tea.Cmd {
	e.closed = true
	if e.done {
		return e.say(hub)
	}
	return nil
}

func (e *ended) say(hub *cards.Hub) tea.Cmd {
	if e.failed {
		return hub.Fail(e.note)
	}
	return hub.Done(e.note)
}
