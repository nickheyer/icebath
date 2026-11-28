// Pages of Images and Videos in the settings menu: whether videos are made, Automatic and the services with how each stands, the page of each
// with its model and its key or account, and the options of images
package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/profile"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screens of the image and video services, the pages of their areas
const (
	Images = "images"
	Videos = "videos"
)

type screens struct{}

func Register(registry *app.Registry, shown *toolsapp.Registry) {
	registry.Register(screens{})
	shown.Register(&views{rows: map[string][]string{}})
}

// What each page chooses the service of
type kind struct {
	id, title, short string
	// Names the page answered to before, which no list shows
	aliases []string
	// What the service makes, plural and lower case: images, videos
	noun string
	// Area of the page and the kind of thing the model settings of its services name
	area, models string
	proto        generatev1.Kind
}

var kinds = []kind{
	{id: Images, title: "Images", short: "Choose which service makes images for the agent", aliases: []string{"imagegen"}, noun: "images",
		area: generationconfig.ImageKey, models: generationconfig.ImageModels, proto: generatev1.Kind_KIND_IMAGE},
	{id: Videos, title: "Videos", short: "Choose which service makes videos for the agent", aliases: []string{"videogen"}, noun: "videos",
		area: generationconfig.VideoKey, models: generationconfig.VideoModels, proto: generatev1.Kind_KIND_VIDEO},
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := generatev1connect.NewGenerateServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	titles := newTitles()
	var out []tui.Screen
	for i, k := range kinds {
		out = append(out, tui.Screen{
			ID:      k.id,
			Group:   tui.GroupMedia,
			Order:   10 * (i + 1),
			Title:   k.title,
			Short:   k.short,
			Slash:   []string{k.id},
			Aliases: k.aliases,
			Usage:   "[@profile] [service or words to search]",
			Area:    k.area,
			Pickers: map[string]tui.Picker{k.models: modelPicker(service, k, titles)},
			Pages:   func(kit *tui.Kit, name string) []tui.Page { return servicePages(kit, service, k, name) },
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, target := profile.Split(argument)
				return (&page{kit: kit, kind: k, service: service, settings: settings, titles: titles, profile: name}).open(target)
			},
		})
	}
	return out
}

// The page of Images or Videos for one profile, and what every page inside it reads
type page struct {
	kit      *tui.Kit
	kind     kind
	service  generatev1connect.GenerateServiceClient
	settings configv1connect.ConfigServiceClient
	titles   *titles
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	state   *generatev1.ListResponse
	loaded  bool
	err     error
	// A view was opened over the first view, which reads again once it is back on top
	opened bool
}

// Row of the first view: the switch of videos, Automatic, a service, an option, or the row of the advanced options
type row struct {
	videos   bool
	auto     bool
	service  *generatev1.Service
	setting  *configv1.Setting
	advanced bool
}

