package app

import (
	"cmp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screens of the image and video services
const (
	Images = "imagegen"
	Videos = "videogen"
)

type screens struct{}

func Register(registry *app.Registry, shown *toolsapp.Registry) {
	registry.Register(screens{})
	shown.Register(&views{rows: map[string][]string{}})
}

// What each screen chooses the service of
type kind struct {
	id, title, short string
	// What the service makes, plural and lower case: images, videos
	noun string
	// Line above the services saying what is chosen
	intro           string
	proto           generatev1.Kind
	provider, model string
}

var kinds = []kind{
	{id: Images, title: "Images", short: "Choose which service makes images for the agent", noun: "images",
		intro: "Choose the service that makes images when the agent asks for one.", proto: generatev1.Kind_KIND_IMAGE,
		provider: generationconfig.ImageProvider, model: generationconfig.ImageModel},
	{id: Videos, title: "Videos", short: "Choose which service makes videos for the agent", noun: "videos",
		intro: "Choose the service that makes videos when the agent asks for one.", proto: generatev1.Kind_KIND_VIDEO,
		provider: generationconfig.VideoProvider, model: generationconfig.VideoModel},
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := generatev1connect.NewGenerateServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	var out []tui.Screen
	for i, k := range kinds {
		out = append(out, tui.Screen{
			ID:    k.id,
			Group: tui.GroupMedia,
			Order: 10 * (i + 1),
			Title: k.title,
			Short: k.short,
			Slash: []string{k.id},
			Usage: "[@profile] [words to search]",
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, search := profile.Split(argument)
				return (&picker{kit: kit, kind: k, service: service, settings: settings, profile: name}).open(search)
			},
		})
	}
	return out
}

type picker struct {
	kit      *tui.Kit
	kind     kind
	service  generatev1connect.GenerateServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	// Lists open over the hub, read again once the services are
	views  []*cards.List
	state  *generatev1.ListResponse
	loaded bool
	err    error
	rows   []row
}

// Row of the list of services: a service, the choice of the first service that is ready, or for videos none at all
type row struct {
	service *generatev1.Service
	auto    bool
	off     bool
}

