package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

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
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the web search services: which service answers web searches and reads pages
const Screen = "web"

// Column of the picker that holds what each row is, shown whole or not at all
const aboutColumn = 3

// What the automatic choice does, as the setting of the service says it
const automaticAbout = "The first service you have an API key for, else a free one"

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
		Usage: "[@profile] [words to search]",
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, search := profile.Split(argument)
			return (&picker{kit: kit, service: service, settings: settings, profile: name}).open(search)
		},
	}}
}

type picker struct {
	kit      *tui.Kit
	service  webv1connect.WebServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	state   *webv1.ListResponse
	loaded  bool
	err     error
	// Rows as last listed: the automatic choice, then the services in use, those ready and those that need setting up
	rows []*webv1.Backend
}

func (p *picker) open(search string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title(p.profile),
		Glyph:   t.Glyphs.Fetch,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search services",
		Intro: func() []string {
			return []string{"Choose the service the agent searches the web and reads pages with."}
		},
		Banner: func() string {
			if p.loaded && p.err == nil && !p.state.GetAvailable() {
				return "The agent can't search the web until a service is ready."
			}
			return ""
		},
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			p.rows = p.order()
			cells := make([][]string, len(p.rows))
			for i, backend := range p.rows {
				if backend == nil {
					cells[i] = []string{p.kit.Mark(true, p.automatic()), "Automatic", "", render.Dim(automaticAbout, t)}
					continue
				}
				cells[i] = []string{p.kit.Mark(true, p.chosen(backend)), backend.GetTitle(), p.stateOf(backend), render.Dim(backend.GetDescription(), t)}
			}
			return cells
		},
		Group: func(i int) string {
			switch backend := p.rows[i]; {
			case backend == nil:
				return ""
			case rank(backend) < 2:
				return "Ready to use"
			}
			return "Needs setting up"
		},
		Words: func(i int) string {
			if backend := p.rows[i]; backend != nil {
				return backend.GetName() + " " + backend.GetDescription()
			}
			return automaticAbout
		},
		Detail: func(i int) []string { return p.detail(p.rows[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "There are no web search services.") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(p.rows) {
				return nil
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "choose"}}
			if p.section(p.rows[i]) != "" {
				hints = append(hints, wm.Hint{Action: "hub.edit", Label: "open settings"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if backend := p.rows[i]; backend != nil {
					return p.use(backend)
				}
				return p.leave()
			},
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.configure(p.rows[i]) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	p.list.ToggleDetail()
	p.hub = p.kit.Hub(Screen, p.list)
	if search != "" {
		p.list.Filter(search)
	}
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

func (p *picker) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &webv1.ListRequest{Profile: p.profile})
		return func() tea.Cmd {
			if err == nil {
				p.state = response
			}
			p.err, p.loaded = err, true
			p.list.Refresh()
			return nil
		}
	})
}

// Rows of the picker once the services are read: the automatic choice, a nil row, then every service, those in use first, then those ready
// to use, then those that need setting up, each run in the order the services were registered
func (p *picker) order() []*webv1.Backend {
	if p.state == nil {
		return nil
	}
	backends := slices.Clone(p.state.GetBackends())
	slices.SortStableFunc(backends, func(a, b *webv1.Backend) int { return rank(a) - rank(b) })
	return append([]*webv1.Backend{nil}, backends...)
}

// Place of a service in the picker: 0 in use, 1 ready to use, 2 needing setting up
func rank(backend *webv1.Backend) int {
	switch _, ready := readiness(backend); {
	case backend.GetSearching() || backend.GetReading():
		return 0
	case ready:
		return 1
	}
	return 2
}

// Whether the automatic choice is in force: no service chosen for both and none pinned for one of them
func (p *picker) automatic() bool {
	return p.state.GetBackend() == "" && p.state.GetSearchBackend() == "" && p.state.GetExtractBackend() == ""
}

// Whether the service is chosen, for both or pinned for one of them
func (p *picker) chosen(backend *webv1.Backend) bool {
	name := backend.GetName()
	return p.state.GetBackend() == name || p.state.GetSearchBackend() == name || p.state.GetExtractBackend() == name
}

