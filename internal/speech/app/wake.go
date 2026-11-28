package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/proto"
)

// Row of the page of the wake word: an option, its phrase, its listener, Speech to text while the wake word waits for it, or the row of its
// advanced options
type wakeRow struct {
	setting     *configv1.Setting
	phrase      bool
	listener    bool
	recognition bool
	advanced    bool
}

// Options of the wake word: those of its section but its switch, the advanced ones in place while they are few
func (p *page) wakeOptions() (shown, behind []*configv1.Setting) {
	return p.section(speechconfig.Wake, speechconfig.WakeEnabled)
}

// Rows of the page of the wake word: its switch, its phrase, its listener, Speech to text while that is the step the wake word misses, then
// its options. The row that does a missing step is always on the page
func (p *page) wakeRows() []wakeRow {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []wakeRow
	for _, setting := range p.settingsOf(speechconfig.WakeEnabled) {
		rows = append(rows, wakeRow{setting: setting})
	}
	rows = append(rows, wakeRow{phrase: true}, wakeRow{listener: true})
	if p.wake.Row == voice.AtRecognition {
		rows = append(rows, wakeRow{recognition: true})
	}
	shown, behind := p.wakeOptions()
	for _, setting := range shown {
		rows = append(rows, wakeRow{setting: setting})
	}
	if len(behind) > 0 {
		rows = append(rows, wakeRow{advanced: true})
	}
	return rows
}

// What keeps the wake word from listening in this app, as one warm line: what it can't do here, the place it listens in, a microphone that
// gives only silence; empty while nothing does
func (p *page) wakeLine() string {
	w := p.voice.GetWake()
	switch {
	case !p.wake.Available:
		return p.wake.Line()
	case w.GetEnabled() && elsewhere(w):
		place := p.options.Setting(speechconfig.WakeSurface).GetShown()
		return "Listens in is set to " + place + ", so this app does not listen. Set it to This app or Anywhere to listen here."
	case p.live != nil && p.live.Listening && p.live.Silent:
		return "The microphone gives only silence. " + p.live.Hint
	case p.live != nil && p.live.Device.Error != "" && w.GetEnabled():
		return p.live.Hint
	}
	return ""
}

