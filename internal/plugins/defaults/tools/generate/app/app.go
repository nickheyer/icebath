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

// What each screen picks for
type kind struct {
	id, title, short, noun string
	proto                  generatev1.Kind
	provider, model        string
}

var kinds = []kind{
	{id: Images, title: "Image generation", short: "Pick the service that draws pictures for image_generate, and its model", noun: "pictures",
		proto: generatev1.Kind_KIND_IMAGE, provider: generationconfig.ImageProvider, model: generationconfig.ImageModel},
	{id: Videos, title: "Video generation", short: "Pick the service that makes videos for video_generate, and its model", noun: "videos",
		proto: generatev1.Kind_KIND_VIDEO, provider: generationconfig.VideoProvider, model: generationconfig.VideoModel},
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := generatev1connect.NewGenerateServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	var out []tui.Screen
	for _, k := range kinds {
		out = append(out, tui.Screen{
			ID:    k.id,
			Title: k.title,
			Short: k.short,
			Slash: []string{k.id},
			Usage: "[@profile] [search]",
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
	state   *generatev1.ListResponse
	loaded  bool
	err     error
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
		Columns: []render.Column{{Max: 32}, {Title: "Plan"}, {Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			services := p.services()
			rows := make([][]string, len(services))
			for i, s := range services {
				rows[i] = []string{s.GetTitle(), render.Dim(s.GetBadge(), t), p.status(s)}
			}
			return rows
		},
		Detail: func(i int) []string { return p.describe(p.services()[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "No "+p.kind.noun+" services are installed") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "settings"}, {Action: "hub.reset", Label: "automatic"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.use(p.services()[i]) },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.configure(p.services()[i]) },
			"hub.reset":   func(*cards.List, int) tea.Cmd { return p.automatic() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	p.list.ToggleDetail()
	p.hub = p.kit.Hub(p.kind.id, p.list)
	if search != "" {
		p.list.Filter(search)
	}
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

func (p *picker) services() []*generatev1.Service {
	return p.state.GetServices()
}

func (p *picker) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &generatev1.ListRequest{Profile: p.profile, Kind: p.kind.proto})
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

// How the service stands in a few words
func (p *picker) status(s *generatev1.Service) string {
	t := p.kit.Env.Theme
	switch {
	case s.GetActive() && p.state.GetOffered():
		return render.Good("In use", t)
	case s.GetChosen() && !s.GetReady():
		return render.Warm("Chosen, not set up", t)
	case s.GetChosen():
		return render.Warm("Chosen", t)
	case s.GetReady():
		return "Set up"
	}
	return render.Dim("Not set up", t)
}

// What the service makes, what it needs and what the profile's settings make of it
func (p *picker) describe(s *generatev1.Service) []string {
	t := p.kit.Env.Theme
	lines := []string{s.GetDescription()}
	if !p.state.GetOffered() && p.state.GetReason() != "" {
		lines = append(lines, render.Warm(p.state.GetReason(), t))
	}
	if model := s.GetModel(); model != "" {
		lines = append(lines, "Model: "+model)
	}
	switch {
	case s.GetKey() != "":
		lines = append(lines, render.Dim("Key: "+s.GetPrompt()+"  "+s.GetLink(), t))
	case s.GetProvider() != "":
		lines = append(lines, render.Dim("Uses the "+s.GetProvider()+" key or sign-in connected in /providers", t))
	}
	for _, note := range s.GetNotes() {
		lines = append(lines, render.Dim(note, t))
	}
	return append(lines, render.Dim(s.GetName(), t))
}

// Ways of using the service: choosing it, choosing it with one of its models, setting its key or connecting its provider
func (p *picker) use(s *generatev1.Service) tea.Cmd {
	choices := []tui.Choice{{ID: "use", Title: "Use " + s.GetTitle(), Detail: "For " + p.kind.noun + ", with its default model"},
		{ID: "model", Title: "Pick a model", Detail: "Use " + s.GetTitle() + " with a model of its catalog"}}
	switch {
	case s.GetKey() != "":
		choices = append(choices, tui.Choice{ID: "key", Title: "Set the key", Detail: s.GetPrompt()})
	case s.GetProvider() != "":
		choices = append(choices, tui.Choice{ID: "provider", Title: "Connect it", Detail: "Its key or sign-in, in /providers"})
	}
	return p.kit.Pick(p.hub, "Use "+s.GetTitle(), "use", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "model":
			return p.models(s)
		case "key":
			return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Edit, profile.Join(p.profile, s.GetKey())))
		case "provider":
			return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(providerapp.Screen, ""))
		}
		return p.choose(s, "")
	})
}

