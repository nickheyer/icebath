package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/speech"
	speechapp "github.com/nickheyer/icebath/internal/speech/app"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Action that starts and stops a recording, and the key it starts with
	voiceAction = speechapp.RecordAction
	voiceKey    = "alt+r"
	// Lease that keeps the voice warm while this window speaks replies
	voiceLease = "tui:voice-tts"
)

// The key that records: one binding of the keymap, changed in Keyboard shortcuts like every other, which refuses a key a chat can't give it
var voiceKeys = []keys.Action{
	{ID: voiceAction, Context: keys.Session, Label: "record", Doc: "Start or stop recording your voice", Default: []string{voiceKey}, Refuse: refuseRecord},
}

// Contexts a chat presses the record key in besides its own, whose keys it can't take
var voiceContexts = []string{keys.Text, keys.Global}

// Letters the terminal keeps for itself with a modifier: ctrl everywhere, the command key and option on a Mac
const (
	reservedCtrl     = "cdlx"
	reservedSuper    = "cdlv"
	reservedAltOnMac = "cdl"
)

// Why the record key can't be the chord: another key of a chat does something with it, or the terminal keeps it for itself
func refuseRecord(m *keys.Map, chord string) string {
	for _, holder := range m.Lookup(voiceContexts, keys.Press(chord)) {
		if holder != voiceAction {
			act, _ := m.Action(holder)
			return keys.Format(chord) + " already does “" + act.LabelText() + "” in a chat"
		}
	}
	mod, key, _ := strings.Cut(chord, "+")
	if len([]rune(key)) != 1 {
		return ""
	}
	mac := runtime.GOOS == "darwin"
	switch {
	case mod == "ctrl" && strings.Contains(reservedCtrl, key):
		return "your terminal keeps " + keys.Format(chord) + " for itself"
	case mac && mod == "super" && strings.Contains(reservedSuper, key):
		return "macOS keeps " + keys.Format(chord) + " for itself"
	case mac && mod == "alt" && strings.Contains(reservedAltOnMac, key):
		return keys.Format(chord) + " types a character on a Mac"
	}
	return ""
}

// Voice of this app: one microphone and one speaker. Voice input records into a chat; replies are read aloud by a rule of their own, which
// needs no chat to hold voice input
type voicing struct {
	desk   *desk
	mode   *voice.Mode
	client speechv1connect.SpeechServiceClient
	// Chat recordings go to, the one voice input last recorded in, nil while voice input is off in this window; the voice input setting was
	// on, so the next chat focused takes it
	owner *chat
	adopt bool
	// Chat the last message of this window was sent from while the reply to it has not started, the chat a spoken message is sent from
	// while it is, and the chat whose reply is read aloud with the speech of that reply while its text is still written
	asked  *chat
	sender *chat
	aloud  *chat
	turn   *voice.Turn
	// Chats by when each last came to the front, the last highest
	fronts map[*chat]int
	seen   int
	// Name of the profile a chat that names none is on, as the settings last said it
	homed string
	// listening, transcribing or idle, and the words a live transcription heard so far
	state   string
	partial string
	// The wake word listens here; the person stopped it with /wake off, which keeps it from arming on its own
	listening bool
	quiet     bool
	// Profile the voice is kept warm on, empty while it isn't
	leased string
	// A read of the settings runs, and another was asked for meanwhile
	reading bool
	again   bool
	// Profile the engine reads settings of and the chat voice input is on in, kept by the app's goroutine for the engine's
	current atomic.Value
	owned   atomic.Pointer[chat]
}