func (p *picker) open(search string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   p.title(),
		Glyph:   t.Glyphs.Write,
		Key:     p.kind.id,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search services",
		Intro: func() []string {
			lines := []string{p.kind.intro}
			if p.state != nil && !p.state.GetOffered() && p.state.GetReason() != "" {
				lines = append(lines, sentence(p.state.GetReason()))
			}
			return lines
		},
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			p.rows = p.entries()
			cells := make([][]string, len(p.rows))
			for i, r := range p.rows {
				cells[i] = []string{p.kit.Mark(true, p.marked(r)), p.name(r), p.status(r), render.Dim(p.about(r), t)}
			}
			return cells
		},
		Words: func(i int) string {
			if s := p.rows[i].service; s != nil {
				return s.GetName() + " " + s.GetBadge()
			}
			return ""
		},
		Detail: func(i int) []string { return p.describe(p.rows[i]) },
		Empty:  p.empty,
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(p.rows) {
				return nil
			}
			r := p.rows[i]
			var out []wm.Hint
			if verb := p.verb(r); verb != "" {
				out = append(out, wm.Hint{Action: "hub.use", Label: verb})
			}
			if r.service != nil && p.verb(r) == "choose" {
				out = append(out, wm.Hint{Action: "hub.edit", Label: "set up"})
			}
			return out
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				r := p.rows[i]
				switch p.verb(r) {
				case "choose":
					return p.choose(r, "")
				case "set up":
					return p.setup(r.service.GetName())
				}
				return nil
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if r := p.rows[i]; r.service != nil {
					return p.setup(r.service.GetName())
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	p.list.ToggleDetail()
	p.views = append(p.views, p.list)
	p.hub = p.kit.Hub(p.kind.id, p.list)
	if search != "" {
		p.list.Filter(search)
	}
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

// Rows of the list: the first service that is ready, for videos none at all, then each service
func (p *picker) entries() []row {
	if p.state == nil {
		return nil
	}
	rows := []row{{auto: true}}
	if p.kind.id == Videos {
		rows = append(rows, row{off: true})
	}
	for _, s := range p.services() {
		rows = append(rows, row{service: s})
	}
	return rows
}

// Whether the row is the choice the settings hold
func (p *picker) marked(r row) bool {
	chosen := p.state.GetChosen()
	switch {
	case r.auto:
		return chosen == "" && (p.kind.id != Videos || p.state.GetEnabled())
	case r.off:
		return chosen == "" && !p.state.GetEnabled()
	}
	return r.service.GetName() == chosen
}

// Name of a row
func (p *picker) name(r row) string {
	switch {
	case r.auto:
		return "Automatic"
	case r.off:
		return "Off"
	}
	return r.service.GetTitle()
}

// What a row is, in a phrase
func (p *picker) about(r row) string {
	switch {
	case r.auto:
		return "The first service that is ready"
	case r.off:
		return "The agent does not make videos"
	}
	return r.service.GetDescription()
}

// What enter does on a row: choosing it when it is ready and not the choice held, else setting it up; nothing on a choice held that has
// nothing to set up
func (p *picker) verb(r row) string {
	switch {
	case r.service == nil && p.marked(r):
		return ""
	case r.service == nil:
		return "choose"
	case r.service.GetReady() && !p.marked(r):
		return "choose"
	}
	return "set up"
}

// What the list says while it has no rows
func (p *picker) empty() string {
	switch {
	case p.err != nil:
		return "Could not read the services. " + sentence(tui.Reason(p.err)) + " " + p.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !p.loaded:
		return "Reading the services…"
	}
	return "No services that make " + p.kind.noun + " are installed."
}

func (p *picker) services() []*generatev1.Service {
	return p.state.GetServices()
}

// Service of the name as last read, nil when it is gone
func (p *picker) find(name string) *generatev1.Service {
	for _, s := range p.services() {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

func (p *picker) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &generatev1.ListRequest{Profile: p.profile, Kind: p.kind.proto})
		return func() tea.Cmd {
			if err == nil {
				p.state = response
			}
			p.err, p.loaded = err, true
			for _, view := range p.views {
				view.Refresh()
			}
			return nil
		}
	})
}

// How a service stands in a few words: in use when the first ready one is used, else whether it is ready and free or what it still needs
func (p *picker) status(r row) string {
	t := p.kit.Env.Theme
	s := r.service
	switch {
	case s == nil:
		return ""
	case p.state.GetChosen() == "" && s.GetActive() && p.state.GetOffered():
		return render.Good("In use", t)
	case s.GetReady() && s.GetBadge() == "free":
		return render.Good("Free", t)
	case s.GetReady():
		return render.Good("Ready", t)
	case s.GetKey() != "":
		return render.Warm("Needs an API key", t)
	case s.GetProvider() != "":
		return render.Warm("Not connected", t)
	}
	return render.Warm("Not ready", t)
}

// Lines under the list about a row: what it is when the row can't show it, then its model, its cost, how to get its API key or connect
// it, and what it says of itself
func (p *picker) describe(r row) []string {
	t := p.kit.Env.Theme
	var lines []string
	if !p.list.Shows(3) {
		lines = append(lines, sentence(p.about(r)))
	}
	s := r.service
	if s == nil {
		return lines
	}
	var facts []string
	if model := s.GetModel(); model != "" {
		facts = append(facts, "Model: "+model)
	} else {
		facts = append(facts, "Model: its default")
	}
	if badge := s.GetBadge(); badge != "" {
		facts = append(facts, strings.ToUpper(badge[:1])+badge[1:])
	}
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	switch {
	case s.GetKey() != "" && !s.GetReady():
		key := "Needs " + cmp.Or(s.GetPrompt(), "an API key") + "."
		if link := s.GetLink(); link != "" {
			key += " Get one at " + link
		}
		lines = append(lines, render.Dim(key, t))
	case s.GetProvider() != "" && !s.GetReady():
		lines = append(lines, render.Dim("Uses your "+s.GetProvider()+" account. Connect it on the Providers screen.", t))
	}
	for _, note := range s.GetNotes() {
		lines = append(lines, render.Dim(sentence(note), t))
	}
	return lines
}