// Opens the page of the wake word over the first view
func (p *page) wakePage() tea.Cmd {
	t := p.kit.Env.Theme
	var shown []wakeRow
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Wake word",
		Glyph:   t.Glyphs.Session,
		Key:     "voice:" + speechconfig.Wake,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"Say a phrase to start talking, hands-free."}
			if line := p.wakeLine(); p.loaded && p.err == nil && line != "" {
				lines = append(lines, render.Warm(line, t))
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.wakeRows()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = p.wakeCells(row)
			}
			return cells
		},
		Detail: func(i int) []string {
			switch row := shown[i]; {
			case row.setting != nil:
				return p.options.Detail(row.setting, list.Cut(i, 2))
			case row.phrase:
				return []string{"The words you say to start a recording."}
			case row.listener:
				return []string{"Which listener hears the wake phrase."}
			case row.recognition:
				return []string{partAbout[speechconfig.STT]}
			}
			return []string{"Settings of the wake word that few people change."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch row := shown[i]; {
			case row.setting != nil:
				return p.options.Hints(row.setting)
			case row.phrase:
				hints := []wm.Hint{{Action: "hub.use", Label: "change"}}
				if p.ownPhrase() != "" {
					hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
				}
				return hints
			case row.listener:
				if st := p.listenerFix(); st != nil {
					return []wm.Hint{{Action: "hub.use", Label: st.verb}, {Action: "hub.edit", Label: "open"}}
				}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.setting.GetKey() == speechconfig.WakeEnabled && !p.voice.GetWake().GetEnabled() && !p.wake.Available:
					return p.landWake(list)
				case row.setting != nil:
					return p.options.Change(row.setting)
				case row.phrase:
					return p.askPhrase()
				case row.listener:
					if st := p.listenerFix(); st != nil {
						return st.run()
					}
					return p.listenersPage("")
				case row.recognition:
					return p.openPart(speechconfig.STT)
				}
				return p.advanced(speechconfig.Wake, speechconfig.WakeEnabled)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if shown[i].listener {
					return p.listenersPage("")
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.setting != nil:
					return p.options.Reset(l, row.setting)
				case row.phrase && p.ownPhrase() != "":
					return p.write("The wake phrase is back to its default.", map[string]any{speechconfig.WakePhrase: nil})
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	p.watch(list)
	p.wakeList = list
	return p.hub.Push(list)
}

// Turning the wake word on with a step missing: nothing turns on, the cursor lands on the row that does the step, and the note says what to
// do there first, or, while the switch is on already, that the wake word listens once it is done. The switch and /wake on both come here,
// off the one answer of whether the wake word can listen
func (p *page) landWake(list *cards.List) tea.Cmd {
	if list == nil || p.wake.Available {
		return nil
	}
	// A page just opened has not read its rows yet
	list.Refresh()
	for i, row := range p.wakeRows() {
		switch {
		case p.wake.Row == voice.AtListener && row.listener, p.wake.Row == voice.AtRecognition && row.recognition,
			p.wake.Row == voice.AtMicrophone && row.setting.GetKey() == speechconfig.WakeInput:
			list.Select(i)
		}
	}
	if p.voice.GetWake().GetEnabled() {
		return p.hub.Say(p.wake.First+", and the wake word starts listening.", cards.NoteInfo)
	}
	return p.hub.Say(p.wake.First+" first, then turn on Listen hands-free.", cards.NoteInfo)
}

// The step that fixes what keeps the listener in effect from listening, picked by name or by Automatic alike, which its row on the page
// of the wake word does: installing it, giving it its access key or its model file, or picking another; nil while it can listen
func (p *page) listenerFix() *step {
	w := p.voice.GetWake()
	e, known := voice.Listener(p.sp, w)
	if !known {
		return &step{verb: "pick", run: func() tea.Cmd { return p.listenersPage("") }}
	}
	switch voice.EngineStanding(p.sp, w, e).State {
	case speech.NotInstalled, voice.NeedsAccessKey, voice.NeedsModelFile:
		for _, st := range p.listenerSteps(e) {
			if st.fixes {
				return &st
			}
		}
	case speech.CantRun:
		return &step{verb: "pick", run: func() tea.Cmd { return p.listenersPage("") }}
	}
	return nil
}

func (p *page) wakeCells(row wakeRow) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.setting != nil:
		return p.options.Cells(row.setting)
	case row.phrase:
		mark := ""
		if p.ownPhrase() != "" {
			mark = render.Cool("●", t)
		}
		return []string{"Phrase", mark, p.voice.GetWake().GetPhrase()}
	case row.listener:
		w := p.voice.GetWake()
		e, known := voice.Listener(p.sp, w)
		if !known {
			return []string{"Listener", opens, render.Warm(w.GetProvider()+" · "+speech.CantRun, t)}
		}
		title := e.Title
		if auto(w) {
			title = "Automatic · " + e.Title
		}
		standing := voice.EngineStanding(p.sp, w, e)
		return []string{"Listener", opens, title + " · " + p.stateWords(standing.State, standing.Cause != "")}
	case row.recognition:
		return []string{partTitle[speechconfig.STT], opens, p.partState(speechconfig.STT)}
	}
	return p.advancedRow(speechconfig.Wake, speechconfig.WakeEnabled)
}

// Whether the wake settings have it listen somewhere other than this app
func elsewhere(w *speechv1.Wake) bool {
	surface := strings.ToLower(strings.TrimSpace(w.GetSurface()))
	return surface != "" && surface != speechconfig.AutoWake && surface != "tui"
}

// Whether the wake settings leave the listener to the automatic choice
func auto(w *speechv1.Wake) bool {
	return w.GetProvider() == "" || w.GetProvider() == speechconfig.AutoWake
}

// The phrase the profile set of its own, empty while it holds the default
func (p *page) ownPhrase() string {
	return plain(p.options.Setting(speechconfig.WakePhrase).GetValue())
}

