package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of the page of a service: using it, its account at a provider, an option, or the row of its advanced options
type serviceRow struct {
	use, account bool
	setting      *configv1.Setting
	advanced     bool
}

// Opens the page of a service over the first view: how it stands as a sentence, using it, its model, its key or account, and its other
// options. The titles of its models are read as it opens, so its model is said by name
func (p *page) servicePage(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	t := p.kit.Env.Theme
	var shown []serviceRow
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
		Title:   s.GetTitle(),
		Glyph:   t.Glyphs.Write,
		Key:     p.kind.id + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.serviceState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.serviceRows(name)
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.serviceCells(name, r)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].setting != nil || shown[i].advanced {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string { return p.serviceDetail(name, shown[i], view.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, s.GetTitle()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.use:
				return []wm.Hint{{Action: "hub.use", Label: "use"}}
			case r.account, r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i].setting)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.use:
					return p.use(name)
				case r.account:
					if now := p.find(name); now != nil {
						return p.openAccount(now)
					}
				case r.setting != nil:
					opened = true
					return p.options.Change(r.setting)
				case r.advanced:
					if _, _, open := p.serviceOptions(name); open != nil {
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
	return tea.Batch(p.hub.Push(view), p.readTitles(name))
}

// Reads the models of the service, so the row of its model says the model by name, then draws the lists again
func (p *page) readTitles(name string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Models(p.kit.Env.Ctx, &generatev1.ModelsRequest{Profile: p.profile, Kind: p.kind.proto, Service: name})
		return func() tea.Cmd {
			if err == nil {
				p.titles.keep(p.kind.models, name, response)
				p.refresh()
			}
			return nil
		}
	})
}

// Opens the provider whose account the service uses in Providers, or adds one of its kind while the profile has none
func (p *page) openAccount(s *generatev1.Service) tea.Cmd {
	if provider := s.GetProvider(); provider != "" {
		return p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+provider))
	}
	return p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.AddOf+s.GetProviderKind()))
}

// First line of the page of a service: how it stands, as a sentence
func (p *page) serviceState(name string) []string {
	s := p.find(name)
	if s == nil {
		return nil
	}
	t := p.kit.Env.Theme
	noun := p.kind.noun
	lacks := "an API key"
	if s.GetKey() == "" {
		lacks = "your " + s.GetProviderTitle() + " account"
	}
	switch {
	case s.GetState() == generatev1.State_STATE_IN_USE:
		return []string{render.Good("In use. It makes "+noun+" for the agent now.", t)}
	// Chosen and ready, yet not making them: the person turned the making of them off, which only videos can be
	case s.GetChosen() && ready(s) && p.kind.id == Videos:
		return []string{"Chosen. It makes " + noun + " once Make videos is on."}
	case s.GetChosen() && ready(s):
		return []string{"Chosen. It makes " + noun + " once making them is on."}
	case s.GetChosen():
		return []string{render.Warm("Chosen, but it needs "+lacks+" before it can make "+noun+".", t)}
	case s.GetState() == generatev1.State_STATE_FREE:
		return []string{"Ready to use, and free."}
	case ready(s):
		return []string{"Ready to use."}
	}
	return []string{render.Warm("Needs "+lacks+" before it can make "+noun+".", t)}
}

// Rows of the page of a service
func (p *page) serviceRows(name string) []serviceRow {
	s := p.find(name)
	if s == nil {
		return nil
	}
	var rows []serviceRow
	if !s.GetChosen() {
		rows = append(rows, serviceRow{use: true})
	}
	if s.GetKey() == "" && s.GetProviderKind() != "" {
		rows = append(rows, serviceRow{account: true})
	}
	shown, advanced, _ := p.serviceOptions(name)
	for _, setting := range shown {
		rows = append(rows, serviceRow{setting: setting})
	}
	if advanced != nil {
		rows = append(rows, serviceRow{advanced: true})
	}
	return rows
}

