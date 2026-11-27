// Screens of speech: the voice that reads the agent's replies aloud and the service that turns what you say into text, each chosen here
// with its API key, sign-in, install and the voice or model it uses
package app

import (
	"os"
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

// Screens of the voice and of speech to text
const (
	Voices      = "speech"
	Recognizers = "recognition"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

// What each screen chooses
type kind struct {
	id, title, short, setting string
	// Line above the services saying what is chosen
	intro string
	// What a service does once chosen, after its name: "reads replies aloud"; and as an instruction: "Read replies aloud"
	does, do string
	// Section of the settings the services' own settings sit under
	section string
	voices  bool
}

var kinds = []kind{
	{id: Voices, title: "Text to speech", short: "Choose the voice that reads replies aloud", setting: speechconfig.TTSProvider,
		intro: "Choose the voice that reads the agent's replies aloud.", does: "reads replies aloud", do: "Read replies aloud", section: speechconfig.TTS, voices: true},
	{id: Recognizers, title: "Speech to text", short: "Choose which service turns what you say into text", setting: speechconfig.STTProvider,
		intro: "Choose the service that turns what you say into text.", does: "turns what you say into text", do: "Turn what you say into text", section: speechconfig.STT},
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := speechv1connect.NewSpeechServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	var out []tui.Screen
	// Speech to text before text to speech, as the settings list them, after voice mode and the wake word
	for i, k := range kinds {
		out = append(out, tui.Screen{
			ID:    k.id,
			Group: tui.GroupVoice,
			Order: 40 - 10*i,
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
	service  speechv1connect.SpeechServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose services are shown, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	// Lists open over the hub, read again once the services are
	views  []*cards.List
	state  *speechv1.ServicesReply
	loaded bool
	err    error
	rows   []row
}

// Row of the list of services: a service, or for speech to text the choice of none in particular
type row struct {
	service *speechv1.Service
	auto    bool
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
		Intro:   func() []string { return []string{p.kind.intro} },
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			p.rows = p.entries()
			cells := make([][]string, len(p.rows))
			for i, r := range p.rows {
				if r.auto {
					cells[i] = []string{p.kit.Mark(true, p.chosen() == ""), "Automatic", "", render.Dim("The first service that is ready, those on this computer first", t)}
					continue
				}
				cells[i] = []string{p.kit.Mark(true, r.service.GetName() == p.chosen()), r.service.GetTitle(), p.status(r.service),
					render.Dim(r.service.GetAbout(), t)}
			}
			return cells
		},
		Group: func(i int) string {
			switch r := p.rows[i]; {
			case r.auto:
				return ""
			case r.service.GetLocal():
				return "On this computer"
			}
			return "Online"
		},
		Words: func(i int) string {
			if s := p.rows[i].service; s != nil {
				return s.GetName() + " " + s.GetSetup().GetBadge()
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
					return p.choose(r)
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

// What enter does on a row: choosing it when it is ready and not the one in use, else setting it up; nothing on the automatic choice
// once it is in use
func (p *picker) verb(r row) string {
	switch {
	case r.auto && p.chosen() == "":
		return ""
	case r.auto:
		return "choose"
	case r.service.GetReady() && r.service.GetName() != p.chosen():
		return "choose"
	}
	return "set up"
}

// Rows of the list: the automatic choice of speech to text first, then the services, those on this computer first
func (p *picker) entries() []row {
	var rows []row
	if !p.kind.voices && p.state != nil {
		rows = append(rows, row{auto: true})
	}
	for _, s := range p.services() {
		rows = append(rows, row{service: s})
	}
	return rows
}

// What the list says while it has no rows
func (p *picker) empty() string {
	switch {
	case p.err != nil:
		return "Could not read the services. " + sentence(tui.Reason(p.err)) + " " + p.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !p.loaded:
		return "Reading the services…"
	}
	return "No " + strings.ToLower(p.kind.title) + " services are installed."
}

func (p *picker) services() []*speechv1.Service {
	if p.kind.voices {
		return p.state.GetVoices()
	}
	return p.state.GetRecognizers()
}

// Service of the name as last read, nil when it is gone
func (p *picker) find(name string) *speechv1.Service {
	for _, s := range p.services() {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// Service the setting names now, empty for speech to text left to the first service that is ready
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
			for _, view := range p.views {
				view.Refresh()
			}
			return nil
		}
	})
}

// Whether a badge says the service costs nothing
func free(badge string) bool {
	return strings.Contains(badge, "free") && !strings.Contains(badge, "tier")
}

// How the service stands in a few words: what it still needs, else whether it is ready and free
func (p *picker) status(s *speechv1.Service) string {
	t := p.kit.Env.Theme
	setup := s.GetSetup()
	switch {
	case setup.GetUnsupported() != "" && !setup.GetInstalled():
		return render.Bad("Can't run here", t)
	case setup.GetExtra() != "" && !setup.GetInstalled():
		return render.Warm("Not installed", t)
	case len(setup.GetAbsent()) > 0:
		var names []string
		for _, program := range setup.GetAbsent() {
			names = append(names, program.GetName())
		}
		return render.Warm("Needs "+strings.Join(names, " and "), t)
	case s.GetReady() && len(setup.GetKeys()) == 0 && setup.GetSignIn() == "" && free(setup.GetBadge()):
		return render.Good("Free", t)
	case s.GetReady():
		return render.Good("Ready", t)
	case len(setup.GetKeys()) > 0 && len(setup.GetSet()) == 0 && setup.GetSignIn() != "":
		return render.Warm("Needs an API key or sign-in", t)
	case len(setup.GetKeys()) > 0 && len(setup.GetSet()) == 0:
		return render.Warm("Needs an API key", t)
	}
	return render.Warm("Not ready", t)
}

// Lines under the list about a row: what the service is when the row can't show it, then what it needs, its voice or model, its cost, and
// how to get its key, sign in or install it
func (p *picker) describe(r row) []string {
	t := p.kit.Env.Theme
	if r.auto {
		if p.list.Shows(3) {
			return nil
		}
		return []string{"The first service that is ready, those on this computer first."}
	}
	s := r.service
	var lines []string
	if !p.list.Shows(3) {
		lines = append(lines, s.GetAbout())
	}
	if missing := s.GetMissing(); missing != "" {
		lines = append(lines, render.Warm(sentence(missing), t))
	}
	setup := s.GetSetup()
	var facts []string
	for _, c := range s.GetChoices() {
		if c.GetValue() != "" {
			facts = append(facts, settingTitle(c)+": "+c.GetValue())
		}
	}
	if badge := setup.GetBadge(); badge != "" {
		facts = append(facts, strings.ToUpper(badge[:1])+badge[1:])
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	if keys := setup.GetKeys(); len(keys) > 0 {
		key := "API key: kept as " + strings.Join(keys, " or ") + " in your keys file, not set yet."
		if set := setup.GetSet(); len(set) > 0 {
			key = "API key: set as " + strings.Join(set, " and ") + "."
		}
		if link := setup.GetLink(); link != "" && len(setup.GetSet()) == 0 {
			key += " Get one at " + link
		}
		lines = append(lines, render.Dim(key, t))
	}
	if signIn := setup.GetSignIn(); signIn != "" && !s.GetReady() {
		lines = append(lines, render.Dim("Or sign in to "+s.GetTitle()+" on the Providers screen.", t))
	}
	switch {
	case setup.GetExtra() == "" || setup.GetInstalled():
	case setup.GetUnsupported() != "":
		lines = append(lines, render.Warm(p.unsupported(s), t))
	default:
		lines = append(lines, render.Dim("Runs on this computer once installed. Set up installs it.", t))
	}
	for _, program := range setup.GetAbsent() {
		lines = append(lines, render.Warm(s.GetTitle()+" also needs "+program.GetName()+" from your system. Install it with: "+program.GetInstall(), t))
	}
	return lines
}

// Why a service on this computer can't run here, with what to do instead
func (p *picker) unsupported(s *speechv1.Service) string {
	return s.GetTitle() + " can't run on this computer. " + sentence(s.GetSetup().GetUnsupported()) + " Choose another service."
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

// Chooses the row: a service, which then does the work, or the automatic choice
func (p *picker) choose(r row) tea.Cmd {
	if r.auto {
		return p.write("Speech to text now uses the first service that is ready.", write{p.kind.setting, nil})
	}
	return p.write(r.service.GetTitle()+" now "+p.kind.does+".", write{p.kind.setting, r.service.GetName()})
}

// Something to do in the setup of a service
type step struct {
	title, state, about string
	// Word on the button that does it
	verb string
	run  func() tea.Cmd
}

// Steps of the setup of the service as it stands now: choosing it, each of its voices or models, its API key, its sign-in, its install
// and its settings
func (p *picker) steps(name string) []step {
	t := p.kit.Env.Theme
	s := p.find(name)
	if s == nil {
		return nil
	}
	setup := s.GetSetup()
	unsupported := setup.GetUnsupported() != "" && !setup.GetInstalled()
	use := step{title: "Use " + s.GetTitle(), about: p.kind.do + " with " + s.GetTitle() + ", as it is set up now", verb: "choose",
		run: func() tea.Cmd { return p.choose(row{service: p.find(name)}) }}
	switch {
	case s.GetName() == p.chosen():
		use.state, use.verb, use.run = render.Good("In use", t), "", nil
	case unsupported:
		use.about = p.unsupported(s)
	}
	steps := []step{use}
	for i, c := range s.GetChoices() {
		if len(c.GetValues()) == 0 {
			continue
		}
		about := strings.TrimSuffix(c.GetAbout(), ".")
		if about == "" {
			about = "One of " + itemsOf(c)
		}
		value := c.GetValue()
		if value == "" {
			value = render.Dim("Default", t)
		}
		steps = append(steps, step{title: settingTitle(c), state: value, about: about, verb: "change",
			run: func() tea.Cmd { return p.pick(name, i) }})
	}
	if keys := setup.GetKeys(); len(keys) > 0 {
		state := render.Warm("Not set", t)
		if len(setup.GetSet()) > 0 {
			state = render.Good("Set", t)
		}
		about := "Kept in your keys file as " + keys[0]
		if link := setup.GetLink(); link != "" {
			about = "Get one at " + link
		}
		steps = append(steps, step{title: "API key", state: state, about: about, verb: "change",
			run: func() tea.Cmd { return p.key(name, keys[0], setup.GetLink()) }})
	}
	if signIn := setup.GetSignIn(); signIn != "" && !s.GetReady() {
		steps = append(steps, step{title: "Sign in", about: "Sign in on the Providers screen instead of an API key", verb: "sign in",
			run: func() tea.Cmd {
				return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+signIn)))
			}})
	}
	if setup.GetExtra() != "" && !setup.GetInstalled() && !unsupported {
		steps = append(steps, step{title: "Install", state: render.Warm("Not installed", t),
			about: "Download " + s.GetTitle() + " into icebath's own Python, in a terminal window", verb: "install",
			run: func() tea.Cmd { return p.install(setup.GetExtra()) }})
	}
	return append(steps, step{title: "All settings", about: "Open every setting of " + s.GetTitle(), verb: "open",
		run: func() tea.Cmd { return p.configure(name) }})
}

// Text with a capital first letter
func capitalize(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// Opens the setup of the service over the list: choosing it, its voices or models, its API key, its sign-in, its install and its settings
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
		Glyph:   t.Glyphs.Session,
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

// Asks for the API key of the service and keeps it in the profile's keys file under its variable, the service then chosen too
func (p *picker) key(name, variable, link string) tea.Cmd {
	s := p.find(name)
	if s == nil {
		return nil
	}
	help := "Kept in your keys file as " + variable + "."
	if link != "" {
		help = "Get one at " + link + "\n" + help
	}
	return p.kit.Ask(p.hub, "API key of "+s.GetTitle(), cards.Field{Label: "API key", Kind: cards.FieldSecret, Help: help}, "Save", func(text string) tea.Cmd {
		return tui.Call(p.hub, func() (*configv1.PutResponse, error) {
			return p.settings.Put(p.kit.Env.Ctx, &configv1.PutRequest{Key: variable, Value: text, Profile: p.profile})
		}, func(*configv1.PutResponse) tea.Cmd {
			return p.write("Saved the API key of "+s.GetTitle()+". "+s.GetTitle()+" now "+p.kind.does+".", write{p.kind.setting, name})
		})
	})
}

// Title of a choice's setting, else the last name of its key with its words spaced
func settingTitle(c *speechv1.Choice) string {
	if c.GetTitle() != "" {
		return c.GetTitle()
	}
	setting := c.GetSetting()
	return capitalize(strings.ReplaceAll(setting[strings.LastIndex(setting, ".")+1:], "_", " "))
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
func (p *picker) pick(name string, at int) tea.Cmd {
	s := p.find(name)
	if s == nil || at >= len(s.GetChoices()) {
		return nil
	}
	c := s.GetChoices()[at]
	choices := make([]tui.Choice, 0, len(c.GetValues()))
	for _, value := range c.GetValues() {
		choices = append(choices, tui.Choice{ID: value, Title: value})
	}
	return p.kit.Pick(p.hub, settingTitle(c)+" of "+s.GetTitle(), "choose", choices, c.GetValue(), func(choice tui.Choice) tea.Cmd {
		return p.write(settingTitle(c)+" is now "+choice.ID+", and "+s.GetTitle()+" "+p.kind.does+".",
			write{p.kind.setting, name}, write{c.GetSetting(), choice.ID})
	})
}

// Installs the Python set of a service on this computer in a terminal window, where its progress shows
func (p *picker) install(extra string) tea.Cmd {
	binary, err := os.Executable()
	if err != nil {
		binary = "icebath"
	}
	shell := p.kit.Terminal()
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.WM.OpenHere(shell), shell.Run(terminal.Join([]string{binary, "pm", "install", "--extra", extra})))
}

// Opens the settings of the service in place of this screen
func (p *picker) configure(name string) tea.Cmd {
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, p.kind.section+"."+name)))
}

type write struct {
	key   string
	value any
}

// Saves the settings in order, a nil value putting the default back, tells the screens that follow them, then reads the services again
func (p *picker) write(note string, writes ...write) tea.Cmd {
	type saved struct{ key, value string }
	return tui.Call(p.hub, func() ([]saved, error) {
		ctx := p.kit.Env.Ctx
		for _, w := range writes {
			if w.value == nil {
				if _, err := p.settings.Unset(ctx, &configv1.UnsetRequest{Key: w.key, Profile: p.profile}); err != nil {
					return nil, err
				}
				continue
			}
			value, err := yaml.Marshal(w.value)
			if err != nil {
				return nil, err
			}
			if _, err := p.settings.Set(ctx, &configv1.SetRequest{Key: w.key, Value: string(value), Profile: p.profile}); err != nil {
				return nil, err
			}
		}
		var out []saved
		for _, w := range writes {
			if got, err := p.settings.Get(ctx, &configv1.GetRequest{Key: w.key, Profile: p.profile}); err == nil {
				out = append(out, saved{w.key, got.GetSetting().GetValue()})
			}
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
