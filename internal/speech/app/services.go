package app

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	packagesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// The services of one part of Voice: speech to text or text to speech
type kind struct {
	part, title, about string
	// Setting that chooses the service in use, and the switch of the part, empty for none
	choice, enabled string
	// What a service in use does, after its name
	does string
	// Table of the programs of yours, and what a program does, after "A command of your own that"
	table, program string
	kind           speechv1.Kind
	// Lists Automatic first
	auto bool
	// Settings of the part shown elsewhere than its options
	leave []string
}

var (
	recognizers = kind{part: speechconfig.STT, title: "Speech to text", about: "Which service turns what you say into text.",
		choice: speechconfig.STTProvider, enabled: speechconfig.STTEnabled, does: "turns what you say into text", table: speechconfig.STTCommands,
		program: "turns a recording into text", kind: speechv1.Kind_KIND_RECOGNIZER, auto: true,
		leave: []string{speechconfig.STTEnabled, speechconfig.LocalCommand}}
	voices = kind{part: speechconfig.TTS, title: "Text to speech", about: "The voice that reads replies aloud.", choice: speechconfig.TTSProvider,
		does: "reads replies aloud", table: speechconfig.TTSCommands, program: "reads text aloud", kind: speechv1.Kind_KIND_VOICE,
		leave: []string{speechconfig.PluginVoice, speechconfig.PluginModel, speechconfig.OutputFormat}}
)

// Settings of the part a service's page shows besides those of its own section, by service
var ownSettings = map[string][]string{"local_command": {speechconfig.LocalCommand}}

// Settings of every program of yours that reads text aloud, shown on the page of each
var everyProgram = []string{speechconfig.PluginVoice, speechconfig.PluginModel, speechconfig.OutputFormat}

// Row of the page of a part: its switch, Automatic, a service, the row that adds a program, an option, or the row of the advanced options
type serviceRow struct {
	setting  *configv1.Setting
	service  *speechv1.Service
	auto     bool
	add      bool
	advanced bool
}

// Services of the part as last read
func (p *page) offered(k kind) []*speechv1.Service {
	if k.auto {
		return p.services.GetRecognizers()
	}
	return p.services.GetVoices()
}