// Sentence of a reason a server gave: capital first letter, ending in a period
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

// Chooses the row: a service with the model, its default for none, asking for its API key when it has none yet; the first service that
// is ready; or for videos none at all
func (p *picker) choose(r row, model string) tea.Cmd {
	noun := strings.ToUpper(p.kind.noun[:1]) + p.kind.noun[1:]
	switch {
	case r.auto && p.kind.id == Videos:
		return p.write(noun+" now come from the first service that is ready.", write{p.kind.provider, ""}, write{p.kind.model, ""},
			write{generationconfig.VideoEnabled, true})
	case r.auto:
		return p.write(noun+" now come from the first service that is ready.", write{p.kind.provider, ""}, write{p.kind.model, ""})
	case r.off:
		return p.write("The agent no longer makes videos.", write{p.kind.provider, ""}, write{generationconfig.VideoEnabled, false})
	}
	s := r.service
	note := noun + " now come from " + s.GetTitle()
	if model != "" {
		note += ", with the model " + model
	}
	done := p.write(note+".", write{p.kind.provider, s.GetName()}, write{cmp.Or(s.GetModelSetting(), p.kind.model), model})
	if !s.GetReady() && s.GetKey() != "" {
		return tea.Batch(done, p.kit.Open(configapp.Edit, profile.Join(p.profile, s.GetKey())))
	}
	return done
}

// Something to do in the setup of a service
type step struct {
	title, state, about string
	// Word on the button that does it
	verb string
	run  func() tea.Cmd
}

// Steps of the setup of the service as it stands now: choosing it, its model, its API key or account, and its settings
func (p *picker) steps(name string) []step {
	t := p.kit.Env.Theme
	s := p.find(name)
	if s == nil {
		return nil
	}
	use := step{title: "Use " + s.GetTitle(), about: "Make " + p.kind.noun + " with " + s.GetTitle() + " and the model below", verb: "choose",
		run: func() tea.Cmd { return p.choose(row{service: p.find(name)}, p.find(name).GetModel()) }}
	if s.GetName() == p.state.GetChosen() {
		use.state, use.verb, use.run = render.Good("In use", t), "", nil
	}
	model := s.GetModel()
	if model == "" {
		model = render.Dim("Default", t)
	}
	steps := []step{use, {title: "Model", state: model, about: "One of the models of " + s.GetTitle(), verb: "change",
		run: func() tea.Cmd { return p.models(name) }}}
	switch {
	case s.GetKey() != "":
		state := render.Warm("Not set", t)
		if s.GetReady() {
			state = render.Good("Set", t)
		}
		about := cmp.Or(s.GetPrompt(), "The API key of "+s.GetTitle())
		if link := s.GetLink(); link != "" {
			about += ", from " + link
		}
		steps = append(steps, step{title: "API key", state: state, about: about, verb: "change",
			run: func() tea.Cmd {
				return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Edit, profile.Join(p.profile, s.GetKey())))
			}})
	case s.GetProvider() != "":
		state := render.Warm("Not connected", t)
		if s.GetReady() {
			state = render.Good("Connected", t)
		}
		steps = append(steps, step{title: "Account", state: state, about: "Your " + s.GetProvider() + " account, on the Providers screen", verb: "connect",
			run: func() tea.Cmd {
				return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+s.GetProvider())))
			}})
	}
	return append(steps, step{title: "All settings", about: "Open every setting of " + s.GetTitle(), verb: "open",
		run: func() tea.Cmd { return p.configure(name) }})
}