// Chooses the service with the model, its default for none, and asks for its key when it has none yet
func (p *picker) choose(s *generatev1.Service, model string) tea.Cmd {
	note := s.GetTitle() + " now makes the " + p.kind.noun
	if model != "" {
		note += " with " + model
	}
	done := p.write(note, write{p.kind.provider, s.GetName()}, write{cmp.Or(s.GetModelSetting(), p.kind.model), model})
	if !s.GetReady() && s.GetKey() != "" {
		return tea.Batch(done, p.kit.Open(configapp.Edit, profile.Join(p.profile, s.GetKey())))
	}
	return done
}

// Lists the models of the service to pick one from
func (p *picker) models(s *generatev1.Service) tea.Cmd {
	return tui.Call(p.hub, func() (*generatev1.ModelsResponse, error) {
		return p.service.Models(p.kit.Env.Ctx, &generatev1.ModelsRequest{Profile: p.profile, Kind: p.kind.proto, Service: s.GetName()})
	}, func(response *generatev1.ModelsResponse) tea.Cmd {
		var choices []tui.Choice
		for _, m := range response.GetModels() {
			detail := strings.Join(filled(m.GetStrengths(), m.GetSpeed(), m.GetPrice()), "  ")
			title := cmp.Or(m.GetTitle(), m.GetId())
			if m.GetId() == response.GetDefault() {
				title += " (default)"
			}
			choices = append(choices, tui.Choice{ID: m.GetId(), Title: title, Detail: detail})
		}
		if len(choices) == 0 {
			return p.hub.Fail(s.GetTitle() + " lists no models now; its default is used")
		}
		return p.kit.Pick(p.hub, "Model of "+s.GetTitle(), "use", choices, s.GetModel(), func(choice tui.Choice) tea.Cmd {
			return p.choose(s, choice.ID)
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

// Leaves the choice to what is set up; for videos, whether video_generate is offered then
func (p *picker) automatic() tea.Cmd {
	if p.kind.id == Images {
		return p.write("Pictures go to the first service with a key", write{p.kind.provider, ""}, write{p.kind.model, ""})
	}
	choices := []tui.Choice{{ID: "on", Title: "Automatic", Detail: "Videos go to the first available service"},
		{ID: "off", Title: "Off", Detail: "video_generate is not offered"}}
	return p.kit.Pick(p.hub, "Video generation", "use", choices, "", func(choice tui.Choice) tea.Cmd {
		if choice.ID == "off" {
			return p.write("Video generation is off", write{p.kind.provider, ""}, write{generationconfig.VideoEnabled, false})
		}
		return p.write("Videos go to the first available service", write{p.kind.provider, ""}, write{p.kind.model, ""}, write{generationconfig.VideoEnabled, true})
	})
}

// Opens the settings of the service
func (p *picker) configure(s *generatev1.Service) tea.Cmd {
	settings := s.GetSettings()
	if len(settings) == 0 {
		return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, p.kind.provider[:strings.Index(p.kind.provider, ".")])))
	}
	section := settings[0]
	if i := strings.LastIndex(section, "."); i > 0 {
		section = section[:i]
	}
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, section)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, then reads the services again
func (p *picker) write(note string, writes ...write) tea.Cmd {
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
		return tea.Batch(p.hub.Done(note), p.load())
	})
}

// Title of the screen, naming the profile when it was opened on one
func (p *picker) title() string {
	if p.profile == "" {
		return p.kind.title
	}
	return p.kind.title + " of " + p.profile
}