// Service of the part with the name as last read, nil when it is gone
func (p *page) find(k kind, name string) *speechv1.Service {
	for _, s := range p.offered(k) {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// Service the setting of the part chooses, empty for Automatic
func (p *page) chosen(k kind) string {
	if k.auto {
		return p.services.GetRecognizer()
	}
	return p.services.GetVoice()
}

// What keeps the part from working, nil while nothing does
func (p *page) hindrance(k kind) *speechv1.Hindrance {
	if k.auto {
		return p.voice.GetRecognition().GetHindrance()
	}
	return p.voice.GetSpeaking().GetHindrance()
}

// What fixes a hindrance, as the page of the part says it: the page of the service, the switch, or a service below
func fixHere(h *speechv1.Hindrance) string {
	switch h.GetLack() {
	case speechv1.Lack_LACK_INSTALL:
		return "Its page installs it."
	case speechv1.Lack_LACK_KEY:
		return "Its page takes its API key."
	case speechv1.Lack_LACK_SIGN_IN:
		return "Its page signs you in."
	case speechv1.Lack_LACK_PROGRAM:
		return "Install it with " + h.GetInstall() + "."
	case speechv1.Lack_LACK_BUILD:
		return "Pick another service."
	case speechv1.Lack_LACK_SETUP:
		return "Its page sets it up."
	case speechv1.Lack_LACK_SWITCH:
		return "Voice input needs it on."
	case speechv1.Lack_LACK_SERVICE:
		return "Pick a service below."
	}
	return ""
}

// Options of the part: those of its section but its switch and what its services' pages show, the advanced ones in place while they are few
func (p *page) partOptions(k kind) (shown, behind []*configv1.Setting) {
	return p.section(k.part, k.leave...)
}

// Heading a service is listed under: on this computer, online, or among your programs
func heading(s *speechv1.Service) string {
	switch {
	case s.GetProgram():
		return "Your programs"
	case s.GetLocal():
		return "On this computer"
	}
	return "Online"
}

// Rows of the page of a part: its switch, Automatic, the services under their headings, the row that adds a program, its options and the
// row of its advanced ones
func (p *page) partRows(k kind) []serviceRow {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []serviceRow
	if setting := p.options.Setting(k.enabled); setting != nil {
		rows = append(rows, serviceRow{setting: setting})
	}
	if k.auto {
		rows = append(rows, serviceRow{auto: true})
	}
	services := slices.Clone(p.offered(k))
	order := map[string]int{"On this computer": 0, "Online": 1, "Your programs": 2}
	slices.SortStableFunc(services, func(a, b *speechv1.Service) int { return order[heading(a)] - order[heading(b)] })
	for _, s := range services {
		rows = append(rows, serviceRow{service: s})
	}
	rows = append(rows, serviceRow{add: true})
	shown, behind := p.partOptions(k)
	for _, setting := range shown {
		rows = append(rows, serviceRow{setting: setting})
	}
	if len(behind) > 0 {
		rows = append(rows, serviceRow{advanced: true})
	}
	return rows
}

// Opens the page of a part over the first view
func (p *page) servicesPage(k kind) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []serviceRow
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   k.title,
		Glyph:   t.Glyphs.Session,
		Key:     "voice:" + k.part,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{k.about}
			if h := p.hindrance(k); p.loaded && h != nil {
				lines = append(lines, render.Warm(strs.Full(h.GetCause())+" "+fixHere(h), t))
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.partRows(k)
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = p.serviceCells(k, row)
			}
			return cells
		},
		Group: func(i int) string {
			switch row := shown[i]; {
			case row.service != nil:
				return heading(row.service)
			case row.add:
				return "Your programs"
			case row.auto || row.setting != nil && row.setting.GetKey() == k.enabled:
				return ""
			}
			return "Options"
		},
		Words: func(i int) string {
			if s := shown[i].service; s != nil {
				return s.GetName() + " " + s.GetAbout() + " " + s.GetSetup().GetBadge()
			}
			return ""
		},
		Detail: func(i int) []string { return p.serviceDetail(k, shown[i], list.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return []wm.Hint{{Action: "hub.add", Label: "add a program"}}
			}
			return p.serviceHints(k, shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.setting != nil:
					return p.options.Change(row.setting)
				case row.auto:
					if p.chosen(k) == "" {
						return nil
					}
					return p.write(k.title+" now uses the first service that is ready.", map[string]any{k.choice: nil})
				case row.service != nil && p.usable(k, row.service):
					return p.use(k, row.service)
				case row.service != nil:
					return p.servicePage(k, row.service.GetName())
				case row.add:
					return p.addProgram(k, list)
				}
				return p.advanced(k.part, k.leave...)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if s := shown[i].service; s != nil {
					return p.servicePage(k, s.GetName())
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return p.addProgram(k, list) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.setting != nil:
					return p.options.Reset(l, row.setting)
				case row.service.GetProgram():
					return p.removeProgram(k, l, row.service.GetName())
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	p.watch(list)
	return p.hub.Push(list)
}

// Whether enter on a service uses it: it is ready and not the one the setting chooses
func (p *page) usable(k kind, s *speechv1.Service) bool {
	return s.GetReady() && s.GetName() != p.chosen(k)
}

// Chooses the service, which does the work of the part from now on
func (p *page) use(k kind, s *speechv1.Service) tea.Cmd {
	return p.write(s.GetTitle()+" now "+k.does+".", map[string]any{k.choice: s.GetName()})
}

func (p *page) serviceCells(k kind, row serviceRow) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.setting != nil:
		return p.options.Cells(row.setting)
	case row.auto:
		chosen := p.chosen(k) == ""
		mark := p.kit.Mark(true, chosen)
		auto := p.find(k, p.services.GetAutomatic())
		switch {
		case auto == nil:
			return []string{"Automatic", mark, render.Warm("None is ready", t)}
		case chosen:
			return []string{"Automatic", mark, render.Good("In use", t) + " · " + auto.GetTitle()}
		}
		return []string{"Automatic", mark, auto.GetTitle()}
	case row.service != nil:
		s := row.service
		return []string{s.GetTitle(), p.kit.Mark(true, s.GetName() == p.chosen(k)), p.stateWords(s.GetState(), s.GetHindrance() != nil)}
	case row.add:
		return []string{"Add a program", opens, ""}
	}
	return p.advancedRow(k.part, k.leave...)
}

