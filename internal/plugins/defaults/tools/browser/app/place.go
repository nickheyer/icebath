package app

import (
	"cmp"
	"strconv"

	tea "charm.land/bubbletea/v2"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of the page of a service: using it, an option, a key kept elsewhere, or the row of its advanced options
type placeRow struct {
	use      bool
	setting  *configv1.Setting
	key      *configv1.Setting
	advanced bool
}

// Place pages open of the name as last read, nil when it is gone
func (p *page) place(name string) *browserv1.Service {
	for _, place := range p.places.GetServices() {
		if place.GetName() == name {
			return place
		}
	}
	return nil
}

// Opens the page of a service over the first view: how it stands, Use it, its options and the keys it shares
func (p *page) placePage(name string) tea.Cmd {
	place := p.place(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var shown []placeRow
	var view *cards.List
	// A view opened over the page, whose change the service is read again for once it closes
	opened := false
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		OnFocus: func(*cards.List) tea.Cmd {
			if !opened || p.hub.Top() != view {
				return nil
			}
			opened = false
			return p.load(nil)
		},
		Title:   place.GetTitle(),
		Glyph:   t.Glyphs.Fetch,
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.placeState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.placeRows(name)
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.placeCells(name, r)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].use {
				return ""
			}
			return "Options"
		},
		Detail: func(i int) []string {
			switch r := shown[i]; {
			case r.use:
				return []string{"Open the agent's web pages in " + place.GetTitle() + " from now on."}
			case r.setting != nil:
				return p.options.Detail(r.setting, view.Cut(i, 2))
			case r.key != nil:
				return []string{r.key.GetDescription(), render.Dim("The same key serves other features too, so it is kept with them.", t)}
			}
			return []string{"Settings of " + place.GetTitle() + " that few people change."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, place.GetTitle()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.use:
				return []wm.Hint{{Action: "hub.use", Label: "use"}}
			case r.key != nil, r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i].setting)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.use:
					return p.use(name)
				case r.setting != nil:
					opened = true
					return p.options.Change(r.setting)
				case r.key != nil:
					opened = true
					return p.options.Change(r.key)
				}
				if _, _, open := p.placeOptions(name); open != nil {
					opened = true
					return open()
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

// First line of the page of a service: how it stands as a sentence
func (p *page) placeState(name string) []string {
	place := p.place(name)
	if place == nil {
		return nil
	}
	t := p.kit.Env.Theme
	switch {
	// In use with nothing chosen: it is the one icebath comes to by what is set up, which choosing another changes
	case place.GetChosen() && p.places.GetChosen() == "":
		return []string{render.Good("In use while you choose no other. The agent's web pages open in "+place.GetTitle()+".", t)}
	case place.GetChosen():
		return []string{render.Good("In use. The agent's web pages open in "+place.GetTitle()+".", t)}
	case place.GetReady():
		return []string{"Ready."}
	}
	return []string{render.Warm(place.GetState()+". Set it below, then Use it.", t)}
}

// Rows of the page of a service
func (p *page) placeRows(name string) []placeRow {
	place := p.place(name)
	if place == nil {
		return nil
	}
	var rows []placeRow
	// One in use only while no other is chosen can be chosen, so it stays in use whatever else is set up later
	if !place.GetChosen() || p.places.GetChosen() == "" {
		rows = append(rows, placeRow{use: true})
	}
	shown, advanced, _ := p.placeOptions(name)
	for _, setting := range shown {
		rows = append(rows, placeRow{setting: setting})
	}
	for _, key := range place.GetKeys() {
		if setting := p.options.Setting(key); setting != nil {
			rows = append(rows, placeRow{key: setting})
		}
	}
	if advanced != nil {
		rows = append(rows, placeRow{advanced: true})
	}
	return rows
}

// Options of a service, those of its own section alone, and the cells of the row of its advanced ones with what it opens; the advanced
// ones in place, no row, while they are few
func (p *page) placeOptions(name string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	place := p.place(name)
	if place == nil || place.GetSection() == "" {
		return nil, nil, nil
	}
	everyday, rare := p.options.Own(place.GetSection(), place.GetKeys()...)
	if cells, open, ok := p.options.OwnAdvanced(place.GetSection(), place.GetKeys()...); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

func (p *page) placeCells(name string, r placeRow) []string {
	t := p.kit.Env.Theme
	switch {
	case r.use:
		return []string{"Use it", "", ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.key != nil:
		return []string{p.place(name).GetTitle() + " API key", render.Dim(t.Glyphs.Forward, t), p.options.Cells(r.key)[2]}
	}
	_, advanced, _ := p.placeOptions(name)
	return advanced
}

// Opens the agent's pages in the service from now on, as Where pages open does
func (p *page) use(name string) tea.Cmd {
	place := p.place(name)
	if place == nil {
		return nil
	}
	value := strconv.Quote(name)
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
		return p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: browserconfig.CloudProvider, Value: value})
	}, func(*configv1.SetResponse) tea.Cmd {
		var changed tea.Cmd
		if p.profile == "" {
			changed = p.kit.Changed(browserconfig.CloudProvider, value)
		}
		note := "The agent's web pages now open in " + place.GetTitle() + "."
		if !place.GetReady() {
			note = "The agent's web pages now open in " + place.GetTitle() + ", once it has what it needs."
		}
		return tea.Batch(changed, p.hub.Done(note), p.load(nil))
	})
}

// Picker of where pages open: this computer, the cloud services and the browsers holding pages of their own, each with how it stands. One
// that is ready is chosen; one that needs a key opens its page, where the key is set
func pickPlace(kit *tui.Kit, hub *cards.Hub, service browserv1connect.BrowserServiceClient, ask tui.Ask) tea.Cmd {
	hub.Busy("Reading where pages can open…")
	return tui.Call(hub, func() (*browserv1.ServicesResponse, error) {
		return service.Services(kit.Env.Ctx, &browserv1.ServicesRequest{Profile: ask.Profile})
	}, func(response *browserv1.ServicesResponse) tea.Cmd {
		hub.Quiet()
		var choices []tui.Choice
		current := ""
		ready := map[string]bool{}
		for _, place := range response.GetServices() {
			detail := place.GetAbout()
			if !place.GetReady() {
				detail += " · " + place.GetState()
			}
			choices = append(choices, tui.Choice{ID: place.GetName(), Title: place.GetTitle(), Detail: detail, Group: place.GetGroup()})
			ready[place.GetName()] = place.GetReady()
			if place.GetChosen() {
				current = place.GetName()
			}
		}
		intro := ask.Intro
		if len(intro) == 0 {
			intro = []string{"Where the agent's web pages open."}
		}
		return kit.PickFrom(hub, cmp.Or(ask.Title, "Where pages open"), intro, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
			if !ready[choice.ID] {
				return kit.Open(Screen, profile.Join(ask.Profile, choice.ID))
			}
			return ask.Done(map[string]string{cmp.Or(ask.Key, browserconfig.CloudProvider): strconv.Quote(choice.ID)})
		})
	})
}
