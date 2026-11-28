package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of the page of Voice: an option, a part of Voice that opens its page, the record key, or the row of the advanced options
type voiceRow struct {
	setting  *configv1.Setting
	part     string
	record   bool
	advanced bool
}

// What each part of Voice is, as the row that opens its page explains it
var partAbout = map[string]string{
	speechconfig.STT:  "Which service turns what you say into text.",
	speechconfig.TTS:  "The voice that reads replies aloud.",
	speechconfig.Wake: "Say a phrase to start talking, hands-free.",
}

// Titles of the parts of Voice
var partTitle = map[string]string{speechconfig.STT: "Speech to text", speechconfig.TTS: "Text to speech", speechconfig.Wake: "Wake word"}

// The first view of the page: what Voice is, the one thing to do while voice can't hear you, its two switches, the rows of its parts, then
// its options with the record key among them and its advanced ones. Opened at a target, it goes there once it is read
func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []voiceRow
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Voice",
		Glyph:   t.Glyphs.Session,
		Key:     "voice",
		Context: cards.HubContext,
		Primary: "hub.use",
		// What keeps each switch from working, each from its own answer: whether voice can hear you, and whether replies can be read aloud
		Intro: func() []string {
			lines := []string{"Talk to the agent and hear its replies."}
			if !p.loaded || p.err != nil {
				return lines
			}
			for _, line := range []string{p.hearing.Line(), voice.SpeakingLine(p.voice)} {
				if line != "" {
					lines = append(lines, render.Warm(line, t))
				}
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.voiceRows()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				cells[i] = p.voiceCells(row)
			}
			return cells
		},
		Group: func(i int) string {
			if row := shown[i]; row.part != "" || row.setting != nil && slices.Contains(switches, row.setting.GetKey()) {
				return ""
			}
			return "Options"
		},
		Detail: func(i int) []string {
			switch row := shown[i]; {
			case row.setting.GetKey() == speechconfig.Input:
				return append(p.options.Detail(row.setting, p.root.Cut(i, 2)), p.recording()...)
			case row.setting != nil:
				return p.options.Detail(row.setting, p.root.Cut(i, 2))
			case row.part != "":
				return []string{partAbout[row.part]}
			case row.record:
				return []string{"The key that starts and stops a recording in a chat."}
			}
			return []string{"Settings of voice input that few people change."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch row := shown[i]; {
			case row.setting != nil:
				return p.options.Hints(row.setting)
			case row.record:
				return []wm.Hint{{Action: "hub.use", Label: "change"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch row := shown[i]; {
				case row.setting != nil:
					return p.options.Change(row.setting)
				case row.part != "":
					return p.openPart(row.part)
				case row.record:
					return p.kit.Open(keysScreen, RecordAction)
				}
				return p.advanced(speechconfig.Voice, switches...)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
		},
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Voice, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// What there is to know of how this computer records, under Voice input: the steps that fix what keeps it from recording, and how its sound
// reaches icebath
func (p *page) recording() []string {
	t := p.kit.Env.Theme
	var lines []string
	if r := p.hearing.Recording; r != nil && r.Detail != "" {
		for _, step := range strings.Split(r.Detail, "\n") {
			lines = append(lines, render.Warm(step, t))
		}
	}
	for _, notice := range p.hearing.Notices {
		lines = append(lines, render.Dim(notice, t))
	}
	return lines
}

// Switches of voice input, first on the page
var switches = []string{speechconfig.Input, speechconfig.SpeakReplies}

// Options of voice input: those of its section but the switches, the advanced ones in place while they are few
func (p *page) voiceOptions() (shown, behind []*configv1.Setting) {
	return p.section(speechconfig.Voice, switches...)
}

// Rows of the page of Voice
func (p *page) voiceRows() []voiceRow {
	if !p.loaded || p.err != nil {
		return nil
	}
	var rows []voiceRow
	for _, setting := range p.settingsOf(switches...) {
		rows = append(rows, voiceRow{setting: setting})
	}
	for _, part := range parts {
		rows = append(rows, voiceRow{part: part})
	}
	shown, behind := p.voiceOptions()
	for i, setting := range shown {
		rows = append(rows, voiceRow{setting: setting})
		// The record key right after what happens once you speak, as the key that starts it
		if i == 0 {
			rows = append(rows, voiceRow{record: true})
		}
	}
	if len(behind) > 0 {
		rows = append(rows, voiceRow{advanced: true})
	}
	return rows
}

func (p *page) voiceCells(row voiceRow) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.setting != nil:
		return p.switchCells(row.setting)
	case row.part != "":
		return []string{partTitle[row.part], opens, p.partState(row.part)}
	case row.record:
		key := p.kit.Env.Keys.DisplayAll(RecordAction)
		if key == "" {
			key = render.Warm("No key", t)
		}
		mark := ""
		if !p.kit.Env.Keys.AtDefault(RecordAction) {
			mark = render.Cool("●", t)
		}
		return []string{"Record key", mark, key}
	}
	return p.advancedRow(speechconfig.Voice, switches...)
}

// Cells of an option row, a switch that is on with what keeps it from working after its value: Voice input while voice can't hear you,
// Speak replies while replies can't be read aloud, each from its own answer
func (p *page) switchCells(setting *configv1.Setting) []string {
	cells := p.options.Cells(setting)
	if len(cells) < 3 {
		return cells
	}
	t, mode := p.kit.Env.Theme, p.voice.GetMode()
	switch setting.GetKey() {
	case speechconfig.Input:
		if mode.GetInput() && !p.hearing.Ready() {
			cells[2] += " · " + render.Warm("Can't hear you yet", t)
		}
	case speechconfig.SpeakReplies:
		if s := p.voice.GetSpeaking(); mode.GetSpeak() && s.GetHindrance() != nil {
			cells[2] += " · " + render.Warm(s.GetState(), t)
		}
	}
	return cells
}

// How a part of Voice stands, as its row says it: the service in use and its state, Automatic with the service it stands for, the wake
// word on or off and whether it can listen
func (p *page) partState(part string) string {
	t := p.kit.Env.Theme
	switch part {
	case speechconfig.STT:
		r := p.voice.GetRecognition()
		switch {
		case !r.GetEnabled():
			return render.Warm("Off", t)
		case r.GetChosen() == "" && r.GetHindrance() != nil:
			return "Automatic · " + render.Warm("None is ready", t)
		case r.GetChosen() == "":
			return "Automatic · " + r.GetTitle()
		}
		return r.GetTitle() + " · " + p.stateWords(r.GetState(), r.GetHindrance() != nil)
	case speechconfig.TTS:
		s := p.voice.GetSpeaking()
		return s.GetTitle() + " · " + p.stateWords(s.GetState(), s.GetHindrance() != nil)
	}
	switch w := p.voice.GetWake(); {
	case !w.GetEnabled():
		return "Off"
	case !p.wake.Available:
		return "On · " + render.Warm("Can't listen", t)
	case elsewhere(w):
		return "On · " + render.Warm("Listens in "+p.options.Setting(speechconfig.WakeSurface).GetShown(), t)
	case p.live != nil && p.live.Silent:
		return "On · " + render.Warm("Hears only silence", t)
	}
	return "On · Listens for “" + p.voice.GetWake().GetPhrase() + "”"
}

// State words in the color of how they stand
func (p *page) stateWords(state string, hindered bool) string {
	if hindered {
		return render.Warm(state, p.kit.Env.Theme)
	}
	return render.Good(state, p.kit.Env.Theme)
}

// Opens the page of a part of Voice
func (p *page) openPart(part string) tea.Cmd {
	switch part {
	case speechconfig.STT:
		return p.servicesPage(recognizers)
	case speechconfig.TTS:
		return p.servicesPage(voices)
	}
	return p.wakePage()
}

// Goes where the page was opened at: the page of a part, or a row of the first view with the note of what to do there
func (p *page) reach(target string) tea.Cmd {
	switch target {
	case AtRecognition:
		return p.openPart(speechconfig.STT)
	case AtSpeech:
		return p.openPart(speechconfig.TTS)
	case AtWake:
		return p.openPart(speechconfig.Wake)
	case AtWakeOn:
		open := p.openPart(speechconfig.Wake)
		return tea.Batch(open, p.landWake(p.wakeList))
	case AtInput:
		p.selectRow(func(row voiceRow) bool { return row.setting.GetKey() == speechconfig.Input })
		if key := p.kit.Env.Keys.Display(RecordAction); key != "" {
			return p.hub.Say("Turn on Voice input to talk with "+key+".", cards.NoteInfo)
		}
		return p.hub.Say("Turn on Voice input, and give the record key a key to talk with.", cards.NoteInfo)
	case AtSpeaking:
		// The second line of the page says what keeps replies from being read aloud and what to do, so no note says it again
		p.selectRow(func(row voiceRow) bool { return row.part == speechconfig.TTS })
		return nil
	}
	// The page of one service or program of a part, or one listener of the wake word in hand, by its name after the part
	part, name, _ := strings.Cut(target, ":")
	switch {
	case name == "":
	case part == AtRecognition:
		return tea.Batch(p.openPart(speechconfig.STT), p.servicePage(recognizers, name))
	case part == AtSpeech:
		return tea.Batch(p.openPart(speechconfig.TTS), p.servicePage(voices, name))
	case part == AtWake:
		return tea.Batch(p.openPart(speechconfig.Wake), p.listenersPage(name))
	}
	return nil
}

// Puts the cursor on the first row of the first view that matches
func (p *page) selectRow(match func(row voiceRow) bool) {
	for i, row := range p.voiceRows() {
		if match(row) {
			p.root.Select(i)
			return
		}
	}
}