// Voice input over the speech service of the connection and the speech host of this machine
func (d *desk) voicing(conn *connect.Client, sp *speech.Speech) *voicing {
	v := &voicing{desk: d, state: "idle", client: speechv1connect.NewSpeechServiceClient(conn), fronts: map[*chat]int{}}
	apply := func(fn func() tea.Cmd) {
		if d.kit != nil {
			d.kit.Apply(fn)
		}
	}
	v.mode = voice.New(sp, v.client, func() string { name, _ := v.current.Load().(string); return name }, voice.Events{
		Status: func(state string) {
			apply(func() tea.Cmd {
				if v.state = state; state != "listening" {
					v.partial = ""
				}
				return nil
			})
		},
		Partial:    func(text string) { apply(func() tea.Cmd { v.partial = text; return nil }) },
		Transcript: func(text string) { apply(func() tea.Cmd { v.partial = ""; return v.heard(text) }) },
		Stopped:    func(said string) { apply(func() tea.Cmd { return v.stopped(said) }) },
		Silent: func() {
			apply(func() tea.Cmd {
				return tea.Batch(v.note("Voice input turned off after three recordings in a row heard no speech."), v.off(true))
			})
		},
		Interrupted: func() {
			apply(func() tea.Cmd {
				if v.aloud == nil {
					return nil
				}
				return tea.Batch(toast(d.kit, "You talked over the reply, so it stopped."), v.aloud.interrupt())
			})
		},
		Woke: func(phrase, profile string, routed, fresh bool) {
			apply(func() tea.Cmd { return v.woke(phrase, profile, routed, fresh) })
		},
	})
	v.mode.Busy(func() bool {
		c := v.owned.Load()
		return c != nil && c.working.Load()
	})
	return v
}

// Keeps the profile voice input reads its settings from where the engine's goroutines read it
func (v *voicing) track() {
	v.current.Store(v.profile())
	v.owned.Store(v.owner)
}

// Profile voice reads its settings from: the owner's while a recording runs or transcribes, else the focused chat's, else the owner's, else
// the one in use
func (v *voicing) profile() string {
	if v.owner != nil && (v.mode.Recording() || v.mode.Transcribing()) {
		return v.owner.on()
	}
	if c, ok := v.desk.focused(); ok {
		return c.on()
	}
	if v.owner != nil {
		return v.owner.on()
	}
	return ""
}

// Chat that has the focus
func (d *desk) focused() (*chat, bool) {
	if d.kit == nil {
		return nil, false
	}
	c, ok := d.kit.Focused().(*chat)
	return c, ok
}

// Line in the owner's transcript, else the focused chat's, else a notice while no chat is open
func (v *voicing) note(text string) tea.Cmd {
	c := v.owner
	if c == nil {
		c, _ = v.desk.focused()
	}
	if c == nil {
		return toast(v.desk.kit, text)
	}
	c.log.note(text)
	return nil
}

// Line in the chat, else a notice while there is none
func (v *voicing) say(c *chat, text string) tea.Cmd {
	if c == nil {
		return toast(v.desk.kit, text)
	}
	c.log.note(text)
	return nil
}

// Opens the page of Voice for the profile, at the place the target names
func (v *voicing) page(name, target string) tea.Cmd {
	return v.desk.kit.Open(speechapp.Voice, strings.TrimSpace(profile.Join(name, target)))
}

// Opens a page of Voice for the profile an argument names, else the focused chat's
func (v *voicing) pageOf(argument, target string) tea.Cmd {
	named, _ := profile.Split(argument)
	return v.page(cmp.Or(named, v.profile()), target)
}

// Reads the settings of voice input again, one read at a time, then follows them
func (v *voicing) refresh() tea.Cmd {
	if v.reading {
		v.again = true
		return nil
	}
	v.reading = true
	v.track()
	from, _ := v.current.Load().(string)
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.mode.Refresh(v.desk.kit.Env.Ctx)
		return func() tea.Cmd {
			v.reading = false
			if err == nil && from == "" {
				v.homed = reply.GetProfile()
			}
			if v.again {
				v.again = false
				return v.refresh()
			}
			if err != nil {
				slog.Debug("Voice settings could not be read", "error", err)
				return nil
			}
			return tea.Batch(v.settle(), v.awake())
		}
	})
}