// How ready a service is, in words, and whether it can answer as it is or once chosen: set up, free, or what it needs first
func readiness(backend *webv1.Backend) (string, bool) {
	switch {
	case backend.GetReady():
		return "Ready", true
	case backend.GetFree():
		return "Free", true
	}
	plans := backend.GetPlans()
	for _, plan := range plans {
		if plan.GetNeeds() == webv1.Needs_NEEDS_NOTHING {
			return "Free", true
		}
	}
	if len(plans) > 0 {
		switch plans[0].GetNeeds() {
		case webv1.Needs_NEEDS_API_KEY:
			return "Needs an API key", false
		case webv1.Needs_NEEDS_ADDRESS:
			return "Needs a server address", false
		case webv1.Needs_NEEDS_SIGN_IN:
			return "Needs a sign-in", false
		}
	}
	return "Not set up", false
}

// State of a service in its row: what it answers now, else how ready it is
func (p *picker) stateOf(backend *webv1.Backend) string {
	t := p.kit.Env.Theme
	switch {
	case backend.GetRotating():
		return render.Good("In use, taking turns", t)
	case backend.GetSearching() && backend.GetReading():
		return render.Good("In use", t)
	case backend.GetSearching():
		return render.Good("In use for searches", t)
	case backend.GetReading():
		return render.Good("In use for pages", t)
	}
	words, ready := readiness(backend)
	if ready {
		return words
	}
	return render.Dim(words, t)
}

// Lines under the list about the highlighted row: what it is when its row can't show that, then what it does now, whether it was chosen,
// and for a service the ways it can be used and where to get what it needs
func (p *picker) detail(backend *webv1.Backend) []string {
	t := p.kit.Env.Theme
	var lines []string
	if backend == nil {
		if !p.list.Shows(aboutColumn) {
			lines = append(lines, automaticAbout+".")
		}
		lines = append(lines, p.serving()...)
		if !p.state.GetFree() {
			lines = append(lines, render.Dim("Free services are off in settings, so a free plan answers only for a service you choose.", t))
		}
		return lines
	}
	if !p.list.Shows(aboutColumn) {
		lines = append(lines, backend.GetDescription()+".")
	}
	switch {
	case backend.GetRotating():
		lines = append(lines, render.Good("Takes turns with the other free services, so your searches spread over them.", t))
	case backend.GetSearching() && backend.GetReading():
		lines = append(lines, render.Good("Answers the agent's web searches and reads pages for it.", t))
	case backend.GetSearching():
		lines = append(lines, render.Good("Answers the agent's web searches.", t))
	case backend.GetReading():
		lines = append(lines, render.Good("Reads pages for the agent.", t))
	}
	name := backend.GetName()
	switch {
	case p.state.GetBackend() == name:
		lines = append(lines, "You chose it for searches and pages.")
	case p.state.GetSearchBackend() == name && p.state.GetExtractBackend() == name:
		lines = append(lines, "You chose it for searches and for reading pages.")
	case p.state.GetSearchBackend() == name:
		lines = append(lines, "You chose it for searches only.")
	case p.state.GetExtractBackend() == name:
		lines = append(lines, "You chose it for reading pages only.")
	}
	if backend.GetExplicit() {
		lines = append(lines, render.Dim("Used only when you choose it, because its sign-in serves other features too.", t))
	}
	switch backend.GetTier() {
	case webconfig.Free:
		lines = append(lines, "Set to its free plan.")
	case webconfig.Paid:
		lines = append(lines, "Set to its paid plan.")
	}
	if words, ready := readiness(backend); !ready {
		lines = append(lines, render.Warm(words+". Choose it to set it up.", t))
	}
	for _, plan := range backend.GetPlans() {
		line := plan.GetTitle()
		if badge := plan.GetBadge(); badge != "" {
			line += render.Dim(" · "+badge, t)
		}
		lines = append(lines, "", line)
		if about := plan.GetDescription(); about != "" {
			lines = append(lines, render.Dim(about+".", t))
		}
		if prompt := plan.GetPrompt(); prompt != "" && plan.GetKey() == "" {
			lines = append(lines, render.Dim("Needs: "+prompt+".", t))
		}
		if link := plan.GetLink(); link != "" {
			lines = append(lines, render.Dim(link, t))
		}
	}
	return lines
}

