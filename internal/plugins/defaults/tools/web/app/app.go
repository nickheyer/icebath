// Page of Web search in the settings menu: Automatic and the services under how they stand, what each is used for, the page of each with
// its plan and its key or address, the options of web search and the sites it never opens
package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	webv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1/webv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Web search, the page of its area
const Screen = "web"

// What the automatic choice does
const automaticAbout = "The first service you have an API key for, else a free one."

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := webv1connect.NewWebServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupAbilities,
		Order: 60,
		Title: "Web search",
		Short: "Choose which service searches and reads the web",
		Slash: []string{"web"},
		Usage: "[@profile] [service or words to search]",
		Area:  webconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return servicePages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			return (&page{kit: kit, service: service, settings: settings, profile: name}).open(target)
		},
	}}
}

// The page of Web search for one profile, and what every page inside it reads
type page struct {
	kit      *tui.Kit
	service  webv1connect.WebServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	state   *webv1.ListResponse
	loaded  bool
	err     error
	// A view was opened over the first view, which reads again once it is back on top
	opened bool
}

// Row of the first view: Automatic, a service, an option, the row of the blocked sites, or the row of the advanced options
type row struct {
	auto     bool
	service  *webv1.Backend
	setting  *configv1.Setting
	blocked  bool
	advanced bool
}

func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Web search",
		Glyph:   t.Glyphs.Fetch,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group: func(i int) string {
			switch r := shown[i]; {
			case r.auto:
				return ""
			case r.service != nil && r.service.GetUsable():
				return "Ready to use"
			case r.service != nil:
				return "Needs setting up"
			}
			return "Options"
		},
		Words: func(i int) string {
			if s := shown[i].service; s != nil {
				return s.GetName() + " " + s.GetDescription()
			}
			return ""
		},
		Detail: func(i int) []string { return p.detail(shown[i], p.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.refresh": true},
		// Read again as a view over it closes, which may have changed how the services stand, such as Free services
		OnFocus: func(*cards.List) tea.Cmd {
			if !p.opened || p.hub.Depth() > 0 {
				return nil
			}
			p.opened = false
			return p.load(nil)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return p.hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.auto:
					if p.automatic() {
						return nil
					}
					return p.choose(&webv1.ChooseRequest{})
				case r.service != nil && r.service.GetUsable():
					return p.useFor(r.service.GetName())
				case r.service != nil:
					return p.servicePage(r.service.GetName())
				case r.setting != nil:
					p.opened = true
					return p.options.Change(r.setting)
				case r.blocked:
					return p.blockedPage()
				}
				if _, _, open := p.webOptions(); open != nil {
					p.opened = true
					return open()
				}
				return nil
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if s := shown[i].service; s != nil {
					return p.servicePage(s.GetName())
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
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// What Web search is, then while no service can answer, that the agent can't search
func (p *page) intro() []string {
	lines := []string{"Which service searches the web and reads pages."}
	if p.loaded && p.err == nil && !p.state.GetAvailable() {
		lines = append(lines, render.Warm("The agent can't search the web until a service is ready. Pick one below.", p.kit.Env.Theme))
	}
	return lines
}

// Rows of the first view
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	rows := []row{{auto: true}}
	services := slices.Clone(p.state.GetBackends())
	slices.SortStableFunc(services, func(a, b *webv1.Backend) int { return rank(a) - rank(b) })
	for _, s := range services {
		rows = append(rows, row{service: s})
	}
	shown, advanced, _ := p.webOptions()
	for _, setting := range shown {
		rows = append(rows, row{setting: setting})
	}
	if p.options.Setting(webconfig.BlocklistEnabled) != nil {
		rows = append(rows, row{blocked: true})
	}
	if advanced != nil {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Place of a service among the services: those in use, then those ready, then those that need setting up
func rank(s *webv1.Backend) int {
	switch {
	case s.GetSearching() || s.GetReading():
		return 0
	case s.GetUsable():
		return 1
	}
	return 2
}

// Options of web search, and the cells of the row of the advanced ones with what it opens; the advanced ones in place, no row, while they
// are few
func (p *page) webOptions() (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Section(webconfig.Key)
	if cells, open, ok := p.options.Advanced(webconfig.Key); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

// Whether Automatic is in force: no service chosen for both and none pinned for one of them
func (p *page) automatic() bool {
	return p.state.GetBackend() == "" && p.state.GetSearchBackend() == "" && p.state.GetExtractBackend() == ""
}

// Whether the service is chosen, for both or for one of them
func (p *page) chosen(s *webv1.Backend) bool {
	name := s.GetName()
	return p.state.GetBackend() == name || p.state.GetSearchBackend() == name || p.state.GetExtractBackend() == name
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.auto:
		value := ""
		if p.automatic() {
			value = render.Good("In use", t)
		}
		return []string{"Automatic", p.kit.Mark(true, p.automatic()), value}
	case r.service != nil:
		return []string{r.service.GetTitle(), p.kit.Mark(true, p.chosen(r.service)), stateWords(r.service, t)}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.blocked:
		return []string{"Blocked sites", opens, p.options.Cells(p.options.Setting(webconfig.BlocklistEnabled))[2]}
	}
	_, advanced, _ := p.webOptions()
	return advanced
}

// State of a service in the words the server gives, in the color of how it stands
func stateWords(s *webv1.Backend, t theme.Theme) string {
	switch {
	case s.GetSearching() || s.GetReading():
		return render.Good(s.GetState(), t)
	case s.GetUsable():
		return s.GetState()
	}
	return render.Warm(s.GetState(), t)
}

// Lines under the rows about the row in hand: what Automatic does and what answers now, what a service is and what it is used for
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.auto:
		lines := []string{automaticAbout}
		if serving := p.state.GetServing(); serving != "" {
			lines = append(lines, render.Dim(serving, t))
		}
		return lines
	case r.service != nil:
		lines := []string{r.service.GetDescription() + "."}
		if said := p.usedFor(r.service); said != "" {
			lines = append(lines, render.Good(said, t))
		}
		if r.service.GetExplicit() {
			lines = append(lines, render.Dim("Used only when you choose it, because its sign-in serves other features too.", t))
		}
		return lines
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	case r.blocked:
		return []string{"Sites that web tools and the browser never open."}
	}
	return []string{"Settings of web search that few people change."}
}

// What the service is used for now, a sentence; empty while it is not in use
func (p *page) usedFor(s *webv1.Backend) string {
	switch {
	case s.GetRotating():
		return "It takes turns with the other free services, so your searches spread over them."
	case s.GetSearching() && s.GetReading():
		return "It answers the agent's searches and reads pages for it."
	case s.GetSearching():
		return "It answers the agent's searches."
	case s.GetReading():
		return "It reads pages for the agent."
	}
	return ""
}

func (p *page) hints(r row) []wm.Hint {
	switch {
	case r.auto && p.automatic():
		return nil
	case r.auto:
		return []wm.Hint{{Action: "hub.use", Label: "use"}}
	case r.service != nil && r.service.GetUsable():
		return []wm.Hint{{Action: "hub.use", Label: "use for"}, {Action: "hub.edit", Label: "open"}}
	case r.service != nil, r.blocked, r.advanced:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	}
	return p.options.Hints(r.setting)
}

// Goes where the page was opened at: the page of a service named by its name or title, else the services searched for the words
func (p *page) reach(target string) tea.Cmd {
	if target == "" {
		return nil
	}
	if strings.EqualFold(target, BlockedOf) {
		return p.blockedPage()
	}
	for _, s := range p.state.GetBackends() {
		if strings.EqualFold(s.GetName(), target) || strings.EqualFold(s.GetTitle(), target) || strings.EqualFold(lastPart(s.GetSection()), target) {
			return p.servicePage(s.GetName())
		}
	}
	p.root.Filter(target)
	return nil
}

// Last part of a key
func lastPart(key string) string {
	return key[strings.LastIndex(key, ".")+1:]
}

// Reads the services, then the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &webv1.ListRequest{Profile: p.profile})
		return func() tea.Cmd {
			// What was read of the services shows whether or not the options can be read after it
			p.loaded, p.err = true, err
			if err == nil {
				p.state = response
			}
			p.refresh()
			return p.options.Load(func() tea.Cmd {
				p.refresh()
				if then == nil || err != nil {
					return nil
				}
				return then()
			})
		}
	})
}