// The voice input setting changed, or the app started with it: on, the next chat focused takes voice input; off, voice input ends here
func (v *voicing) follow(value string) tea.Cmd {
	var on bool
	yaml.Unmarshal([]byte(value), &on)
	v.adopt = on && v.owner == nil
	if !on && v.owner != nil {
		return tea.Batch(v.off(false), v.refresh())
	}
	return v.refresh()
}

// Voice as the settings now stand: a chat taking voice input that waits to, and the voice kept warm while replies are read aloud on the
// profile
func (v *voicing) settle() tea.Cmd {
	settings := v.mode.Settings()
	mode := settings.GetMode()
	if v.adopt && v.owner == nil && mode.GetInput() {
		if c, ok := v.desk.focused(); ok {
			v.owner, v.adopt = c, false
			v.track()
		}
	}
	return v.warm(mode.GetSpeak() && settings.GetSpeaking().GetReady())
}

// A chat came to the front: it is the last in front, it takes voice input when the voice input setting is on and no chat has it yet, and
// voice reads the settings of its profile when that is another
func (v *voicing) focused(c *chat) tea.Cmd {
	v.seen++
	v.fronts[c] = v.seen
	before, _ := v.current.Load().(string)
	v.track()
	if v.adopt && v.owner == nil && v.mode.Settings().GetMode().GetInput() {
		v.owner, v.adopt = c, false
		v.track()
	}
	if now, _ := v.current.Load().(string); now != before {
		return v.refresh()
	}
	return v.settle()
}

// A chat's window closed: a recording into it ends with the voice input setting kept, the reply of it being read stops, and the reply it
// waits for is not read
func (v *voicing) closed(c *chat) tea.Cmd {
	delete(v.fronts, c)
	if v.asked == c {
		v.asked = nil
	}
	if v.aloud == c {
		v.mode.Hush()
		v.turn, v.aloud = nil, nil
	}
	if v.owner == c {
		return v.off(false)
	}
	return nil
}

// The stop key in a chat: the reply of it read aloud stops, what sounds and what is still to come of it, which is all the key does while that
// reply is no longer written; true when speech was to say or sounding
func (v *voicing) hush(c *chat) bool {
	if v.aloud != c {
		return false
	}
	speaking := v.mode.Speaking()
	v.mode.Hush()
	v.turn, v.aloud = nil, nil
	return speaking
}

// Takes or lets go of the lease that keeps the voice of the profile warm
func (v *voicing) warm(on bool) tea.Cmd {
	profile := ""
	if on {
		profile = v.mode.Settings().GetProfile()
	}
	before := v.leased
	if profile == before {
		return nil
	}
	v.leased = profile
	ctx := v.desk.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		lease(ctx, v.client, before, false)
		lease(ctx, v.client, profile, true)
		return nil
	})
}

// Takes or drops the lease on the profile, a failure only logged
func lease(ctx context.Context, client speechv1connect.SpeechServiceClient, profile string, active bool) {
	if profile == "" {
		return
	}
	if _, err := client.Lease(ctx, &speechv1.LeaseRequest{Profile: profile, Lease: voiceLease, Active: active}); err != nil {
		slog.Debug(fmt.Sprintf("voice: tts lease %s active=%t failed: %s", voiceLease, active, err))
	}
}

// The wake word follows its setting: armed while it is on unless the person stopped it with /wake off, stopped once it is off
func (v *voicing) awake() tea.Cmd {
	enabled := v.mode.Settings().GetWake().GetEnabled()
	switch {
	case enabled && !v.quiet && !v.listening:
		return v.arm()
	case !enabled && v.listening:
		v.mode.StopWake()
		v.listening = false
	}
	return nil
}

// The record key as a chat shows it, alt+r while nobody gave it another; empty while it has none
func (v *voicing) keyLabel() string {
	return v.desk.kit.Env.Keys.Display(voiceAction)
}