// Asks for the wake phrase, empty putting back the one icebath gives it
func (p *page) askPhrase() tea.Cmd {
	help := "Such as hey computer. Leave it empty for hey icebath, or hey and the name of the profile while Phrase per profile is on."
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Phrase",
		Fields: []cards.Field{{ID: "value", Label: "Phrase", Help: help, Value: p.ownPhrase(), Select: true}},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			text := strings.ToLower(strings.Join(strings.Fields(values["value"]), " "))
			note := "The wake phrase is back to its default."
			var value any
			if text != "" {
				note, value = "The wake phrase is now “"+text+"”.", text
			}
			return tea.Batch(p.hub.Close(form), p.write(note, map[string]any{speechconfig.WakePhrase: value}))
		},
	})
	return p.hub.Push(form)
}

// Row of the list of listeners: the automatic choice, or a listener
type listenerRow struct {
	auto   bool
	engine speech.Engine
}

// Wake settings as they would pick the listener automatically
func automatic(w *speechv1.Wake) *speechv1.Wake {
	copied := proto.Clone(w).(*speechv1.Wake)
	copied.Provider = speechconfig.AutoWake
	return copied
}

// Opens the listeners: Automatic, then each listener with how it stands, the one in use marked; enter uses one that is ready, else opens
// its page
func (p *page) listenersPage(open string) tea.Cmd {
	t := p.kit.Env.Theme
	var rows []listenerRow
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Listener",
		Glyph:   t.Glyphs.Session,
		Key:     "voice:wake:listeners",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Which listener hears the wake phrase."} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows = []listenerRow{{auto: true}}
			for _, e := range p.sp.Engines() {
				rows = append(rows, listenerRow{engine: e})
			}
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = p.listenerCells(row)
			}
			return cells
		},
		Detail: func(i int) []string {
			w := p.voice.GetWake()
			lines := []string{"A listener that hears any phrase, or the one you gave a model file."}
			e, known := rows[i].engine, true
			if rows[i].auto {
				e, known = voice.Listener(p.sp, automatic(w))
			} else {
				lines = []string{e.About + "."}
			}
			if s := voice.EngineStanding(p.sp, w, e); known && s.Cause != "" {
				lines = append(lines, render.Warm(strs.Full(s.Cause), t))
			}
			return lines
		},
		Empty: func() string { return "No listener is installed." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(rows) {
				return nil
			}
			switch row := rows[i]; {
			case row.auto && auto(p.voice.GetWake()):
				if st := p.listenerFix(); st != nil {
					return []wm.Hint{{Action: "hub.use", Label: st.verb}}
				}
				return nil
			case row.auto:
				return []wm.Hint{{Action: "hub.use", Label: "use"}}
			case p.listenerUsable(row.engine):
				return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.edit", Label: "open"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.auto && auto(p.voice.GetWake()):
					// Automatic does what its listener misses by the same step as that listener's page
					if st := p.listenerFix(); st != nil {
						return st.run()
					}
					return nil
				case row.auto:
					return p.write("The wake word now uses the listener that suits your phrase.", map[string]any{speechconfig.WakeProvider: nil})
				case p.listenerUsable(row.engine):
					return p.write(row.engine.Title+" now hears the wake phrase.", map[string]any{speechconfig.WakeProvider: row.engine.Name})
				}
				return p.listenerPage(rows[i].engine)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if !rows[i].auto {
					return p.listenerPage(rows[i].engine)
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	p.watch(list)
	cmd := p.hub.Push(list)
	// The listener named in hand, as a search of Settings that found it opens the listeners
	if open != "" {
		list.Refresh()
		for i, row := range rows {
			if row.engine.Name == open {
				list.Select(i)
			}
		}
	}
	return cmd
}

// Whether enter on a listener uses it: it can listen and it is not the one the settings name
func (p *page) listenerUsable(e speech.Engine) bool {
	w := p.voice.GetWake()
	return voice.EngineStanding(p.sp, w, e).Cause == "" && (auto(w) || speech.Normal(w.GetProvider()) != e.Name)
}

func (p *page) listenerCells(row listenerRow) []string {
	t := p.kit.Env.Theme
	w := p.voice.GetWake()
	if row.auto {
		e, known := voice.Listener(p.sp, automatic(w))
		state := ""
		if known {
			s := voice.EngineStanding(p.sp, w, e)
			state = e.Title + " · " + p.stateWords(s.State, s.Cause != "")
		}
		if auto(w) {
			state = render.Good(speech.InUse, t) + " · " + state
		}
		return []string{"Automatic", p.kit.Mark(true, auto(w)), state}
	}
	e := row.engine
	chosen := !auto(w) && speech.Normal(w.GetProvider()) == e.Name
	s := voice.EngineStanding(p.sp, w, e)
	state := p.stateWords(s.State, s.Cause != "")
	if chosen && s.Cause == "" {
		state = render.Good(speech.InUse, t)
	}
	return []string{e.Title, p.kit.Mark(true, chosen), state}
}

// Opens the page of a listener: using it, installing it, its access key, then its own settings
func (p *page) listenerPage(e speech.Engine) tea.Cmd {
	t := p.kit.Env.Theme
	var steps []step
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   e.Title,
		Glyph:   t.Glyphs.Session,
		Key:     "voice:wake:" + e.Name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if s := voice.EngineStanding(p.sp, p.voice.GetWake(), e); s.Cause != "" {
				return []string{render.Warm(strs.Full(s.Cause), t)}
			}
			return []string{"Ready to listen."}
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			steps = p.listenerSteps(e)
			cells := make([][]string, len(steps))
			for i, st := range steps {
				cells[i] = p.stepCells(st)
			}
			return cells
		},
		Group: func(i int) string {
			if steps[i].setting != nil {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			switch st := steps[i]; {
			case st.setting != nil:
				return p.options.Detail(st.setting, view.Cut(i, 2))
			case st.title == "Use it":
				return []string{"Have " + e.Title + " hear the wake phrase from now on."}
			case st.title == "Install" && e.Fetch != nil:
				return []string{"Download " + e.Title + " onto this computer, and show how it goes."}
			case st.title == "Install":
				return []string{"Download " + e.Title + " into icebath's own Python on this computer, and show how it goes."}
			}
			return []string{"Kept in your keys file as " + speechconfig.AccessKeyEnv + ". Picovoice gives one free at https://console.picovoice.ai."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "") },
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

// Rows of the page of a listener as it stands now
func (p *page) listenerSteps(e speech.Engine) []step {
	t := p.kit.Env.Theme
	w := p.voice.GetWake()
	s := voice.EngineStanding(p.sp, w, e)
	var steps []step
	if auto(w) || speech.Normal(w.GetProvider()) != e.Name {
		steps = append(steps, step{title: "Use it", verb: "use",
			run: func() tea.Cmd {
				return p.write(e.Title+" now hears the wake phrase.", map[string]any{speechconfig.WakeProvider: e.Name})
			}})
	}
	switch {
	case s.State == speech.NotInstalled && e.Fetch != nil:
		steps = append(steps, step{title: "Install", value: render.Warm(speech.NotInstalled, t), verb: "install", fixes: true,
			run: func() tea.Cmd { return p.fetch(e) }})
	case s.State == speech.NotInstalled && e.Extra != "":
		steps = append(steps, step{title: "Install", value: render.Warm(speech.NotInstalled, t), verb: "install", fixes: true,
			run: func() tea.Cmd { return p.installHere(e.Title, e.Extra) }})
	}
	if e.Key {
		value := "Set"
		if strings.TrimSpace(w.GetAccessKey()) == "" {
			value = render.Warm("Not set", t)
		}
		steps = append(steps, step{title: "Access key", value: value, verb: "change", fixes: s.State == voice.NeedsAccessKey, run: func() tea.Cmd {
			help := "Picovoice gives one free at https://console.picovoice.ai. Kept in your keys file as " + speechconfig.AccessKeyEnv + "."
			return p.kit.Ask(p.hub, "Access key", cards.Field{Label: "Access key", Kind: cards.FieldSecret, Help: help}, "Save", func(text string) tea.Cmd {
				return p.putKey(e.Title, speechconfig.AccessKeyEnv, text)
			})
		}})
	}
	everyday, advanced := p.options.Section(speechconfig.Wake + "." + e.Name)
	for _, setting := range append(everyday, advanced...) {
		st := step{setting: setting}
		// A file or a folder of a listener is read where the wake word listens, on the computer this app runs on, so it is looked for here
		if setting.GetPath() != nil {
			st.run = func() tea.Cmd { return p.changePath(setting) }
		}
		// The model file a listener misses, which its row on the page of the wake word asks for in place
		if setting.GetKey() == e.Model && s.State == voice.NeedsModelFile {
			st.fixes, st.verb = true, "change"
		}
		steps = append(steps, st)
	}
	return steps
}

// Changes a setting of a listener that names a file or a folder. The listener reads it on the computer this app runs on, so the path is
// found here, a leading ~ the home folder of this computer, and must be there before it is kept whole, whichever computer keeps the
// settings. Empty puts the default back
func (p *page) changePath(setting *configv1.Setting) tea.Cmd {
	kind, what := cards.PathFile, "file"
	if setting.GetPath().GetKind() == configv1.PathKind_PATH_KIND_FOLDER {
		kind, what = cards.PathFolder, "folder"
	}
	var held string
	yaml.Unmarshal([]byte(setting.GetValue()), &held)
	// The path the text names on this computer, or what is wrong with it
	locate := func(typed string) (string, string) {
		if typed = strings.TrimSpace(typed); typed == "" {
			return "", ""
		}
		path := fsx.ExpandHome(typed)
		switch {
		case !filepath.IsAbs(path):
			return "", "Write the whole path of the " + what + ", starting with / or ~."
		case kind == cards.PathFolder && !fsx.IsDir(path):
			return "", "There is no folder at " + path + " on this computer."
		case kind == cards.PathFile && !fsx.IsFile(path):
			return "", "There is no file at " + path + " on this computer."
		}
		return filepath.Clean(path), ""
	}
	help := []string{strs.Full(setting.GetDescription())}
	if more := setting.GetHelp(); more != "" {
		help = append(help, strs.Full(more))
	}
	help = append(help, "The whole path of the "+what+" on this computer, starting with / or ~. Tab completes it.")
	field := cards.Field{ID: "path", Label: setting.GetTitle(), Value: held, Select: true, Path: kind, Help: strings.Join(help, "\n"),
		Check: func(typed string) string { _, wrong := locate(typed); return wrong }}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{Title: setting.GetTitle(), Fields: []cards.Field{field}, Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			path, _ := locate(values["path"])
			if path == held {
				return p.hub.Close(form)
			}
			if path == "" {
				return tea.Batch(p.hub.Close(form), p.write(setting.GetTitle()+" is back to its default.", map[string]any{setting.GetKey(): nil}))
			}
			return tea.Batch(p.hub.Close(form), p.write(setting.GetTitle()+" is now "+path+".", map[string]any{setting.GetKey(): path}))
		}})
	return p.hub.Push(form)
}

