package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What a row of the page of a store does
type act int

const (
	none act = iota
	turn
	setup
	variables
	fetch
	replace
	forget
	part
)

// Row of the page of a store: something it does, a part of its own, an option, or the row of its advanced options
type storeRow struct {
	act act
	// Section of the part the row opens
	part     string
	setting  *configv1.Setting
	advanced bool
}

// Opens the page of a store over the first view: how it stands as a sentence, turning it on or off, setting it up, its variables, a fetch
// now and its token, then its options and the parts of its own
func (p *page) storePage(name string) tea.Cmd {
	source := p.find(name)
	if source == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var shown []storeRow
	var view *cards.List
	opened := false
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		OnFocus: func(*cards.List) tea.Cmd {
			if !opened || p.hub.Top() != view {
				return nil
			}
			opened = false
			return p.load(nil)
		},
		Title:   source.GetLabel(),
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.storeState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.storeRows(name)
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.storeCells(name, r)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].setting != nil || shown[i].advanced || shown[i].act == part {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string { return p.storeDetail(name, shown[i], view.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, source.GetLabel()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return p.storeHints(name, shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				now := p.find(name)
				if now == nil {
					return nil
				}
				r := shown[i]
				switch r.act {
				case turn:
					return p.change(func() (*secretsv1.ChangeResponse, error) {
						return p.service.Switch(p.kit.Env.Ctx, &secretsv1.SwitchRequest{Profile: p.profile, Name: name, On: !now.GetEnabled()})
					}, nil)
				case setup:
					return p.own(name).Setup(p.steps(), now)
				case variables:
					return p.variablesPage(name)
				case fetch:
					return p.fetchNow(now)
				case replace:
					return p.own(name).Token(p.steps(), now)
				case forget:
					return p.forget(l, now)
				case part:
					return p.partPage(name, r.part)
				}
				switch {
				case r.setting != nil:
					opened = true
					return p.options.Change(r.setting)
				case r.advanced:
					if _, _, open := p.storeOptions(name); open != nil {
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
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

// First line of the page of a store: whether it is on, how many variables it fills, and what keeps it from working
func (p *page) storeState(name string) []string {
	source := p.find(name)
	if source == nil {
		return nil
	}
	t := p.kit.Env.Theme
	lacks := strs.Full(source.GetLacks())
	switch {
	case !source.GetEnabled():
		return []string{strings.TrimSpace("Off. " + lacks)}
	case lacks != "":
		return []string{render.Warm("On. "+lacks, t)}
	case source.GetFills() == 0:
		return []string{render.Good("On.", t) + " It fills no variable yet."}
	}
	return []string{render.Good("On.", t) + " It fills " + strs.Count(int(source.GetFills()), "variable") + "."}
}

// Rows of the page of a store as it stands now
func (p *page) storeRows(name string) []storeRow {
	source := p.find(name)
	if source == nil {
		return nil
	}
	own := p.own(name)
	rows := []storeRow{{act: turn}}
	if own.Setup != nil {
		rows = append(rows, storeRow{act: setup})
	}
	if source.GetShape() == "mapped" {
		rows = append(rows, storeRow{act: variables})
	}
	// A fetch is of use while nothing it lacks keeps it from working, a store whose last fetch failed fetching again
	if source.GetEnabled() && (source.GetNeeds() == "" || source.GetNeeds() == "Failed") {
		rows = append(rows, storeRow{act: fetch})
	}
	if source.GetTokenSet() {
		if own.Token != nil {
			rows = append(rows, storeRow{act: replace})
		}
		rows = append(rows, storeRow{act: forget})
	}
	shown, advanced, _ := p.storeOptions(name)
	for _, setting := range shown {
		rows = append(rows, storeRow{setting: setting})
	}
	for _, each := range own.Parts {
		if p.options.Info(each.Section) != nil {
			rows = append(rows, storeRow{act: part, part: each.Section})
		}
	}
	if advanced != nil {
		rows = append(rows, storeRow{advanced: true})
	}
	return rows
}

// Options of a store: the settings of its own section alone, and the cells of the row of the advanced ones with what it opens; the advanced
// ones in place while they are few
func (p *page) storeOptions(name string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	source := p.find(name)
	if source == nil {
		return nil, nil, nil
	}
	everyday, rare := p.options.Own(source.GetSection())
	if cells, open, ok := p.options.OwnAdvanced(source.GetSection()); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

func (p *page) storeCells(name string, r storeRow) []string {
	source := p.find(name)
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch r.act {
	case turn:
		if source.GetEnabled() {
			return []string{"Turn off", "", ""}
		}
		return []string{"Turn on", "", ""}
	case setup:
		return []string{"Set up", opens, ""}
	case variables:
		if len(source.GetBindings()) == 0 {
			return []string{"Variables", opens, "None"}
		}
		return []string{"Variables", opens, strs.Count(len(source.GetBindings()), "variable")}
	case fetch:
		return []string{"Fetch now", "", ""}
	case replace:
		return []string{"Replace the token", "", ""}
	case forget:
		return []string{"Remove the token", "", ""}
	case part:
		info := p.options.Info(r.part)
		everyday, rare := p.options.Own(r.part)
		return []string{info.GetTitle(), opens, render.Dim(strs.Count(len(everyday)+len(rare), "setting"), t)}
	}
	if r.setting != nil {
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.storeOptions(name)
	return advanced
}

func (p *page) storeHints(name string, r storeRow) []wm.Hint {
	source := p.find(name)
	switch r.act {
	case turn:
		if source.GetEnabled() {
			return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
		}
		return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
	case setup, variables, part:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	case fetch:
		return []wm.Hint{{Action: "hub.use", Label: "fetch"}}
	case replace:
		return []wm.Hint{{Action: "hub.use", Label: "replace"}}
	case forget:
		return []wm.Hint{{Action: "hub.use", Label: "remove"}}
	}
	if r.setting != nil {
		return p.options.Hints(r.setting)
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}}
}

// Lines under the rows of the page of a store about the row in hand
func (p *page) storeDetail(name string, r storeRow, cut bool) []string {
	source := p.find(name)
	if source == nil {
		return nil
	}
	label := source.GetLabel()
	switch r.act {
	case turn:
		switch {
		case source.GetEnabled() && source.GetTokenSet():
			return []string{"Stop filling variables from " + label + ". Its settings and token stay for when you turn it on."}
		case source.GetEnabled():
			return []string{"Stop filling variables from " + label + ". Its settings stay for when you turn it on."}
		}
		return []string{"Fill variables from " + label + " from now on and each time icebath starts."}
	case setup:
		return []string{"Set up how " + label + " signs in, step by step."}
	case variables:
		return []string{"The variables " + label + " fills, each with the reference that holds its value."}
	case fetch:
		return p.fetched(source)
	case replace:
		return []string{"Type a new token, kept in your keys file once " + label + " takes it."}
	case forget:
		return []string{"Take the token of " + label + " out of your keys file, as " + source.GetTokenEnv() + "."}
	case part:
		return []string{strs.Full(p.options.Info(r.part).GetDescription())}
	}
	if r.setting != nil {
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of " + label + " that few people change."}
}

// What the last fetch of a store did, said under the row that fetches it again: the variables it set, those it left and why, and what went
// wrong
func (p *page) fetched(source *secretsv1.Source) []string {
	t := p.kit.Env.Theme
	lines := []string{"Ask " + source.GetLabel() + " for its variables again now."}
	if applied := source.GetApplied(); len(applied) > 0 {
		lines = append(lines, "Its last fetch set "+strs.Count(len(applied), "variable")+": "+strings.Join(applied, ", ")+".")
	}
	if existing := source.GetExisting(); len(existing) > 0 {
		lines = append(lines, render.Dim("Left as you set them: "+strings.Join(existing, ", ")+".", t))
	}
	if claimed := source.GetClaimed(); len(claimed) > 0 {
		lines = append(lines, render.Dim("Set by a store before it in Store order: "+strings.Join(claimed, ", ")+".", t))
	}
	if protected := source.GetProtected(); len(protected) > 0 {
		lines = append(lines, render.Dim("Left alone because they hold the token of a store: "+strings.Join(protected, ", ")+".", t))
	}
	if invalid := source.GetInvalid(); len(invalid) > 0 {
		lines = append(lines, render.Dim("Not set because they are not variable names: "+strings.Join(invalid, ", ")+".", t))
	}
	if failure := source.GetError(); failure != "" && source.GetNeeds() == "" {
		lines = append(lines, render.Warm("Its last fetch failed. "+strs.Full(failure), t))
	}
	for _, warning := range source.GetWarnings() {
		lines = append(lines, render.Warm(strs.Full(warning), t))
	}
	return lines
}

// Fetches every store again, saying what this store set
func (p *page) fetchNow(source *secretsv1.Source) tea.Cmd {
	name, label := source.GetName(), source.GetLabel()
	p.hub.Busy("Fetching from " + label + "…")
	return tui.Call(p.hub, func() (*secretsv1.SourcesResponse, error) {
		return p.service.Reload(p.kit.Env.Ctx, &secretsv1.ReloadRequest{Profile: p.profile})
	}, func(response *secretsv1.SourcesResponse) tea.Cmd {
		p.state = response
		p.refresh()
		now := p.find(name)
		switch {
		case now == nil:
			return nil
		case now.GetError() != "":
			return p.hub.Fail(label + " could not be read. " + strs.Full(now.GetError()))
		}
		return p.hub.Done(label + " set " + strs.Count(len(now.GetApplied()), "variable") + ".")
	})
}

// Asks before the token of a store leaves the keys file, then takes it out
func (p *page) forget(l *cards.List, source *secretsv1.Source) tea.Cmd {
	name, label := source.GetName(), source.GetLabel()
	question := cards.Question{Text: "Remove the token of " + label + " from your keys file? " + label + " can't sign in with it again until you " +
		"set it up again.", Verb: "Remove", Deletes: true}
	l.Confirm(question, func() tea.Cmd {
		return p.change(func() (*secretsv1.ChangeResponse, error) {
			return p.service.Forget(p.kit.Env.Ctx, &secretsv1.ForgetRequest{Profile: p.profile, Name: name})
		}, nil)
	})
	return nil
}

// Opens the options of a part of a store over its page, such as the offline copy of Bitwarden
func (p *page) partPage(name, section string) tea.Cmd {
	info := p.options.Info(section)
	if info == nil {
		return nil
	}
	var shown []*configv1.Setting
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   info.GetTitle(),
		Glyph:   p.kit.Env.Theme.Glyphs.Settings,
		Key:     Screen + ":" + name + ":" + section,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{strs.Full(info.GetDescription())} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			everyday, rare := p.options.Own(section)
			shown = append(everyday, rare...)
			cells := make([][]string, len(shown))
			for i, setting := range shown {
				cells[i] = p.options.Cells(setting)
			}
			return cells
		},
		Detail: func(i int) []string { return p.options.Detail(shown[i], view.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "No settings to show.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return p.options.Hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":    func(_ *cards.List, i int) tea.Cmd { return p.options.Change(shown[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd { return p.options.Reset(l, shown[i]) },
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}
