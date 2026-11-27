// Screens of speech: the voice that speaks for icebath and the service that turns speech into text, each picked with its key, sign in,
// install and the voice or model it uses
package app

import (
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/terminal"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screens of the voice and of recognition
const (
	Voices      = "speech"
	Recognizers = "recognition"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

// What each screen picks
type kind struct {
	id, title, short, noun, setting string
	voices                          bool
}

var kinds = []kind{
	{id: Voices, title: "Speech voice", short: "Pick the voice that reads replies aloud and speaks for text_to_speech, with its key and voice", noun: "speech",
		setting: speechconfig.TTSProvider, voices: true},
	{id: Recognizers, title: "Speech recognition", short: "Pick the service that turns your recordings and voice messages into text, with its key and model",
		noun: "recognition", setting: speechconfig.STTProvider},
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := speechv1connect.NewSpeechServiceClient(conn)
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
	service  speechv1connect.SpeechServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	state   *speechv1.ServicesReply
	loaded  bool
	err     error
}

func (p *picker) open(search string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   p.title(),
		Glyph:   t.Glyphs.Session,
		Key:     p.kind.id,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search services",
		Columns: []render.Column{{Max: 32}, {Title: "Runs"}, {Max: 24}, {Flex: 1}},
		Yield:   []int{1, 2},
		Rows: func() [][]string {
			services := p.services()
			rows := make([][]string, len(services))
			for i, s := range services {
				where := "cloud"
				if s.GetLocal() {
					where = "this machine"
				}
				rows[i] = []string{s.GetTitle(), render.Dim(where, t), render.Dim(s.GetSetup().GetBadge(), t), p.status(s)}
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
			return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "settings"}, {Action: "hub.reset", Label: "default"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.use(p.services()[i]) },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.configure(p.services()[i]) },
			"hub.reset":   func(*cards.List, int) tea.Cmd { return p.reset() },
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

func (p *picker) services() []*speechv1.Service {
	if p.kind.voices {
		return p.state.GetVoices()
	}
	return p.state.GetRecognizers()
}

// Service the setting names now
func (p *picker) chosen() string {
	if p.kind.voices {
		return p.state.GetVoice()
	}
	return p.state.GetRecognizer()
}

func (p *picker) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Services(p.kit.Env.Ctx, &speechv1.ServicesRequest{Profile: p.profile})
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
func (p *picker) status(s *speechv1.Service) string {
	t := p.kit.Env.Theme
	chosen := s.GetName() == p.chosen()
	switch {
	case chosen && s.GetReady():
		return render.Good("In use", t)
	case chosen:
		return render.Warm("Chosen, not set up", t)
	case s.GetReady():
		return "Set up"
	}
	return render.Dim("Not set up", t)
}

// What the service is, what it needs and what the profile's settings make of it
func (p *picker) describe(s *speechv1.Service) []string {
	t := p.kit.Env.Theme
	lines := []string{s.GetAbout()}
	if missing := s.GetMissing(); missing != "" {
		lines = append(lines, render.Warm(missing, t))
	}
	for _, c := range s.GetChoices() {
		if c.GetValue() != "" {
			lines = append(lines, settingTitle(c)+": "+c.GetValue())
		}
	}
	setup := s.GetSetup()
	if keys := setup.GetKeys(); len(keys) > 0 {
		state := "not set"
		if len(setup.GetSet()) > 0 {
			state = strings.Join(setup.GetSet(), ", ") + " set"
		}
		lines = append(lines, render.Dim("Key: "+strings.Join(keys, " or ")+" ("+state+")  "+setup.GetLink(), t))
	}
	if setup.GetSignIn() != "" {
		lines = append(lines, render.Dim("Or the "+setup.GetSignIn()+" sign-in connected in /providers", t))
	}
	switch {
	case setup.GetExtra() == "" || setup.GetInstalled():
	case setup.GetUnsupported() != "":
		lines = append(lines, render.Warm(p.unsupported(s), t))
	default:
		lines = append(lines, render.Dim("Runs on this machine once installed: "+setup.GetInstall(), t))
	}
	for _, program := range setup.GetAbsent() {
		lines = append(lines, render.Warm(s.GetTitle()+" also needs "+program.GetName()+" from your system: "+program.GetInstall(), t))
	}
	return append(lines, render.Dim(s.GetName(), t))
}

// Why a local service can't run here, with what to do instead
func (p *picker) unsupported(s *speechv1.Service) string {
	instead := "pick another service"
	if p.kind.voices {
		instead = "pick another voice"
	}
	return s.GetTitle() + " cannot run on this computer (" + s.GetSetup().GetUnsupported() + "); " + instead + "."
}

// Ways of using the service: choosing it, picking one of its voices or models, setting its key, connecting its provider or installing it
func (p *picker) use(s *speechv1.Service) tea.Cmd {
	setup := s.GetSetup()
	unsupported := setup.GetUnsupported() != "" && !setup.GetInstalled()
	use := tui.Choice{ID: "use", Title: "Use " + s.GetTitle(), Detail: "For " + p.kind.noun + ", with its settings as they are"}
	if unsupported {
		use.Detail = p.unsupported(s)
	}
	choices := []tui.Choice{use}
	for i, c := range s.GetChoices() {
		if len(c.GetValues()) > 0 {
			detail := c.GetAbout()
			if detail == "" {
				detail = "One of " + itemsOf(c)
			}
			choices = append(choices, tui.Choice{ID: "choice:" + strconv.Itoa(i), Title: "Pick " + settingTitle(c), Detail: detail})
		}
	}
	if len(setup.GetKeys()) > 0 {
		choices = append(choices, tui.Choice{ID: "key", Title: "Set the key", Detail: setup.GetKeys()[0] + "  " + setup.GetLink()})
	}
	// A sign in is offered while neither it nor a key serves the service
	if setup.GetSignIn() != "" && !s.GetReady() {
		choices = append(choices, tui.Choice{ID: "provider", Title: "Sign in to " + s.GetTitle(), Detail: "Opens its sign-in in /providers; or set the key instead"})
	}
	if setup.GetExtra() != "" && !setup.GetInstalled() && !unsupported {
		choices = append(choices, tui.Choice{ID: "install", Title: "Install " + s.GetTitle(), Detail: "Downloads " + s.GetTitle() +
			" and what it needs into icebath's own Python."})
	}
	return p.kit.Pick(p.hub, "Use "+s.GetTitle(), "use", choices, "", func(choice tui.Choice) tea.Cmd {
		switch {
		case strings.HasPrefix(choice.ID, "choice:"):
			i, _ := strconv.Atoi(strings.TrimPrefix(choice.ID, "choice:"))
			return p.pick(s, s.GetChoices()[i])
		case choice.ID == "key":
			return p.key(s, setup.GetKeys()[0], setup.GetLink())
		case choice.ID == "provider":
			return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+setup.GetSignIn())))
		case choice.ID == "install":
			return p.install(setup.GetExtra())
		}
		return p.write(s.GetTitle()+" now does the "+p.kind.noun, write{p.kind.setting, s.GetName()})
	})
}

