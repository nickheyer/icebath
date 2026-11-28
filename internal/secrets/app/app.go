// Page of Secret stores in the settings menu: the stores with how each stands, the page of each with turning it on or off, setting it up, the
// variables it fills and its options, the order they are asked in, and the options every store shares
package app

import (
	"maps"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the secret stores, the page of their area
const Screen = "secrets"

// Arguments of the screen that open a page inside it: the order of the stores, and after a store's name its variables
const (
	OrderOf     = "order"
	VariablesOf = ":variables"
)

// What a store brings to its page beyond what every store has
type Store struct {
	// Name of the secret store it is of
	Source string
	// Steps that set the store up, run as a flow on its page; nil for a store with nothing to set up
	Setup func(p *Page, source *secretsv1.Source) tea.Cmd
	// Replaces the token the store signs in with, checked before it is kept; nil for a store with no token
	Token func(p *Page, source *secretsv1.Source) tea.Cmd
	// Sections of its own under its section, each a row of its page that opens its options
	Parts []Part
	// Pickers of the things of the store that settings name, such as its accounts, by the kind each picks
	Pickers map[string]tui.Picker
}

// A section of a store's own under its section, with the title a search finds its page by
type Part struct {
	Section, Title string
}

type Registrant interface {
	Stores(conn *connect.Client) []Store
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Stores(conn *connect.Client) []Store {
	var stores []Store
	for _, registrant := range r.registrants {
		stores = append(stores, registrant.Stores(conn)...)
	}
	return stores
}

// What the stores bring to the page
type Stores struct {
	Registry
}

func New() *Stores {
	return &Stores{}
}

type screens struct {
	stores *Stores
}

func Register(host *app.App, stores *Stores) {
	host.Register(screens{stores: stores})
}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := secretsv1connect.NewSecretServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	pickers := map[string]tui.Picker{}
	for _, store := range s.stores.Stores(conn) {
		maps.Copy(pickers, store.Pickers)
	}
	return []tui.Screen{{
		ID:      Screen,
		Group:   tui.GroupSecrets,
		Order:   10,
		Title:   "Secret stores",
		Short:   "Load API keys from a password manager or a command",
		Slash:   []string{"secrets"},
		Usage:   "[@profile] [store or words to search]",
		Area:    secretsconfig.Key,
		Pickers: pickers,
		Pages: func(kit *tui.Kit, name string) []tui.Page {
			return storePages(kit, service, s.stores.Stores(conn), name)
		},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			p := &page{kit: kit, service: service, settings: settings, stores: s.stores.Stores(conn), profile: name}
			return p.open(target)
		},
	}}
}

// The page a store's own steps run on: the kit, the hub and the profile, with the note said and the stores read again once a step changed
// one
type Page struct {
	Kit     *tui.Kit
	Hub     *cards.Hub
	Profile string
	page    *page
}

// Says what was done and reads the stores again
func (p *Page) Done(note string) tea.Cmd {
	return tea.Batch(p.Hub.Done(note), p.page.load(nil))
}

// The page of Secret stores for one profile, and what every page inside it reads
type page struct {
	kit      *tui.Kit
	service  secretsv1connect.SecretServiceClient
	settings configv1connect.ConfigServiceClient
	stores   []Store
	// Profile whose stores are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	state   *secretsv1.SourcesResponse
	loaded  bool
	err     error
	// A view was opened over the first view, which reads again once it is back on top
	opened bool
}

// Row of the first view: a store, the order of the stores, an option, or the row of the advanced options
type row struct {
	source   *secretsv1.Source
	order    bool
	setting  *configv1.Setting
	advanced bool
}

