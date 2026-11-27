package app

import (
	"cmp"
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

// Screen of the web backends: which service answers web searches and reads pages
const Screen = "web"

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := webv1connect.NewWebServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Title: "Web search",
		Short: "Pick the service that answers web searches and reads pages",
		Slash: []string{"web"},
		Usage: "[@profile] [search]",
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
	// Profile whose backends are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	state   *webv1.ListResponse
	loaded  bool
	err     error
}

func (p *picker) open(search string) tea.Cmd {
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title(p.profile),
		Glyph:   p.kit.Env.Theme.Glyphs.Fetch,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search web services",
		Columns: []render.Column{{Max: 44}, {Title: "Does"}, {Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			backends := p.backends()
			rows := make([][]string, len(backends))
			for i, backend := range backends {
				rows[i] = []string{backend.GetTitle(), render.Dim(does(backend), p.kit.Env.Theme), p.status(backend)}
			}
			return rows
		},
		Detail: func(i int) []string { return p.describe(p.backends()[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "No web services are registered") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "use"}}
			if len(p.backends()[i].GetSettings()) > 0 {
				hints = append(hints, wm.Hint{Action: "hub.edit", Label: "settings"})
			}
			return append(hints, wm.Hint{Action: "hub.reset", Label: "automatic"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.use(p.backends()[i]) },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.configure(p.backends()[i]) },
			"hub.reset":   func(_ *cards.List, i int) tea.Cmd { return p.automatic(p.backends()[i]) },
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

func (p *picker) backends() []*webv1.Backend {
	return p.state.GetBackends()
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

// What the backend does, in a word or two
func does(backend *webv1.Backend) string {
	if backend.GetExtract() {
		return "search, pages"
	}
	return "search"
}

// How the backend stands in a few words: what it answers now, else whether it is set up
func (p *picker) status(backend *webv1.Backend) string {
	t := p.kit.Env.Theme
	var serves []string
	if backend.GetSearching() {
		serves = append(serves, "searching")
	}
	if backend.GetReading() {
		serves = append(serves, "reading pages")
	}
	switch {
	case len(serves) > 0 && backend.GetRotating():
		return render.Good("Free, in turn: "+strings.Join(serves, ", "), t)
	case len(serves) > 0:
		return render.Good("In use: "+strings.Join(serves, ", "), t)
	case backend.GetReady():
		return "Set up"
	case backend.GetFree():
		return render.Dim("Free plan", t)
	}
	return render.Dim("Not set up", t)
}

// What the backend is, what it needs, and how it is chosen and paid for
func (p *picker) describe(backend *webv1.Backend) []string {
	t := p.kit.Env.Theme
	lines := []string{backend.GetDescription()}
	if !p.state.GetAvailable() {
		lines = append(lines, render.Warm("No web service is set up or free, so web_search and web_extract are off", t))
	}
	var chosen []string
	if p.state.GetBackend() == backend.GetName() {
		chosen = append(chosen, "for searching and reading pages")
	}
	if p.state.GetSearchBackend() == backend.GetName() {
		chosen = append(chosen, "pinned for searching")
	}
	if p.state.GetExtractBackend() == backend.GetName() {
		chosen = append(chosen, "pinned for reading pages")
	}
	if len(chosen) > 0 {
		lines = append(lines, render.Good("Chosen: "+strings.Join(chosen, ", "), t))
	}
	if backend.GetExplicit() {
		lines = append(lines, render.Dim("Used only once chosen here: its credential serves other features too", t))
	}
	if tier := backend.GetTier(); tier != webconfig.Auto {
		lines = append(lines, "Plan: "+tier)
	}
	for _, plan := range backend.GetPlans() {
		line := plan.GetTitle()
		if badge := plan.GetBadge(); badge != "" {
			line += render.Dim("  "+badge, t)
		}
		lines = append(lines, line)
		if about := plan.GetDescription(); about != "" {
			lines = append(lines, render.Dim("  "+about, t))
		}
		if link := plan.GetLink(); link != "" {
			lines = append(lines, render.Dim("  "+link, t))
		}
	}
	return append(lines, render.Dim(backend.GetName(), t))
}

// Ways to use the backend: each of its plans for searching and reading pages, or pinned for one of them
func (p *picker) use(backend *webv1.Backend) tea.Cmd {
	var choices []tui.Choice
	for i, plan := range backend.GetPlans() {
		choices = append(choices, tui.Choice{ID: "plan:" + strconv.Itoa(i), Title: plan.GetTitle(), Detail: strings.TrimSpace(plan.GetBadge() + "  " + plan.GetDescription())})
	}
	if len(choices) == 0 {
		choices = append(choices, tui.Choice{ID: "both", Title: "Use " + backend.GetTitle(), Detail: "For web searches and reading pages"})
	}
	if backend.GetSearch() {
		choices = append(choices, tui.Choice{ID: "search", Title: "Only for searching", Detail: "Pins it in web.search_backend"})
	}
	if backend.GetExtract() {
		choices = append(choices, tui.Choice{ID: "extract", Title: "Only for reading pages", Detail: "Pins it in web.extract_backend"})
	}
	return p.kit.Pick(p.hub, "Use "+backend.GetTitle(), "use", choices, "", func(choice tui.Choice) tea.Cmd {
		switch {
		case choice.ID == "search":
			return p.write(backend, "Pinned "+backend.GetTitle()+" for searching", write{key: webconfig.SearchBackend, value: backend.GetName()})
		case choice.ID == "extract":
			return p.write(backend, "Pinned "+backend.GetTitle()+" for reading pages", write{key: webconfig.ExtractBackend, value: backend.GetName()})
		case strings.HasPrefix(choice.ID, "plan:"):
			i, _ := strconv.Atoi(strings.TrimPrefix(choice.ID, "plan:"))
			return p.apply(backend, backend.GetPlans()[i])
		}
		return p.apply(backend, nil)
	})
}

// Chooses the backend for searching and reading pages on the plan, pinning or clearing its tier, then asks for its key when the plan
// needs one that is not set
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
	note := "Web searches and pages now go to " + backend.GetTitle()
	if plan.GetTier() != "" {
		note += " (" + plan.GetTier() + " plan)"
	}
	done := p.write(backend, note, writes...)
	if key := plan.GetKey(); key != "" && (plan.GetTier() != "free") && !backend.GetReady() {
		return tea.Batch(done, p.kit.Open(configapp.Edit, profile.Join(p.profile, key)))
	}
	return done
}

// Leaves the backends to what is set up: clears the choice for both and each pin that names this backend
func (p *picker) automatic(backend *webv1.Backend) tea.Cmd {
	writes := []write{{key: webconfig.Backend, value: ""}}
	if p.state.GetSearchBackend() == backend.GetName() {
		writes = append(writes, write{key: webconfig.SearchBackend, value: ""})
	}
	if p.state.GetExtractBackend() == backend.GetName() {
		writes = append(writes, write{key: webconfig.ExtractBackend, value: ""})
	}
	return p.write(backend, "Web searches and pages go to the first service set up, else a free one", writes...)
}

// Opens the settings of the backend
func (p *picker) configure(backend *webv1.Backend) tea.Cmd {
	settings := backend.GetSettings()
	if len(settings) == 0 {
		return nil
	}
	section := settings[0][:strings.LastIndex(settings[0], ".")]
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, section)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, then reads the backends again
func (p *picker) write(backend *webv1.Backend, note string, writes ...write) tea.Cmd {
	return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
		var response *configv1.SetResponse
		for _, w := range writes {
			value, err := yaml.Marshal(w.value)
			if err != nil {
				return nil, err
			}
			if response, err = p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Key: w.key, Value: string(value), Profile: p.profile}); err != nil {
				return nil, err
			}
		}
		return response, nil
	}, func(*configv1.SetResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(cmp.Or(note, backend.GetTitle())), p.load())
	})
}

// Title of the screen, naming the profile when it was opened on one
func title(profile string) string {
	if profile == "" {
		return "Web search"
	}
	return "Web search of " + profile
}