// What answers web searches and reads pages now, said for the automatic choice
func (p *picker) serving() []string {
	t := p.kit.Env.Theme
	if !p.state.GetAvailable() {
		return []string{render.Warm("No service is ready, so the agent can't search the web or read pages.", t)}
	}
	var searching, reading []string
	rotating := false
	for _, backend := range p.state.GetBackends() {
		if backend.GetRotating() {
			rotating = true
			continue
		}
		if backend.GetSearching() {
			searching = append(searching, backend.GetTitle())
		}
		if backend.GetReading() {
			reading = append(reading, backend.GetTitle())
		}
	}
	var lines []string
	switch {
	case len(searching) > 0 && slices.Equal(searching, reading):
		lines = append(lines, render.Good("Now "+strings.Join(searching, " and ")+" answers searches and reads pages.", t))
	default:
		if len(searching) > 0 {
			lines = append(lines, render.Good("Now "+strings.Join(searching, " and ")+" answers searches.", t))
		}
		if len(reading) > 0 {
			lines = append(lines, render.Good("Now "+strings.Join(reading, " and ")+" reads pages.", t))
		}
	}
	if rotating {
		lines = append(lines, render.Good("The free services take turns with the rest, so your searches spread over them.", t))
	}
	return lines
}

// Ways to use the service: each of its plans for searches and pages, or the service for one of them alone. The way in use is marked
func (p *picker) use(backend *webv1.Backend) tea.Cmd {
	name, title := backend.GetName(), backend.GetTitle()
	var choices []tui.Choice
	current := ""
	for i, plan := range backend.GetPlans() {
		id := "plan:" + strconv.Itoa(i)
		choices = append(choices, tui.Choice{ID: id, Title: plan.GetTitle(), Detail: plan.GetDescription()})
		if p.state.GetBackend() == name && (current == "" || plan.GetTier() != "" && plan.GetTier() == backend.GetTier()) {
			current = id
		}
	}
	if len(choices) == 0 {
		choices = append(choices, tui.Choice{ID: "both", Title: "For searches and pages", Detail: title + " answers searches and reads pages"})
		if p.state.GetBackend() == name {
			current = "both"
		}
	}
	if backend.GetSearch() {
		choices = append(choices, tui.Choice{ID: "search", Title: "Only for searches", Detail: title + " answers searches, and another service reads pages"})
		if p.state.GetSearchBackend() == name && p.state.GetBackend() != name {
			current = "search"
		}
	}
	if backend.GetExtract() {
		choices = append(choices, tui.Choice{ID: "extract", Title: "Only for reading pages", Detail: title + " reads pages, and another service answers searches"})
		if p.state.GetExtractBackend() == name && p.state.GetBackend() != name && p.state.GetSearchBackend() != name {
			current = "extract"
		}
	}
	return p.kit.Pick(p.hub, "How to use "+title, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
		switch {
		case choice.ID == "search":
			return p.write(title+" now answers the agent's web searches.", write{key: webconfig.SearchBackend, value: name})
		case choice.ID == "extract":
			return p.write(title+" now reads pages for the agent.", write{key: webconfig.ExtractBackend, value: name})
		case strings.HasPrefix(choice.ID, "plan:"):
			i, _ := strconv.Atoi(strings.TrimPrefix(choice.ID, "plan:"))
			return p.apply(backend, backend.GetPlans()[i])
		}
		return p.apply(backend, nil)
	})
}