// Voice input moves to the chat: a recording into the chat that had it ends
func (v *voicing) claim(c *chat) {
	if v.owner == c {
		return
	}
	if v.owner != nil {
		v.mode.Stop(v.desk.kit.Env.Ctx, false)
	}
	v.owner, v.adopt = c, false
	v.track()
}

// The text of the reply read aloud ended: what is queued is still said, and the chat stays the one whose reply is read until another's is
func (v *voicing) finish() {
	if v.turn != nil {
		v.turn.Finish()
	}
	v.turn = nil
}

// How voice stands for a profile: its settings, and whether voice can hear you here with them and the record key as the app shows it; read
// off the app's goroutine
func (v *voicing) look(ctx context.Context, name, key string) (*speechv1.VoiceReply, voice.Hearing, error) {
	settings, err := v.client.Voice(ctx, &speechv1.VoiceRequest{Profile: name})
	if err != nil {
		return nil, voice.Hearing{}, err
	}
	return settings, voice.Hears(settings, key), nil
}

// The record key in a chat: stops a recording that runs, else starts one while voice can hear you there. Where voice can't hear you it opens
// the page of Voice, which says why, and where voice input is off the page on that switch
func (v *voicing) toggle(c *chat) tea.Cmd {
	ctx := v.desk.kit.Env.Ctx
	if v.owner == c && v.mode.Recording() {
		go v.mode.Stop(ctx, true)
		return nil
	}
	if v.owner == c && v.mode.Transcribing() {
		c.log.note(stillHearing)
		return nil
	}
	name, key := c.on(), v.keyLabel()
	return tui.Async(func() func() tea.Cmd {
		settings, hearing, err := v.look(ctx, name, key)
		return func() tea.Cmd {
			switch {
			case err != nil:
				c.log.note("The recording did not start, because the voice settings could not be read. The error was: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
				return nil
			case !hearing.Ready():
				return v.page(name, "")
			case !settings.GetMode().GetInput():
				return v.page(name, speechapp.AtInput)
			}
			v.claim(c)
			return v.record(c)
		}
	})
}

// Starts a recording in the chat that holds voice input, with the settings of its profile: one utterance a press, sent once its speaker
// falls silent
func (v *voicing) record(c *chat) tea.Cmd {
	ctx := v.desk.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		_, err := v.mode.Refresh(ctx)
		started := false
		if err != nil {
			err = fmt.Errorf("The recording did not start, because the voice settings could not be read. The error was: %s.",
				strings.TrimSuffix(tui.Reason(err), "."))
		} else {
			started, err = v.mode.Record(ctx, false)
		}
		return func() tea.Cmd {
			switch {
			case err != nil:
				c.log.note(tui.Reason(err))
			case !started:
				c.log.note(stillHearing)
			}
			return v.settle()
		}
	})
}

// What a recording said: sent at once in place of what the prompt holds, or added to the prompt to edit, as voice.submit_mode says
func (v *voicing) heard(text string) tea.Cmd {
	c := v.owner
	if c == nil {
		c, _ = v.desk.focused()
	}
	if text = strings.TrimSpace(text); c == nil || text == "" {
		return nil
	}
	if v.mode.Settings().GetMode().GetSubmitMode() == speechconfig.SubmitDraft {
		if typed := strings.TrimRight(c.composer.Value(), " \t\n"); strings.TrimSpace(typed) != "" {
			text = typed + " " + text
		}
		return c.composer.SetValue(text)
	}
	v.sender = c
	defer func() { v.sender = nil }()
	return tea.Batch(c.composer.SetValue(""), c.sendSpoken(text))
}

// Whether a message typed in the chat is only a stop phrase while voice input is on there, which turns voice input off instead of being sent
func (v *voicing) typedStop(c *chat, text string) bool {
	return v.owner == c && v.mode.Settings().GetMode().GetInput() && v.mode.StopPhrase(text)
}