func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Secret stores",
		Glyph:   t.Glyphs.Keys,
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
			if shown[i].source != nil {
				return "Stores"
			}
			return "Options"
		},
		Words: func(i int) string {
			if s := shown[i].source; s != nil {
				return s.GetName()
			}
			return ""
		},
		Detail: func(i int) []string { return p.detail(shown[i], p.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "No secret store is installed.") },
		Free:   map[string]bool{"hub.refresh": true},
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
			if setting := shown[i].setting; setting != nil {
				return p.options.Hints(setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				p.opened = true
				switch r := shown[i]; {
				case r.source != nil:
					return p.storePage(r.source.GetName())
				case r.order:
					return p.orderPage()
				case r.setting != nil:
					return p.options.Change(r.setting)
				}
				if _, _, open := p.areaOptions(); open != nil {
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
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.reload() },
		},
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// What the area is, then in the warm color the variables two stores both hold
func (p *page) intro() []string {
	lines := []string{"Load API keys from Bitwarden, 1Password or a command."}
	if p.loaded && p.err == nil {
		for _, conflict := range p.state.GetConflicts() {
			lines = append(lines, render.Warm(conflict, p.kit.Env.Theme))
		}
	}
	return lines
}

// Rows of the first view
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []row
	for _, source := range p.state.GetSources() {
		rows = append(rows, row{source: source})
	}
	rows = append(rows, row{order: true})
	shown, advanced, _ := p.areaOptions()
	for _, setting := range shown {
		rows = append(rows, row{setting: setting})
	}
	if advanced != nil {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Options every store shares, and the cells of the row of the advanced ones with what it opens; the advanced ones in place while they are
// few
func (p *page) areaOptions() (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Own(secretsconfig.Key)
	if cells, open, ok := p.options.OwnAdvanced(secretsconfig.Key); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	switch {
	case r.source != nil:
		return []string{r.source.GetLabel(), "", standing(r.source, t)}
	case r.order:
		return []string{"Store order", render.Dim(t.Glyphs.Forward, t), p.orderWords()}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.areaOptions()
	return advanced
}

// State of a store in a few words, in the color of how it stands: On with how many variables it fills, Off, or what it needs
func standing(source *secretsv1.Source, t theme.Theme) string {
	switch {
	case !source.GetEnabled():
		return "Off"
	case source.GetNeeds() != "":
		return render.Warm(source.GetNeeds(), t)
	}
	return render.Good("On", t) + " · " + strs.Count(int(source.GetFills()), "variable")
}

// The stores in the order they are asked, by name
func (p *page) orderWords() string {
	var labels []string
	for _, source := range p.state.GetSources() {
		labels = append(labels, source.GetLabel())
	}
	return strings.Join(labels, ", ")
}

// Lines under the rows about the row in hand
func (p *page) detail(r row, cut bool) []string {
	switch {
	case r.source != nil:
		return p.storeAbout(r.source)
	case r.order:
		return []string{"The order the stores are asked in. The first store to give a variable wins it."}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of Secret stores that few people change."}
}

// What a store does, said under its row, and what it lacks
func (p *page) storeAbout(source *secretsv1.Source) []string {
	var lines []string
	if info := p.options.Info(source.GetSection()); info != nil {
		lines = append(lines, strs.Full(info.GetDescription()))
	}
	switch lacks := strs.Full(source.GetLacks()); {
	case lacks == "":
	case source.GetEnabled():
		lines = append(lines, render.Warm(lacks, p.kit.Env.Theme))
	default:
		lines = append(lines, render.Dim(lacks, p.kit.Env.Theme))
	}
	return lines
}

// Goes where the page was opened at: the order of the stores, the variables of a store, the page of a store or of one of its parts, else
// the stores searched for the words
func (p *page) reach(target string) tea.Cmd {
	if target = strings.TrimSpace(target); target == "" {
		return nil
	}
	if strings.EqualFold(target, OrderOf) {
		p.opened = true
		return p.orderPage()
	}
	name, part, _ := strings.Cut(target, ":")
	for _, source := range p.state.GetSources() {
		if !strings.EqualFold(source.GetName(), name) && !strings.EqualFold(source.GetLabel(), name) {
			continue
		}
		p.opened = true
		open := p.storePage(source.GetName())
		switch {
		case part == strings.TrimPrefix(VariablesOf, ":"):
			return tea.Batch(open, p.variablesPage(source.GetName()))
		case part != "":
			return tea.Batch(open, p.partPage(source.GetName(), part))
		}
		return open
	}
	p.root.Filter(target)
	return nil
}

// Reads the stores, then the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return p.read(func() (*secretsv1.SourcesResponse, error) {
		return p.service.Sources(p.kit.Env.Ctx, &secretsv1.SourcesRequest{Profile: p.profile})
	}, then)
}

// Fetches every store again, then reads them
func (p *page) reload() tea.Cmd {
	return p.read(func() (*secretsv1.SourcesResponse, error) {
		return p.service.Reload(p.kit.Env.Ctx, &secretsv1.ReloadRequest{Profile: p.profile})
	}, nil)
}

func (p *page) read(call func() (*secretsv1.SourcesResponse, error), then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := call()
		return func() tea.Cmd {
			return p.options.Load(func() tea.Cmd {
				p.loaded, p.err = true, err
				if err == nil {
					p.state = response
				}
				p.refresh()
				switch {
				case err != nil && p.hub.Top() != p.root:
					// The first view says why in place of its rows; a view over it has the reason under it
					return p.hub.Fail(tui.Reason(err))
				case then == nil || err != nil:
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

// Store of the name as last read, nil when it is gone
func (p *page) find(name string) *secretsv1.Source {
	for _, source := range p.state.GetSources() {
		if source.GetName() == name {
			return source
		}
	}
	return nil
}

// What a store brings of its own, nothing for a store that brings nothing
func (p *page) own(name string) Store {
	for _, store := range p.stores {
		if store.Source == name {
			return store
		}
	}
	return Store{Source: name}
}

// Runs a change of the stores with the hub saying it waits, then says what it did and reads the stores again
func (p *page) change(call func() (*secretsv1.ChangeResponse, error), then func() tea.Cmd) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, call, func(response *secretsv1.ChangeResponse) tea.Cmd {
		cmds := []tea.Cmd{p.hub.Done(response.GetNote()), p.load(nil)}
		if then != nil {
			cmds = append(cmds, then())
		}
		return tea.Batch(cmds...)
	})
}

// The page the steps of a store run on
func (p *page) steps() *Page {
	return &Page{Kit: p.kit, Hub: p.hub, Profile: p.profile, page: p}
}