// Chooses the service for searches and pages on the plan, pinning or clearing its plan. A plan that needs a key or an address the service
// lacks asks for it first, and nothing changes until it is given
func (p *picker) apply(backend *webv1.Backend, plan *webv1.Plan) tea.Cmd {
	tiers := map[string]string{}
	for name, tier := range p.state.GetTiers() {
		tiers[name] = tier
	}
	delete(tiers, backend.GetName())
	if plan.GetTier() != "" {
		tiers[backend.GetName()] = plan.GetTier()
	}
	writes := []write{{key: webconfig.Backend, value: backend.GetName()}, {key: webconfig.ProviderTier, value: tiers}}
	note := backend.GetTitle() + " is now in use for searches and pages."
	switch plan.GetTier() {
	case webconfig.Free:
		note = backend.GetTitle() + " is now in use for searches and pages, on its free plan."
	case webconfig.Paid:
		note = backend.GetTitle() + " is now in use for searches and pages, on its paid plan."
	}
	if plan.GetKey() != "" && plan.GetTier() != webconfig.Free && !backend.GetReady() {
		return p.ask(backend, plan, note, writes)
	}
	return p.write(note, writes...)
}

// Form asking for the API key or the server address a plan needs, then saving it with the choice of the service. A service that answers on
// a free plan without it takes an empty field
func (p *picker) ask(backend *webv1.Backend, plan *webv1.Plan, note string, writes []write) tea.Cmd {
	field := cards.Field{ID: "value", Label: "API key", Kind: cards.FieldSecret, Required: !backend.GetFree()}
	intro := backend.GetTitle() + " needs an API key before it can answer."
	if plan.GetNeeds() == webv1.Needs_NEEDS_ADDRESS {
		field.Label, field.Kind = "Address", cards.FieldText
		intro = backend.GetTitle() + " needs the address of your server."
	}
	var help []string
	if prompt := plan.GetPrompt(); prompt != "" {
		help = append(help, prompt+".")
	}
	if link := plan.GetLink(); link != "" {
		help = append(help, "Get one at "+link)
	}
	if !field.Required {
		intro = backend.GetTitle() + " answers on its free plan without an API key, and a key raises its limits."
		help = append(help, "Leave empty to use it without one.")
	}
	field.Help = strings.Join(help, "\n")
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  backend.GetTitle(),
		Intro:  []string{intro},
		Fields: []cards.Field{field},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			if value := strings.TrimSpace(values["value"]); value != "" {
				writes = append(writes, write{key: plan.GetKey(), value: value})
			}
			return tea.Batch(p.hub.Close(form), p.write(note, writes...))
		},
	})
	return p.hub.Push(form)
}

// Leaves the services to what is set up: clears the choice for both and each service pinned for one of them
func (p *picker) leave() tea.Cmd {
	if p.automatic() {
		return p.hub.Done("Automatic is already in use.")
	}
	return p.write("Web search is now Automatic. icebath uses the first service you have an API key for, else a free one.",
		write{key: webconfig.Backend, value: ""}, write{key: webconfig.SearchBackend, value: ""}, write{key: webconfig.ExtractBackend, value: ""})
}

// Section of the settings that holds the row's settings: the service's own, or web search itself for the automatic choice. Empty for a
// service with no settings
func (p *picker) section(backend *webv1.Backend) string {
	if backend == nil {
		return webconfig.Key
	}
	settings := backend.GetSettings()
	if len(settings) == 0 {
		return ""
	}
	return settings[0][:strings.LastIndex(settings[0], ".")]
}

// Opens the settings of the row in place of the picker
func (p *picker) configure(backend *webv1.Backend) tea.Cmd {
	section := p.section(backend)
	if section == "" {
		return nil
	}
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, section)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, then reads the services again and tells the screens that follow a setting of the profile in use
func (p *picker) write(note string, writes ...write) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() ([]string, error) {
		var saved []string
		for _, w := range writes {
			value, err := yaml.Marshal(w.value)
			if err != nil {
				return nil, err
			}
			if _, err := p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Key: w.key, Value: string(value), Profile: p.profile}); err != nil {
				return nil, err
			}
			saved = append(saved, strings.TrimSpace(string(value)))
		}
		return saved, nil
	}, func(saved []string) tea.Cmd {
		cmds := []tea.Cmd{p.hub.Done(note), p.load()}
		if p.profile == "" {
			for i, w := range writes {
				cmds = append(cmds, p.kit.Changed(w.key, saved[i]))
			}
		}
		return tea.Batch(cmds...)
	})
}

// Title of the screen, naming the profile when it was opened on one
func title(profile string) string {
	if profile == "" {
		return "Web search"
	}
	return "Web search · " + profile
}