// Options of a service: its model, its API key, then the other settings of its own section alone, and the cells of the row of the advanced
// ones with what it opens; the advanced ones in place, no row, while they are few
func (p *page) serviceOptions(name string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	s := p.find(name)
	if s == nil {
		return nil, nil, nil
	}
	var leave []string
	for _, key := range []string{s.GetModelSetting(), s.GetKey()} {
		if key == "" {
			continue
		}
		leave = append(leave, key)
		if setting := p.options.Setting(key); setting != nil && !setting.GetInternal() && !slices.Contains(shown, setting) {
			shown = append(shown, setting)
		}
	}
	everyday, rare := p.options.Own(s.GetSection(), leave...)
	shown = append(shown, everyday...)
	behind := func() []*configv1.Setting {
		_, rare := p.options.Own(s.GetSection(), leave...)
		return rare
	}
	if cells, open, ok := p.options.AdvancedOf(p.kind.id+":"+name+":advanced", s.GetTitle(), shown, behind); ok {
		return shown, cells, open
	}
	return append(shown, rare...), nil, nil
}

func (p *page) serviceCells(name string, r serviceRow) []string {
	s := p.find(name)
	t := p.kit.Env.Theme
	switch {
	case r.use:
		return []string{"Use it", "", ""}
	case r.account:
		state := render.Warm("Not connected", t)
		if ready(s) {
			state = "Connected"
		}
		return []string{"Account", render.Dim(t.Glyphs.Forward, t), state}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.serviceOptions(name)
	return advanced
}

// Lines under the rows of the page of a service about the row in hand
func (p *page) serviceDetail(name string, r serviceRow, cut bool) []string {
	s := p.find(name)
	if s == nil {
		return nil
	}
	switch {
	case r.use && s.GetState() == generatev1.State_STATE_IN_USE:
		return []string{"Keep " + s.GetTitle() + " in use, whatever else is ready."}
	case r.use:
		line := "Have " + s.GetTitle() + " make " + p.kind.noun + " for the agent, with the model below."
		if !ready(s) && s.GetKey() != "" {
			line += " It asks for the API key first."
		}
		return []string{line}
	case r.account:
		if s.GetProvider() == "" {
			return []string{"Add " + s.GetProviderTitle() + " to your providers, whose account " + s.GetTitle() + " uses."}
		}
		return []string{"The " + s.GetProviderTitle() + " provider, whose account " + s.GetTitle() + " uses."}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of " + s.GetTitle() + " that few people change."}
}

// Uses the service. One that lacks its API key asks for it first, in a job of one step that keeps the key and the choice only once the key
// is typed
func (p *page) use(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	if ready(s) || s.GetKey() == "" {
		return p.choose(name)
	}
	f := p.kit.Flow(p.hub, "Use "+s.GetTitle())
	return f.Ask("", []cards.Field{keyField(s)}, "Use", func(values map[string]string) tea.Cmd {
		typed := strings.TrimSpace(values["key"])
		p.hub.Busy("Saving…")
		return tui.Call(p.hub, func() (*generatev1.ChooseResponse, error) {
			ctx := p.kit.Env.Ctx
			if _, err := p.settings.Set(ctx, &configv1.SetRequest{Profile: p.profile, Key: s.GetKey(), Value: strconv.Quote(typed)}); err != nil {
				return nil, err
			}
			return p.service.Choose(ctx, &generatev1.ChooseRequest{Profile: p.profile, Kind: p.kind.proto, Service: name})
		}, func(response *generatev1.ChooseResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(response.GetNote()), f.End(), p.load(p.changed))
		})
	})
}

// Field of the API key of a service, its line saying whose key it is and where to get one
func keyField(s *generatev1.Service) cards.Field {
	help := "Your " + s.GetTitle() + " API key."
	if link := s.GetLink(); link != "" {
		help += " Get one at " + link + "."
	}
	return cards.Field{ID: "key", Label: "API key", Kind: cards.FieldSecret, Required: true, Help: help}
}