func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   p.kind.title,
		Glyph:   t.Glyphs.Write,
		Key:     p.kind.id,
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
			case r.videos:
				return ""
			case r.auto || r.service != nil:
				return "Services"
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
		Empty: func() string {
			return tui.Waiting(p.loaded, p.err, "No services that make "+p.kind.noun+" are installed.")
		},
		Free: map[string]bool{"hub.refresh": true},
		// Read again as a view over it closes, which may have changed how the services stand, such as a key kept
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
				case r.videos:
					return p.makeVideos(!p.state.GetOn())
				case r.auto:
					if p.state.GetChosen() == "" {
						return nil
					}
					return p.choose("")
				case r.service != nil && ready(r.service) && !r.service.GetChosen():
					return p.choose(r.service.GetName())
				case r.service != nil:
					p.opened = true
					return p.servicePage(r.service.GetName())
				case r.setting != nil:
					p.opened = true
					return p.options.Change(r.setting)
				}
				if _, _, open := p.kindOptions(); open != nil {
					p.opened = true
					return open()
				}
				return nil
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if s := shown[i].service; s != nil {
					p.opened = true
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
	hub, cmd := p.kit.Show(p.kind.id, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// What the area is, then in the warm color what keeps the agent from making them
func (p *page) intro() []string {
	lines := []string{"Which service makes " + p.kind.noun + " for the agent."}
	if p.loaded && p.err == nil && p.state.GetHindrance() != "" {
		lines = append(lines, render.Warm(p.state.GetHindrance(), p.kit.Env.Theme))
	}
	return lines
}

// Rows of the first view
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []row
	if p.kind.id == Videos {
		rows = append(rows, row{videos: true})
	}
	rows = append(rows, row{auto: true})
	for _, s := range p.state.GetServices() {
		rows = append(rows, row{service: s})
	}
	shown, advanced, _ := p.kindOptions()
	for _, setting := range shown {
		rows = append(rows, row{setting: setting})
	}
	if advanced != nil {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Options of the area, and the cells of the row of the advanced ones with what it opens; the advanced ones in place, no row, while they are
// few
func (p *page) kindOptions() (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Section(p.kind.area)
	if cells, open, ok := p.options.Advanced(p.kind.area); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

// Whether the service has its key or account
func ready(s *generatev1.Service) bool {
	switch s.GetState() {
	case generatev1.State_STATE_IN_USE, generatev1.State_STATE_READY, generatev1.State_STATE_FREE:
		return true
	}
	return false
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	switch {
	case r.videos:
		if setting := p.options.Setting(generationconfig.VideoEnabled); setting != nil {
			return p.options.Cells(setting)
		}
		return []string{"Make videos", "", onOff(p.state.GetOn())}
	case r.auto:
		return []string{"Automatic", p.kit.Mark(true, p.state.GetChosen() == ""), ""}
	case r.service != nil:
		return []string{r.service.GetTitle(), p.kit.Mark(true, r.service.GetChosen()), stateWords(r.service, t)}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.kindOptions()
	return advanced
}

// On or Off
func onOff(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

// State of a service in its words, in the color of how it stands
func stateWords(s *generatev1.Service, t theme.Theme) string {
	switch {
	case s.GetState() == generatev1.State_STATE_IN_USE:
		return render.Good(s.GetWords(), t)
	case ready(s):
		return s.GetWords()
	}
	return render.Warm(s.GetWords(), t)
}

// Lines under the rows about the row in hand: what the switch does, what Automatic does and which service it takes now, what a service is
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.videos:
		return []string{"Let the agent make videos from text, a still image or reference images."}
	case r.auto:
		lines := []string{"The first service in this list that is ready."}
		if serving := p.state.GetServing(); serving != "" && p.state.GetChosen() == "" {
			lines = append(lines, render.Dim(serving+" makes "+p.kind.noun+" now.", t))
		}
		return lines
	case r.service != nil:
		lines := []string{strs.Full(r.service.GetDescription())}
		for _, note := range r.service.GetNotes() {
			lines = append(lines, render.Dim(strs.Full(note), t))
		}
		return lines
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of " + p.kind.title + " that few people change."}
}

func (p *page) hints(r row) []wm.Hint {
	switch {
	case r.videos && p.state.GetOn():
		return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
	case r.videos:
		return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
	case r.auto && p.state.GetChosen() == "":
		return nil
	case r.auto:
		return []wm.Hint{{Action: "hub.use", Label: "use"}}
	case r.service != nil && ready(r.service) && !r.service.GetChosen():
		return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "open"}}
	case r.service != nil, r.advanced:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	}
	return p.options.Hints(r.setting)
}

// Goes where the page was opened at: the page of a service named by its name or title, else the services searched for the words
func (p *page) reach(target string) tea.Cmd {
	if target = strings.TrimSpace(target); target == "" {
		return nil
	}
	for _, s := range p.state.GetServices() {
		if strings.EqualFold(s.GetName(), target) || strings.EqualFold(s.GetTitle(), target) {
			p.opened = true
			return p.servicePage(s.GetName())
		}
	}
	p.root.Filter(target)
	return nil
}

// Reads the services, then the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &generatev1.ListRequest{Profile: p.profile, Kind: p.kind.proto})
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
func (p *page) find(name string) *generatev1.Service {
	for _, s := range p.state.GetServices() {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// Uses a service, its model kept, or Automatic for none, saying what makes them now, and tells the screens that follow the settings
func (p *page) choose(name string) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*generatev1.ChooseResponse, error) {
		return p.service.Choose(p.kit.Env.Ctx, &generatev1.ChooseRequest{Profile: p.profile, Kind: p.kind.proto, Service: name})
	}, func(response *generatev1.ChooseResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(response.GetNote()), p.load(p.changed))
	})
}

// Turns the making of videos on or off
func (p *page) makeVideos(on bool) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*generatev1.ChooseResponse, error) {
		return p.service.Videos(p.kit.Env.Ctx, &generatev1.VideosRequest{Profile: p.profile, On: on})
	}, func(response *generatev1.ChooseResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(response.GetNote()), p.load(p.changed))
	})
}

// Tells the screens that follow the settings of the area what they hold now, for the profile in use
func (p *page) changed() tea.Cmd {
	if p.profile != "" {
		return nil
	}
	keys := []string{generationconfig.ImageProvider}
	if p.kind.id == Videos {
		keys = []string{generationconfig.VideoProvider, generationconfig.VideoEnabled}
	}
	var cmds []tea.Cmd
	for _, key := range keys {
		if setting := p.options.Setting(key); setting != nil {
			cmds = append(cmds, p.kit.Changed(key, setting.GetValue()))
		}
	}
	return tea.Batch(cmds...)
}