// Draws every list of the page again
func (p *page) refresh() {
	p.views.Refresh()
}

// Keeps a list among those drawn again on every read
func (p *page) watch(view *cards.List) {
	p.views.Add(view)
	p.options.Watch(view)
}

// Service of the name as last read, nil when it is gone
func (p *page) find(name string) *webv1.Backend {
	for _, s := range p.state.GetBackends() {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// Chooses what a service is used for and its plan, or Automatic, saying what is in use for what now, and tells the screens that follow the
// settings of the profile in use
func (p *page) choose(request *webv1.ChooseRequest) tea.Cmd {
	request.Profile = p.profile
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*webv1.ChooseResponse, error) {
		return p.service.Choose(p.kit.Env.Ctx, request)
	}, func(response *webv1.ChooseResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(response.GetNote()), p.load(p.changed))
	})
}

// Tells the screens that follow the settings of web search what they hold now, for the profile in use
func (p *page) changed() tea.Cmd {
	if p.profile != "" {
		return nil
	}
	var cmds []tea.Cmd
	for _, key := range []string{webconfig.Backend, webconfig.SearchBackend, webconfig.ExtractBackend} {
		if setting := p.options.Setting(key); setting != nil {
			cmds = append(cmds, p.kit.Changed(key, setting.GetValue()))
		}
	}
	return tea.Batch(cmds...)
}

// List of option rows over the hub, each drawn, explained and changed as Settings does
func (p *page) optionList(title, key, about string, settings func() []*configv1.Setting) tea.Cmd {
	var shown []*configv1.Setting
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   p.kit.Env.Theme.Glyphs.Settings,
		Key:     key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{about} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = settings()
			cells := make([][]string, len(shown))
			for i, setting := range shown {
				cells[i] = p.options.Cells(setting)
			}
			return cells
		},
		Detail: func(i int) []string { return p.options.Detail(shown[i], list.Cut(i, 2)) },
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
	list.ToggleDetail()
	p.watch(list)
	return p.hub.Push(list)
}

// The page of the sites web tools and the browser never open: its switch, the sites, asked one by one as Site, and the list files
func (p *page) blockedPage() tea.Cmd {
	return p.optionList("Blocked sites", Screen+":blocked", "Sites that web tools and the browser never open.", func() []*configv1.Setting {
		everyday, rare := p.options.Own(webconfig.Blocklist)
		return append(everyday, rare...)
	})
}