// Reporter of an install that runs on this computer, its lines and what it does now handed to the page that shows them
type reporting struct {
	line, status func(text string)
}

func (r reporting) Line(text string) { r.line(text) }

func (r reporting) Status(text string) { r.status(text) }

func (reporting) Terminal() (bool, int) { return false, 0 }

// Installs the Python set of a listener on the computer this app runs on, where the wake word listens and where its listener is looked for,
// which is not the computer of a server the app is connected to; the steps shown as it goes, and the wake word that is on starts once it is
func (p *page) installHere(title, extra string) tea.Cmd {
	w, name := p.voice.GetWake(), p.voice.GetProfile()
	started := func() tea.Cmd {
		if !w.GetEnabled() {
			return nil
		}
		return p.follow(map[string]any{speechconfig.WakeEnabled: true})
	}
	return p.installPage(title, extra, started, func(ctx context.Context, line, status func(text string)) (string, bool) {
		cfg, err := p.sp.Profile(name)
		if err != nil {
			return notInstalled(title, err.Error()), false
		}
		code, err := p.sp.Packages().Install(ctx, cfg, packages.InstallArgs{Extras: []string{extra}}, reporting{line: line, status: status})
		switch {
		case err != nil && ctx.Err() != nil:
			return "", true
		case err != nil:
			reason, known := p.sp.Packages().Failed(err)
			if !known {
				reason = err.Error()
			}
			return notInstalled(title, reason), false
		case code != 0:
			return title + " was not installed. The lines above say why.", false
		}
		return "", false
	})
}