// Lines under the rows about the row in hand: what a service is and, where its state does not say all, what it lacks in its own words
func (p *page) serviceDetail(k kind, row serviceRow, cut bool) []string {
	switch {
	case row.setting != nil:
		return p.options.Detail(row.setting, cut)
	case row.auto:
		return []string{"The first service that is ready, those on this computer first."}
	case row.add:
		return []string{"A command of your own that " + k.program + ", such as a whisper program."}
	case row.advanced:
		return []string{"Settings of " + strings.ToLower(k.title) + " that few people change."}
	}
	s := row.service
	lines := []string{s.GetAbout() + "."}
	if badge := s.GetSetup().GetBadge(); badge != "" {
		lines[0] = s.GetAbout() + ", " + strings.TrimPrefix(strings.TrimPrefix(badge, "recommended · "), "★ ") + "."
	}
	switch s.GetHindrance().GetLack() {
	case speechv1.Lack_LACK_BUILD, speechv1.Lack_LACK_SETUP:
		lines = append(lines, render.Warm(strs.Full(s.GetMissing()), p.kit.Env.Theme))
	}
	return lines
}

func (p *page) serviceHints(k kind, row serviceRow) []wm.Hint {
	add := wm.Hint{Action: "hub.add", Label: "add a program"}
	switch {
	case row.setting != nil:
		return p.options.Hints(row.setting)
	case row.auto && p.chosen(k) != "":
		return []wm.Hint{{Action: "hub.use", Label: "use"}, add}
	case row.auto:
		return []wm.Hint{add}
	case row.service != nil:
		hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
		if p.usable(k, row.service) {
			hints = []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "open"}}
		}
		if row.service.GetProgram() {
			hints = append(hints, wm.Hint{Action: "hub.remove", Label: "remove"})
		}
		return append(hints, add)
	case row.add:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}}
}

// Something to do on the page of a service
type step struct {
	title, value string
	verb         string
	setting      *configv1.Setting
	// Opens a page: the advanced options of the service, or the provider it signs in at
	opens, advanced bool
	// Does what keeps a listener from listening, which the Listener row of the page of the wake word does in its place
	fixes bool
	run   func() tea.Cmd
}