// A stop phrase, spoken or typed, turned voice input off: the switch saved off
func (v *voicing) stopped(said string) tea.Cmd {
	said = strings.Trim(strings.TrimSpace(said), ".,!?;: ")
	return tea.Batch(v.note("You said “"+said+"”, so Voice input is now Off."), v.off(true))
}

// A message is sent from a chat of this window, typed or spoken: speech still playing is cut, and the reply to this message is the one read
// aloud next. Answers whether the message cuts a spoken reply off or follows one that was, which every message tells the model
func (v *voicing) cut() bool {
	c := v.sender
	if c == nil {
		c, _ = v.desk.focused()
	}
	v.asked = c
	return v.mode.TakeInterrupted()
}

// Saves voice settings of the profile, reads the settings again and follows them, then runs after on what they came to; a failure said in
// the chat
func (v *voicing) change(c *chat, name string, values map[string]bool, after func(settings *speechv1.VoiceReply) tea.Cmd) tea.Cmd {
	v.track()
	ctx := v.desk.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		var err error
		for key, on := range values {
			value, _ := yaml.Marshal(on)
			if _, err = v.desk.settings.Set(ctx, &configv1.SetRequest{Key: key, Value: string(value), Profile: name}); err != nil {
				break
			}
		}
		var settings *speechv1.VoiceReply
		if err == nil {
			settings, err = v.client.Voice(ctx, &speechv1.VoiceRequest{Profile: name})
		}
		if err == nil {
			_, err = v.mode.Refresh(ctx)
		}
		return func() tea.Cmd {
			if err != nil {
				return v.say(c, "The voice settings were not saved. The error was: "+strings.TrimSuffix(tui.Reason(err), ".")+".")
			}
			cmds := []tea.Cmd{v.settle()}
			if after != nil {
				cmds = append(cmds, after(settings))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Voice input off in this window: a recording stopped with nothing sent, listening for talking over a reply ended; a reply being read aloud
// goes on, since that is Speak replies. The switch saved off when asked
func (v *voicing) off(persist bool) tea.Cmd {
	c := v.owner
	v.mode.Stop(v.desk.kit.Env.Ctx, false)
	v.owner, v.adopt, v.state, v.partial = nil, false, "idle", ""
	v.track()
	if persist && c != nil {
		return v.change(c, c.on(), map[string]bool{speechconfig.Input: false}, nil)
	}
	return nil
}

// Reads aloud the reply to the message you just sent, by one rule that needs no chat to hold voice input: the reply that starts in the chat
// the last message of this window was sent from, typed or spoken, while that chat is in front, in the voice and by the Speak replies of
// that chat's own profile, whether voice input is on or off. A reply that starts in a chat that is not in front is skipped, not kept to
// say later: a voice reading another chat over the one you are in, or reading a stale reply once you come back to it, is not what a person
// at the keyboard expects, and the reply waits in its chat to be read. Once started, a reply is read to its end unless a new message, the
// stop key, a recording or talking over it cuts it. A reply starts when the session takes the message, or begins working; a reply nobody
// here sent a message for, such as a subagent's or a background session's, is not read, and one whose chat you sent a message in follows
// this rule with its own profile's Speak replies. Only the reply's words are read, cleaned for speech: not its reasoning, its tool calls or
// their output, nor the notes icebath prints
func (v *voicing) reply(c *chat, response *agentv1.WatchResponse, before, after bool) {
	ctx := v.desk.kit.Env.Ctx
	took := response.GetMessage().GetRole() == sessionsv1.Role_ROLE_USER || !before && after
	if c == v.asked && took {
		v.asked = nil
		if front, _ := v.desk.focused(); front == c {
			v.turn, v.aloud = v.mode.Reply(ctx, c.on()), c
		}
	}
	if c != v.aloud || v.turn == nil {
		return
	}
	if response.GetDelta().GetRestart() {
		v.turn = v.mode.Reply(ctx, c.on())
	}
	if text := response.GetDelta().GetText(); text != "" {
		v.turn.Delta(text)
	}
	if before && !after {
		v.finish()
	}
}

// Words a live transcription heard so far of the chat's recording, for its status line
func (v *voicing) status(c *chat) string {
	if c != v.owner || v.partial == "" {
		return ""
	}
	return v.partial
}

// Mark of voice on the bar for the focused chat, each switch of its profile shown on its own: the mark of voice input dim while it is off,
// cool while on, warm while transcribing and the live mark while recording into this chat; the speech mark after it while Speak replies is
// on and a voice can read replies aloud, whether voice input is on or not. A click records while voice input is on
func (v *voicing) dial(status *wm.Status) {
	glyphs := v.desk.kit.Env.Theme.Glyphs
	c, _ := v.desk.focused()
	mode := v.mode.Settings().GetMode()
	on := c != nil && mode.GetInput()
	into := c != nil && c == v.owner
	mark, style := cmp.Or(glyphs.Voice, "~"), wm.Dim
	switch {
	case into && v.mode.Recording():
		mark, style = glyphs.Live, wm.Bad
	case into && (v.mode.Transcribing() || v.state == "transcribing"):
		style = wm.Warm
	case on:
		style = wm.Cool
	}
	if c != nil && mode.GetSpeak() && v.mode.Settings().GetSpeaking().GetReady() {
		mark += glyphs.Speak
	}
	segment := wm.Segment{Text: mark, Style: style}
	if on {
		segment.Action = voiceAction
	}
	status.Dials = append(status.Dials, segment)
}

// What the chat says while the last recording is still being turned into text
const stillHearing = "Your last recording is still being turned into text. Try again in a moment."

// /voice: on, off and tts are shortcuts to the switches Voice input and Speak replies, saying what the switch says; anything else opens the
// page of Voice
func (v *voicing) command(kit *tui.Kit, argument string) tea.Cmd {
	named, action := profile.Split(argument)
	c, _ := v.desk.focused()
	if named == "" && c != nil {
		named = c.on()
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "on":
		return v.on(kit, c, named)
	case "off":
		return tea.Batch(v.off(false), v.change(c, named, map[string]bool{speechconfig.Input: false}, func(*speechv1.VoiceReply) tea.Cmd {
			return v.say(c, "Voice input is now Off.")
		}))
	case "tts":
		return v.speak(kit, c, named)
	}
	return v.page(named, "")
}

// /voice on: Voice input on and one line saying how to talk, when voice can hear you; else the page of Voice, which says the one thing to do,
// and nothing changed
func (v *voicing) on(kit *tui.Kit, c *chat, named string) tea.Cmd {
	ctx, key := kit.Env.Ctx, v.keyLabel()
	return tui.Async(func() func() tea.Cmd {
		_, hearing, err := v.look(ctx, named, key)
		return func() tea.Cmd {
			switch {
			case err != nil:
				return v.say(c, "Voice input was not turned on, because the voice settings could not be read. The error was: "+
					strings.TrimSuffix(tui.Reason(err), ".")+".")
			case !hearing.Ready():
				return v.page(named, "")
			}
			if c != nil {
				v.claim(c)
			}
			return v.change(c, named, map[string]bool{speechconfig.Input: true}, func(*speechv1.VoiceReply) tea.Cmd {
				return v.say(c, "Voice input is on. Press "+v.keyLabel()+" to talk.")
			})
		}
	})
}

// /voice tts: Speak replies turned the other way, saying what the switch says; turning it on while no voice can read replies aloud opens the
// page of Voice on Text to speech, which says why, and changes nothing
func (v *voicing) speak(kit *tui.Kit, c *chat, named string) tea.Cmd {
	ctx := kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		settings, err := v.client.Voice(ctx, &speechv1.VoiceRequest{Profile: named})
		return func() tea.Cmd {
			if err != nil {
				return v.say(c, "Speak replies was not changed, because the voice settings could not be read. The error was: "+
					strings.TrimSuffix(tui.Reason(err), ".")+".")
			}
			speak := !settings.GetMode().GetSpeak()
			if speak && !settings.GetSpeaking().GetReady() {
				return v.page(named, speechapp.AtSpeaking)
			}
			if !speak {
				v.mode.Hush()
			}
			return v.change(c, named, map[string]bool{speechconfig.SpeakReplies: speak}, func(now *speechv1.VoiceReply) tea.Cmd {
				return v.say(c, "Speak replies is now "+strs.OnOff(now.GetMode().GetSpeak())+".")
			})
		}
	})
}

// Arms the wake word on this app as its setting asks, quietly
func (v *voicing) arm() tea.Cmd {
	v.listening = true
	return tui.Async(func() func() tea.Cmd {
		start := v.mode.StartWake(v.desk.kit.Env.Ctx, "tui", false)
		return func() tea.Cmd {
			v.listening = start.Started
			return nil
		}
	})
}

// /wake: on and off are shortcuts to the switch Listen hands-free, saying what the switch says; on while the wake word can't listen here
// opens its page, which says why, and changes nothing; anything else opens its page
func (v *voicing) wake(kit *tui.Kit, argument string) tea.Cmd {
	named, action := profile.Split(argument)
	if named == "" {
		named = v.profile()
	}
	ctx := kit.Env.Ctx
	save := func(on bool) error {
		value, _ := yaml.Marshal(on)
		_, err := v.desk.settings.Set(ctx, &configv1.SetRequest{Key: speechconfig.WakeEnabled, Value: string(value), Profile: named})
		if err == nil {
			_, err = v.mode.Refresh(ctx)
		}
		return err
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "on":
		v.quiet = false
		return tui.Async(func() func() tea.Cmd {
			v.mode.Refresh(ctx)
			// The one answer the switch on the page of the wake word follows too: with a step missing, that page opens on the row that
			// does it
			check := v.mode.WakeCheck()
			if !check.Available {
				return func() tea.Cmd { return v.page(named, speechapp.AtWakeOn) }
			}
			start := v.mode.StartWake(ctx, "tui", true)
			var err error
			if start.Started {
				err = save(true)
			}
			return func() tea.Cmd {
				v.listening = start.Started
				switch {
				case start.Reason == voice.WakeElsewhere:
					return v.page(named, speechapp.AtWake)
				case !start.Started:
					return v.note(cmp.Or(start.Hint, "The wake word did not start."))
				case err != nil:
					return v.note("Listen hands-free was not saved. The error was: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
				}
				return v.note("Listen hands-free is now On. Say “" + start.Phrase + "” to talk.")
			}
		})
	case "off":
		v.quiet = true
		return tui.Async(func() func() tea.Cmd {
			v.mode.StopWake()
			err := save(false)
			return func() tea.Cmd {
				v.listening = false
				if err != nil {
					return v.note("Listen hands-free was not saved. The error was: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
				}
				return v.note("Listen hands-free is now Off.")
			}
		})
	}
	return v.page(named, speechapp.AtWake)
}

// The wake phrase was heard, and a recording starts in the chat it talks to: the chat in front; when the phrases of the profiles are told
// apart and the phrase named another profile than the chat in front is on, the chat of that profile that was in front last; with no chat
// in front, the chat that was in front last. A new chat opens only when Start a new session is on, or when no chat (of the profile the
// phrase named) is open. A chat that is busy gets the words as it would get them typed: queued, steering or stopping its reply as Busy input
// says. One line in that chat says what happened
func (v *voicing) woke(phrase, owner string, routed, fresh bool) tea.Cmd {
	named := ""
	if routed {
		named = owner
	}
	c := v.chatFor(named)
	if fresh || c == nil {
		c = v.desk.start(nil, owner)
		v.claim(c)
		return tea.Batch(v.desk.begin(), v.desk.kit.WM.OpenHere(c), v.listen(c, phrase, wokeNew))
	}
	v.claim(c)
	if front, _ := v.desk.focused(); front != c {
		return tea.Batch(v.desk.kit.WM.Focus(c), v.listen(c, phrase, wokeBrought))
	}
	return v.listen(c, phrase, wokeHere)
}

// What became of the chat the wake phrase talks to, as the line in it says
const (
	wokeHere    = "this chat is listening"
	wokeBrought = "this chat came to the front and is listening"
	wokeNew     = "this new chat is listening"
)

// The one line a chat says once the wake phrase started a recording in it
func wokeNote(phrase, what string) string {
	return "You said “" + strings.Trim(strings.TrimSpace(phrase), ".,!?;: ") + "”, so " + what + "."
}

// The chat the wake phrase talks to: the chat in front when it is on the profile named, or none is named; else the open chat on that
// profile that was in front last; nil when none is open
func (v *voicing) chatFor(named string) *chat {
	on := func(c *chat) bool { return named == "" || cmp.Or(c.on(), v.home()) == named }
	if c, ok := v.desk.focused(); ok && on(c) {
		return c
	}
	var last *chat
	for _, c := range v.desk.chats {
		if on(c) && (last == nil || v.fronts[c] > v.fronts[last]) {
			last = c
		}
	}
	return last
}

// Name of the profile a chat that names none is on: the profile the voice settings were last read of while voice followed such a chat
func (v *voicing) home() string {
	if current, _ := v.current.Load().(string); current == "" {
		return v.mode.Settings().GetProfile()
	}
	return v.homed
}

// Records in the chat after the wake phrase with the settings of its profile, and says in it what happened; where voice can't hear you,
// the page of Voice opens, which says why. A recording that did not start leaves the wake word listening again
func (v *voicing) listen(c *chat, phrase, what string) tea.Cmd {
	ctx := v.desk.kit.Env.Ctx
	name, key := c.on(), v.keyLabel()
	// Voice follows the chat it records into from now, before the window shows it in front
	v.current.Store(name)
	return tui.Async(func() func() tea.Cmd {
		_, hearing, err := v.look(ctx, name, key)
		ready := err == nil && hearing.Ready()
		started := false
		if err != nil {
			err = fmt.Errorf("The recording did not start, because the voice settings could not be read. The error was: %s.",
				strings.TrimSuffix(tui.Reason(err), "."))
		} else if ready {
			if _, err = v.mode.Refresh(ctx); err == nil {
				started, err = v.mode.Record(ctx, false)
			}
		}
		if !started {
			v.mode.ResumeWake()
		}
		return func() tea.Cmd {
			cmds := []tea.Cmd{v.settle()}
			switch {
			case err != nil:
				c.log.note(tui.Reason(err))
			case !ready:
				cmds = append(cmds, v.page(name, ""))
			case !started:
				c.log.note(stillHearing)
			default:
				c.log.note(wokeNote(phrase, what))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Frees the microphone and speakers as the app exits, the voice no longer kept warm
func (v *voicing) close() {
	v.mode.Close()
	v.listening = false
	if v.desk.kit != nil && v.leased != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(v.desk.kit.Env.Ctx), 2*time.Second)
		defer cancel()
		lease(ctx, v.client, v.leased, false)
	}
	v.leased = ""
}

// Message the person spoke, sent on the voice chat route as one typed would be
func (c *chat) sendSpoken(text string) tea.Cmd {
	c.voiced = true
	defer func() { c.voiced = false }()
	return c.submit(text)
}

// Line that shows for a few seconds in place of the folder
func toast(kit *tui.Kit, text string) tea.Cmd {
	return kit.WM.Toast(text, wm.Info, 6*time.Second)
}