// Installs a listener whose install is a download onto this computer, the steps shown as it goes; the wake word that is on starts once it is
func (p *page) fetch(e speech.Engine) tea.Cmd {
	var lines []string
	ended := false
	view := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  "Install",
		Key:    "voice:install:" + e.Name,
		Follow: true,
		Lines: func(int) []string {
			if len(lines) == 0 {
				return []string{render.Dim("Starting…", p.kit.Env.Theme)}
			}
			return slices.Clone(lines)
		},
	})
	w := p.voice.GetWake()
	wake := &speech.Wake{Speech: p.sp, Profile: p.voice.GetProfile(), Phrase: w.GetPhrase(), Settings: w.GetSettings()}
	ctx := p.kit.Env.Ctx
	go func() {
		err := e.Fetch(ctx, wake, func(line string) {
			p.kit.Apply(func() tea.Cmd { lines = append(lines, line); return nil })
		})
		p.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended = true
			t := p.kit.Env.Theme
			if err != nil {
				text := e.Title + " was not installed. The error was: " + strings.TrimSuffix(tui.Reason(err), ".") + "."
				lines = append(lines, "", render.Bad(text, t))
				return tea.Batch(p.hub.Fail(text), p.load(nil))
			}
			text := e.Title + " is installed."
			lines = append(lines, "", render.Good(text, t))
			cmds := []tea.Cmd{p.hub.Done(text), p.load(nil)}
			if w.GetEnabled() {
				cmds = append(cmds, p.follow(map[string]any{speechconfig.WakeEnabled: true}))
			}
			return tea.Batch(cmds...)
		})
	}()
	return p.hub.Push(view)
}