// Rows of the page of a service as it stands now: using it, installing it, the values picked from its own lists, its API key, its sign-in,
// then its options
func (p *page) serviceSteps(k kind, name string) []step {
	t := p.kit.Env.Theme
	s := p.find(k, name)
	if s == nil {
		return nil
	}
	setup := s.GetSetup()
	var steps []step
	if name != p.chosen(k) {
		steps = append(steps, step{title: "Use it", verb: "use", run: func() tea.Cmd { return p.use(k, s) }})
	}
	if s.GetHindrance().GetLack() == speechv1.Lack_LACK_INSTALL {
		steps = append(steps, step{title: "Install", value: render.Warm(speech.NotInstalled, t), verb: "install",
			run: func() tea.Cmd { return p.install(s.GetTitle(), setup.GetExtra()) }})
	}
	picked := map[string]bool{}
	for _, c := range s.GetChoices() {
		picked[c.GetSetting()] = true
		value := c.GetValue()
		if setting := p.options.Setting(c.GetSetting()); setting != nil {
			value = setting.GetShown()
		}
		steps = append(steps, step{title: cmp.Or(c.GetTitle(), "Model"), value: value, verb: "change", run: func() tea.Cmd { return p.pickValue(k, s, c) }})
	}
	if keys := setup.GetKeys(); len(keys) > 0 {
		value := render.Warm("Not set", t)
		if len(setup.GetSet()) > 0 {
			value = "Set"
		}
		steps = append(steps, step{title: "API key", value: value, verb: "change", run: func() tea.Cmd { return p.askKey(s, keys[0], setup.GetLink()) }})
	}
	if setup.GetSignIn() != "" {
		steps = append(steps, step{title: "Sign in", opens: true, verb: "open", run: func() tea.Cmd { return p.signIn(setup) }})
	}
	// Its options: those of its own section alone and its keys outside it, but those picked from its lists above, read again each time
	leave := slices.Sorted(maps.Keys(picked))
	options := func() (everyday, advanced []*configv1.Setting) {
		if section := s.GetSection(); section != "" {
			everyday, advanced = p.options.Own(section, leave...)
		}
		own := slices.DeleteFunc(p.settingsOf(ownSettings[name]...), func(setting *configv1.Setting) bool { return picked[setting.GetKey()] })
		return append(everyday, own...), advanced
	}
	everyday, advanced := options()
	rare := func() []*configv1.Setting {
		_, advanced := options()
		return advanced
	}
	// The advanced ones behind a row while they are many, else in place, last
	cells, open, apart := p.options.AdvancedOf("voice:"+k.part+":"+name+":advanced", s.GetTitle(), everyday, rare)
	if !apart {
		everyday = append(everyday, advanced...)
	}
	for _, setting := range everyday {
		steps = append(steps, step{setting: setting})
	}
	if apart {
		steps = append(steps, step{title: "Advanced", value: cells[2], verb: "open", opens: true, advanced: true, run: open})
	}
	return steps
}