// Opens the setup of the service over the list: choosing it, its model, its API key or account and its settings
func (p *picker) setup(name string) tea.Cmd {
	t := p.kit.Env.Theme
	s := p.find(name)
	if s == nil {
		return nil
	}
	var steps []step
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   s.GetTitle(),
		Glyph:   t.Glyphs.Write,
		Key:     p.kind.id + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Set up " + s.GetTitle() + ", then choose it."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			steps = p.steps(name)
			rows := make([][]string, len(steps))
			for i, step := range steps {
				rows[i] = []string{step.title, step.state, render.Dim(step.about, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if view.Shows(2) {
				return nil
			}
			return []string{steps[i].about}
		},
		Empty: func() string { return s.GetTitle() + " is no longer installed." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(steps) || steps[i].verb == "" {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: steps[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if steps[i].run == nil {
					return nil
				}
				return steps[i].run()
			},
		},
	})
	view.ToggleDetail()
	p.views = append(p.views, view)
	return p.hub.Push(view)
}

// Lists the models of the service to choose one from, the service then chosen with it
func (p *picker) models(name string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	return tui.Call(p.hub, func() (*generatev1.ModelsResponse, error) {
		return p.service.Models(p.kit.Env.Ctx, &generatev1.ModelsRequest{Profile: p.profile, Kind: p.kind.proto, Service: name})
	}, func(response *generatev1.ModelsResponse) tea.Cmd {
		var choices []tui.Choice
		for _, m := range response.GetModels() {
			var facts []string
			if m.GetId() == response.GetDefault() {
				facts = append(facts, "The default")
			}
			choices = append(choices, tui.Choice{ID: m.GetId(), Title: cmp.Or(m.GetTitle(), m.GetId()),
				Detail: strings.Join(filled(append(facts, m.GetStrengths(), m.GetSpeed(), m.GetPrice())...), " · ")})
		}
		if len(choices) == 0 {
			return p.hub.Say(s.GetTitle()+" lists no models now, so it uses its default model.", cards.NoteInfo)
		}
		return p.kit.Pick(p.hub, "Model of "+s.GetTitle(), "choose", choices, cmp.Or(s.GetModel(), response.GetDefault()), func(choice tui.Choice) tea.Cmd {
			return p.choose(row{service: s}, choice.ID)
		})
	})
}

// Texts that are not empty
func filled(texts ...string) []string {
	var out []string
	for _, text := range texts {
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// Opens the settings of the service in place of this screen
func (p *picker) configure(name string) tea.Cmd {
	section := p.kind.provider[:strings.Index(p.kind.provider, ".")]
	if s := p.find(name); s != nil && len(s.GetSettings()) > 0 {
		section = s.GetSettings()[0]
		if i := strings.LastIndex(section, "."); i > 0 {
			section = section[:i]
		}
	}
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, section)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, tells the screens that follow them, then reads the services again
func (p *picker) write(note string, writes ...write) tea.Cmd {
	type saved struct{ key, value string }
	return tui.Call(p.hub, func() ([]saved, error) {
		ctx := p.kit.Env.Ctx
		var out []saved
		for _, w := range writes {
			value, err := yaml.Marshal(w.value)
			if err != nil {
				return nil, err
			}
			if _, err := p.settings.Set(ctx, &configv1.SetRequest{Key: w.key, Value: string(value), Profile: p.profile}); err != nil {
				return nil, err
			}
			out = append(out, saved{w.key, strings.TrimSpace(string(value))})
		}
		return out, nil
	}, func(done []saved) tea.Cmd {
		cmds := []tea.Cmd{p.hub.Done(note), p.load()}
		// Screens follow the settings of the profile in use
		if p.profile == "" {
			for _, s := range done {
				cmds = append(cmds, p.kit.Changed(s.key, s.value))
			}
		}
		return tea.Batch(cmds...)
	})
}

// Title of the screen, naming the profile when it was opened on one
func (p *picker) title() string {
	if p.profile == "" {
		return p.kind.title
	}
	return p.kind.title + " · " + p.profile
}