// Picker of a microphone of this computer: the default one first, in the zero words of the setting, then each microphone found; the one
// picked handed back by name
func microphones(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	t := kit.Env.Theme
	var found []voice.Microphone
	loaded, failure := false, ""
	current := plain(ask.Values[ask.Key])
	type choice struct{ value, title, about string }
	choices := func() []choice {
		var out []choice
		for _, first := range ask.First {
			out = append(out, choice{value: first.ID, title: first.Title, about: first.Detail})
		}
		for _, m := range found {
			about := m.Host
			if m.Default {
				about = strings.TrimPrefix(about+" · the default one", " · ")
			}
			out = append(out, choice{value: m.Value, title: m.Name, about: about})
		}
		return out
	}
	var shown []choice
	var list *cards.List
	list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Microphone",
		Glyph:   t.Glyphs.Session,
		Key:     "pick:microphone",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro: func() []string {
			if len(ask.Intro) > 0 {
				return ask.Intro
			}
			return []string{"The microphone the listener hears you with."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = choices()
			cells := make([][]string, len(shown))
			for i, c := range shown {
				cells[i] = []string{kit.Mark(true, c.value == current), c.title, render.Dim(c.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if list.Shows(2) || shown[i].about == "" {
				return nil
			}
			return []string{shown[i].about}
		},
		Banner: func() string {
			if failure != "" {
				return render.Warm("No microphone could be listed. "+strs.Full(failure), t)
			}
			return ""
		},
		Empty: func() string { return tui.Waiting(loaded, nil, "No microphone was found.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "choose"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				value := "null"
				if shown[i].value != "" {
					value, _ = encode(shown[i].value)
				}
				return tea.Batch(hub.Close(list), ask.Done(map[string]string{ask.Key: value}))
			},
		},
	})
	list.ToggleDetail()
	// The wake word listens on the computer this app runs on, as voice input records there, so its microphones are the ones to pick from, not
	// those of a server the app is connected to on another computer
	read := tui.Async(func() func() tea.Cmd {
		mics, err := voice.Microphones()
		return func() tea.Cmd {
			loaded = true
			switch {
			case err != nil:
				failure = tui.Reason(err)
			default:
				found, failure = mics, ""
			}
			list.Refresh()
			for i, c := range choices() {
				if c.value == current {
					list.Select(i)
				}
			}
			return nil
		}
	})
	return tea.Batch(hub.Push(list), read)
}