// Opens the page of a service over the page of its part: a program of yours has its own
func (p *page) servicePage(k kind, name string) tea.Cmd {
	s := p.find(k, name)
	if s == nil {
		return nil
	}
	if s.GetProgram() {
		return p.programPage(k, name)
	}
	t := p.kit.Env.Theme
	var steps []step
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   s.GetTitle(),
		Glyph:   t.Glyphs.Session,
		Key:     "voice:" + k.part + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.serviceState(k, name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			steps = p.serviceSteps(k, name)
			cells := make([][]string, len(steps))
			for i, st := range steps {
				cells[i] = p.stepCells(st)
			}
			return cells
		},
		Group: func(i int) string {
			if steps[i].setting != nil || steps[i].advanced {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			switch st := steps[i]; {
			case st.setting != nil:
				return p.options.Detail(st.setting, view.Cut(i, 2))
			case st.advanced:
				return []string{"Settings of " + s.GetTitle() + " that few people change."}
			}
			return p.stepDetail(k, s, steps[i])
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, s.GetTitle()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(steps) {
				return nil
			}
			if st := steps[i]; st.setting != nil {
				return p.options.Hints(st.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: steps[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if st := steps[i]; st.setting != nil {
					return p.options.Change(st.setting)
				}
				return steps[i].run()
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if st := steps[i]; st.setting != nil {
					return p.options.Reset(l, st.setting)
				}
				return nil
			},
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

// Cells of a row of the page of a service or a listener: an option as Settings draws it, else what it does, the mark of a row that opens a
// page and what it holds
func (p *page) stepCells(st step) []string {
	t := p.kit.Env.Theme
	if st.setting != nil {
		return p.options.Cells(st.setting)
	}
	mark := ""
	if st.opens {
		mark = render.Dim(t.Glyphs.Forward, t)
	}
	return []string{st.title, mark, st.value}
}

// First line of the page of a service: how it stands, as a sentence
func (p *page) serviceState(k kind, name string) []string {
	s := p.find(k, name)
	t := p.kit.Env.Theme
	switch {
	case s == nil:
		return nil
	case s.GetHindrance() != nil:
		return []string{render.Warm(strs.Full(s.GetHindrance().GetCause()), t)}
	case s.GetInUse():
		return []string{"It " + k.does + " now."}
	case s.GetState() == speech.Free:
		return []string{"Ready to use, and free."}
	}
	return []string{"Ready to use."}
}

// What a row of the page of a service does, said under the rows
func (p *page) stepDetail(k kind, s *speechv1.Service, st step) []string {
	setup := s.GetSetup()
	switch st.title {
	case "Use it":
		return []string{"Have " + s.GetTitle() + " do the work of " + strings.ToLower(k.title) + " from now on."}
	case "Install":
		return []string{"Download " + s.GetTitle() + " into icebath's own Python on this computer, and show how it goes."}
	case "API key":
		line := "Kept in your keys file as " + setup.GetKeys()[0] + "."
		if link := setup.GetLink(); link != "" {
			line += " Get one at " + link + "."
		}
		return []string{line}
	case "Sign in":
		return []string{"Use your account at the provider in place of an API key."}
	}
	return []string{"Which " + strings.ToLower(st.title) + " " + s.GetTitle() + " uses."}
}

// Mark of the choice that types a value in place of picking one
const typeOwn = "\x00"

// Opens the provider whose sign-in serves the service in Providers, or Providers itself to add one while the profile has none
func (p *page) signIn(setup *speechv1.Setup) tea.Cmd {
	if name := setup.GetSignInProvider(); name != "" {
		return p.kit.Open(providerapp.Screen, profile.Join(p.profile, providerapp.LoginsOf+name))
	}
	return p.kit.Open(providerapp.Screen, profile.Join(p.profile, ""))
}

// Picks one value of a setting of the service from the service's own list, read as it opens, with a row that types one the list lacks
func (p *page) pickValue(k kind, s *speechv1.Service, c *speechv1.Choice) tea.Cmd {
	title := cmp.Or(c.GetTitle(), "Model")
	keep := func(value string) tea.Cmd {
		return p.write(title+" of "+s.GetTitle()+" is now "+value+".", map[string]any{c.GetSetting(): value})
	}
	p.hub.Busy("Reading the choices of " + strings.ToLower(title) + "…")
	return tui.Async(func() func() tea.Cmd {
		reply, err := p.service.Values(p.kit.Env.Ctx, &speechv1.ValuesRequest{Profile: p.profile, Kind: k.kind, Service: s.GetName(), Setting: c.GetSetting()})
		return func() tea.Cmd {
			p.hub.Quiet()
			var choices []tui.Choice
			for _, v := range reply.GetValues() {
				detail := v.GetAbout()
				if v.GetTitle() != "" && v.GetTitle() != v.GetValue() {
					detail = strings.TrimPrefix(v.GetValue()+" · "+detail, " · ")
				}
				choices = append(choices, tui.Choice{ID: v.GetValue(), Title: cmp.Or(v.GetTitle(), v.GetValue()), Detail: strings.TrimSuffix(detail, " · ")})
			}
			why := ""
			switch {
			case err != nil:
				why = strs.Full(tui.Reason(err))
			case reply.GetError() != "":
				why = "The list could not be read. " + strs.Full(reply.GetError())
			}
			choices = append(choices, tui.Choice{ID: typeOwn, Title: "Type a name", Detail: cmp.Or(why, "One this list does not have")})
			intro := []string{c.GetAbout()}
			return p.kit.PickFrom(p.hub, title, intro, "choose", choices, c.GetValue(), func(choice tui.Choice) tea.Cmd {
				if choice.ID != typeOwn {
					return keep(choice.ID)
				}
				field := cards.Field{Label: title, Value: c.GetValue(), Select: true, Help: "The name " + s.GetTitle() + " knows it by."}
				return p.kit.Ask(p.hub, title, field, "Save", keep)
			})
		}
	})
}

// Asks for the API key of the service and keeps it in the profile's keys file under its variable, the one place a key of speech is kept
func (p *page) askKey(s *speechv1.Service, variable, link string) tea.Cmd {
	help := "Kept in your keys file as " + variable + "."
	if link != "" {
		help = "Get one at " + link + ". " + help
	}
	return p.kit.Ask(p.hub, "API key", cards.Field{Label: "API key", Kind: cards.FieldSecret, Help: help}, "Save", func(text string) tea.Cmd {
		return p.putKey(s.GetTitle(), variable, text)
	})
}

// Keeps a key in the profile's keys file, then reads the page again
func (p *page) putKey(title, variable, text string) tea.Cmd {
	return tui.Call(p.hub, func() (*configv1.PutResponse, error) {
		return p.settings.Put(p.kit.Env.Ctx, &configv1.PutRequest{Key: variable, Value: text, Profile: p.profile})
	}, func(*configv1.PutResponse) tea.Cmd {
		return tea.Batch(p.hub.Done("Kept the key of "+title+" in your keys file."), p.load(nil))
	})
}

// How an install ran: why it failed as a sentence, empty when it worked; quiet when the app is closing and nothing more is said
type installRun func(ctx context.Context, line, status func(text string)) (failure string, quiet bool)

// Sentence of an install that failed with a reason
func notInstalled(title, reason string) string {
	return title + " was not installed. The error was: " + strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(reason), "✗ "), ".") + "."
}

// Installs a Python set of icebath on the computer the server runs on, where the voices and recognizers run, on a page that shows how it
// goes, carried on to its end when the page is closed; the page read again once it ends
func (p *page) install(title, extra string) tea.Cmd {
	return p.installPage(title, extra, nil, func(ctx context.Context, line, status func(text string)) (string, bool) {
		stream, err := p.packages.Install(ctx, &packagesv1.InstallRequest{Profile: p.profile, Extras: []string{extra}})
		if err != nil {
			return notInstalled(title, tui.Reason(err)), ctx.Err() != nil
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				return "The install stopped without saying whether it worked. The lines above say what it did.", false
			case err != nil:
				return notInstalled(title, tui.Reason(err)), ctx.Err() != nil
			}
			switch kind := event.GetKind().(type) {
			case *packagesv1.Event_Line:
				line(kind.Line)
			case *packagesv1.Event_Status:
				status(kind.Status)
			case *packagesv1.Event_Done:
				switch {
				case kind.Done.GetError() != "":
					return notInstalled(title, kind.Done.GetError()), false
				case kind.Done.GetExitCode() != 0:
					return title + " was not installed. The lines above say why.", false
				}
				return "", false
			}
		}
	})
}

// Shows an install on a page as it goes, carried on to its end when the page is closed: its lines, what it does now, then how it ended, which
// is the note of the page too. The page of Voice is read again once it ends, and then runs once it worked
func (p *page) installPage(title, extra string, then func() tea.Cmd, run installRun) tea.Cmd {
	var lines []string
	status := ""
	ended := false
	view := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  "Install",
		Key:    "voice:install:" + extra,
		Follow: true,
		Lines: func(int) []string {
			out := slices.Clone(lines)
			if status != "" {
				out = append(out, render.Dim(status, p.kit.Env.Theme))
			}
			if len(out) == 0 {
				out = append(out, render.Dim("Starting…", p.kit.Env.Theme))
			}
			return out
		},
	})
	finish := func(text string, failed bool) {
		p.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended, status = true, ""
			t := p.kit.Env.Theme
			if failed {
				lines = append(lines, "", render.Bad(text, t))
				return tea.Batch(p.hub.Fail(text), p.load(nil))
			}
			lines = append(lines, "", render.Good(text, t))
			cmds := []tea.Cmd{p.hub.Done(text), p.load(nil)}
			if then != nil {
				cmds = append(cmds, then())
			}
			return tea.Batch(cmds...)
		})
	}
	ctx := p.kit.Env.Ctx
	go func() {
		failure, quiet := run(ctx,
			func(text string) { p.kit.Apply(func() tea.Cmd { lines, status = append(lines, text), ""; return nil }) },
			func(text string) { p.kit.Apply(func() tea.Cmd { status = text; return nil }) })
		switch {
		case quiet:
		case failure != "":
			finish(failure, true)
		default:
			finish("Installed "+title+".", false)
		}
	}()
	return p.hub.Push(view)
}