// Asks for the key of the service and keeps it in the profile's .env under its variable, the service then chosen too
func (p *picker) key(s *speechv1.Service, variable, link string) tea.Cmd {
	title := variable + " for " + s.GetTitle()
	if link != "" {
		title += " (" + link + ")"
	}
	return p.kit.Ask(p.hub, title, cards.Field{Label: "key", Kind: cards.FieldSecret}, "Save", func(text string) tea.Cmd {
		text = strings.TrimSpace(text)
		if text == "" {
			return p.hub.Fail("No key was typed; " + variable + " is left as it was")
		}
		return tui.Call(p.hub, func() (*configv1.PutResponse, error) {
			return p.settings.Put(p.kit.Env.Ctx, &configv1.PutRequest{Key: variable, Value: text, Profile: p.profile})
		}, func(*configv1.PutResponse) tea.Cmd {
			return p.write(variable+" is set and "+s.GetTitle()+" now does the "+p.kind.noun, write{p.kind.setting, s.GetName()})
		})
	})
}

// Title of a choice's setting, else the last name of its key with its words spaced
func settingTitle(c *speechv1.Choice) string {
	if c.GetTitle() != "" {
		return c.GetTitle()
	}
	setting := c.GetSetting()
	return strings.ReplaceAll(setting[strings.LastIndex(setting, ".")+1:], "_", " ")
}

// A few of the values of a choice
func itemsOf(c *speechv1.Choice) string {
	values := c.GetValues()
	if len(values) > 4 {
		return strings.Join(values[:4], ", ") + " and more"
	}
	return strings.Join(values, ", ")
}

// Picks one value of a setting of the service, which is then chosen too
func (p *picker) pick(s *speechv1.Service, c *speechv1.Choice) tea.Cmd {
	choices := make([]tui.Choice, 0, len(c.GetValues()))
	for _, value := range c.GetValues() {
		choices = append(choices, tui.Choice{ID: value, Title: value})
	}
	return p.kit.Pick(p.hub, settingTitle(c)+" for "+s.GetTitle(), "use", choices, c.GetValue(), func(choice tui.Choice) tea.Cmd {
		return p.write(s.GetTitle()+" now does the "+p.kind.noun+" with "+choice.ID, write{p.kind.setting, s.GetName()}, write{c.GetSetting(), choice.ID})
	})
}

// Installs the Python set of a local engine in a terminal window, where its progress shows
func (p *picker) install(extra string) tea.Cmd {
	binary, err := os.Executable()
	if err != nil {
		binary = "icebath"
	}
	shell := p.kit.Terminal()
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.WM.OpenHere(shell), shell.Run(terminal.Join([]string{binary, "pm", "install", "--extra", extra})))
}

// Leaves the choice to the default: Edge for the voice, the first service set up for recognition
func (p *picker) reset() tea.Cmd {
	note := "Recognition goes to the first service set up"
	if p.kind.voices {
		note = "Edge's free voices speak"
	}
	return p.write(note, write{p.kind.setting, nil})
}

// Opens the settings of the service
func (p *picker) configure(s *speechv1.Service) tea.Cmd {
	section := speechconfig.STT + "." + s.GetName()
	if p.kind.voices {
		section = speechconfig.TTS + "." + s.GetName()
	}
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, section)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, then reads the services again; a nil value puts the default back
func (p *picker) write(note string, writes ...write) tea.Cmd {
	return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
		var response *configv1.SetResponse
		for _, w := range writes {
			if w.value == nil {
				if _, err := p.settings.Unset(p.kit.Env.Ctx, &configv1.UnsetRequest{Key: w.key, Profile: p.profile}); err != nil {
					return nil, err
				}
				response = &configv1.SetResponse{}
				continue
			}
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
