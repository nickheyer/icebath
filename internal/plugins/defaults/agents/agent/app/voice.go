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

	"github.com/nickheyer/icebath/internal/speech"
	speechapp "github.com/nickheyer/icebath/internal/speech/app"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Action that starts and stops a recording, and the key it has of its own
	voiceAction = "tui.voice"
	voiceKey    = "alt+r"
	// Lease that keeps the voice warm while this window speaks replies
	voiceLease = "tui:voice-tts"
)

var voiceKeys = []keys.Action{
	{ID: voiceAction, Context: keys.Session, Label: "voice", Doc: "start or stop voice recording", Default: []string{voiceKey}},
}

// Contexts a chord of the voice key competes in besides the global one, those of a chat
var voiceContexts = []string{keys.Session, keys.Text}

// Letters voice.record_key may not take with a modifier, kept for the keys that already do something there: ctrl everywhere, the command
// key and option on a Mac
var (
	reservedCtrl     = "cdlrx"
	reservedSuper    = "cdlv"
	reservedAltOnMac = "cdl"
)

// Voice mode of this app: one microphone and one speaker, held by the chat voice mode was turned on in
type voicing struct {
	desk   *desk
	mode   *voice.Mode
	client speechv1connect.SpeechServiceClient
	// Chat voice mode is on in, nil while it is off in this window; the voice input setting was on, so the next chat focused takes it
	owner *chat
	adopt bool
	// Speech of the reply the owner's session writes now
	turn *voice.Turn
	// listening, transcribing or idle, and the words a live transcription heard so far
	state   string
	partial string
	// Chord and holder the voice key notice last named, so it is told once
	warned string
	// The wake word listens here; the person stopped it with /wake off, which keeps it from arming on its own
	listening bool
	quiet     bool
	// Profile the voice is kept warm on, empty while it isn't
	leased string
	// A read of the settings runs, and another was asked for meanwhile
	reading bool
	again   bool
	// Profile the engine reads settings of and the chat voice mode is on in, kept by the app's goroutine for the engine's
	current atomic.Value
	owned   atomic.Pointer[chat]
}

// Voice mode over the speech service of the connection and the speech host of this machine
func (d *desk) voicing(conn *connect.Client, sp *speech.Speech) *voicing {
	v := &voicing{desk: d, state: "idle", client: speechv1connect.NewSpeechServiceClient(conn)}
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
		Stopped:    func(string) { apply(v.stopped) },
		Silent: func() {
			apply(func() tea.Cmd {
				return tea.Batch(v.note("voice: no speech detected 3 times, continuous mode stopped"), v.off(false, true))
			})
		},
		Interrupted: func() {
			apply(func() tea.Cmd {
				if v.owner == nil {
					return nil
				}
				return tea.Batch(toast(d.kit, "voice interrupted the reply"), v.owner.interrupt())
			})
		},
		Woke: func(phrase, profile string, fresh bool) {
			apply(func() tea.Cmd { return v.woke(phrase, profile, fresh) })
		},
		Notice: func(text string) { apply(func() tea.Cmd { return v.note(text) }) },
	})
	v.mode.Busy(func() bool {
		c := v.owned.Load()
		return c != nil && c.working.Load()
	})
	return v
}

// Keeps the profile voice mode reads its settings from where the engine's goroutines read it
func (v *voicing) track() {
	v.current.Store(v.profile())
	v.owned.Store(v.owner)
}

// Profile voice mode reads its settings from: the owner's, else the focused chat's, else the one in use
func (v *voicing) profile() string {
	if v.owner != nil {
		return v.owner.on()
	}
	if c, ok := v.desk.focused(); ok {
		return c.on()
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

// Reads the settings of voice mode again, one read at a time, then follows them
func (v *voicing) refresh() tea.Cmd {
	if v.reading {
		v.again = true
		return nil
	}
	v.reading = true
	v.track()
	return tui.Async(func() func() tea.Cmd {
		_, err := v.mode.Refresh(v.desk.kit.Env.Ctx)
		return func() tea.Cmd {
			v.reading = false
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

// The voice input setting changed, or the app started with it: on, the next chat focused takes voice mode; off, voice mode ends here
func (v *voicing) follow(value string) tea.Cmd {
	var on bool
	yaml.Unmarshal([]byte(value), &on)
	v.adopt = on && v.owner == nil
	if !on && v.owner != nil {
		return tea.Batch(v.off(false, false), v.refresh())
	}
	return v.refresh()
}

// Voice mode as the settings now stand: a chat taking it that waits to, and the voice kept warm while this window speaks replies
func (v *voicing) settle() tea.Cmd {
	mode := v.mode.Settings().GetMode()
	if v.adopt && v.owner == nil && mode.GetInput() {
		if c, ok := v.desk.focused(); ok {
			v.owner, v.adopt = c, false
			v.track()
		}
	}
	return v.warm(v.owner != nil && mode.GetInput() && mode.GetSpeak())
}

// A chat took the focus, which takes voice mode when the voice input setting is on and no chat has it yet
func (v *voicing) focused(c *chat) tea.Cmd {
	defer v.track()
	if !v.adopt || v.owner != nil || !v.mode.Settings().GetMode().GetInput() {
		return nil
	}
	v.owner, v.adopt = c, false
	return v.settle()
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
		return v.arm(false)
	case !enabled && v.listening:
		v.mode.StopWake()
		v.listening = false
	}
	return nil
}

// Binds the chord voice.record_key names besides the voice key, refused with a notice once when it is invalid or reserved, or another key of
// a chat holds it
func (v *voicing) bind(value string) tea.Cmd {
	var text string
	yaml.Unmarshal([]byte(value), &text)
	bindings := v.desk.kit.Env.Keys
	defaults := []string{voiceKey}
	var taken, holder string
	if chord := recordChord(text, runtime.GOOS == "darwin"); chord != "" && chord != voiceKey {
		for _, other := range bindings.Lookup(voiceContexts, keys.Press(chord)) {
			if other != voiceAction {
				holder = other
				break
			}
		}
		if holder == "" {
			defaults = append(defaults, chord)
		} else {
			taken = chord
		}
	}
	var cmds []tea.Cmd
	if err := bindings.SetDefault(voiceAction, defaults); err != nil {
		cmds = append(cmds, v.note(err.Error()))
	}
	if taken != "" {
		if warned := keys.Format(taken) + ":" + holder; warned != v.warned {
			v.warned = warned
			act, _ := bindings.Action(holder)
			cmds = append(cmds, v.note(fmt.Sprintf("The voice key %s already does %s, so it is left unbound. Record with %s, or pick another key in /settings voice.",
				keys.Format(taken), act.LabelText(), v.keyLabel())))
		}
	}
	return tea.Batch(cmds...)
}

// Chord of a voice.record_key value: a modifier and a letter or a named key, none for anything invalid or reserved here
func recordChord(text string, mac bool) string {
	var parts []string
	for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(text)), "+") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) != 2 {
		return ""
	}
	mod, ok := map[string]string{"alt": "alt", "control": "ctrl", "ctrl": "ctrl", "option": "alt", "opt": "alt", "super": "super", "win": "super",
		"windows": "super"}[parts[0]]
	if !ok {
		return ""
	}
	key := parts[1]
	if single := len([]rune(key)) == 1; single {
		if mod == "ctrl" && strings.Contains(reservedCtrl, key) || mac && mod == "super" && strings.Contains(reservedSuper, key) ||
			mac && mod == "alt" && strings.Contains(reservedAltOnMac, key) {
			return ""
		}
	} else if key, ok = map[string]string{"backspace": "backspace", "bs": "backspace", "del": "delete", "delete": "delete", "enter": "enter", "esc": "esc",
		"escape": "esc", "ret": "enter", "return": "enter", "space": "space", "spc": "space", "tab": "tab"}[key]; !ok {
		return ""
	}
	chord, err := keys.Parse(mod + "+" + key)
	if err != nil {
		return ""
	}
	return chord
}

// Keys that record, as /voice names them
func (v *voicing) keyLabel() string {
	return cmp.Or(v.desk.kit.Env.Keys.Display(voiceAction), "a key bound to voice in /keys")
}

// Voice mode moves to the chat: a recording of the chat that had it ends, and its reply is no longer spoken
func (v *voicing) claim(c *chat) {
	if v.owner == c {
		return
	}
	if v.owner != nil {
		v.mode.Stop(v.desk.kit.Env.Ctx, false)
		v.finish()
	}
	v.owner, v.adopt = c, false
	v.track()
}

// Ends the speech of the reply, what is queued still said
func (v *voicing) finish() {
	if v.turn != nil {
		v.turn.Finish()
		v.turn = nil
	}
}

// Starts or stops a recording in the chat, which voice mode must be on in: one utterance a press, sent once its speaker falls silent
func (v *voicing) toggle(c *chat) tea.Cmd {
	if v.owner != c || !v.mode.Settings().GetMode().GetInput() {
		c.log.note("voice: mode is off — enable with /voice on")
		return nil
	}
	ctx := v.desk.kit.Env.Ctx
	if v.mode.Recording() {
		go v.mode.Stop(ctx, true)
		return nil
	}
	if v.mode.Transcribing() {
		c.log.note("voice: still transcribing; try again shortly")
		return nil
	}
	if req := v.mode.Requirements(); !req.Available {
		c.log.note("voice error: " + strings.ReplaceAll(req.Details, "\n", "; "))
		return nil
	}
	go func() {
		started, err := v.mode.Record(ctx, false)
		v.desk.kit.Apply(func() tea.Cmd {
			switch {
			case err != nil:
				c.log.note("voice error: " + err.Error())
			case !started:
				c.log.note("voice: still transcribing; try again shortly")
			}
			return nil
		})
	}()
	return nil
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
	return tea.Batch(c.composer.SetValue(""), c.sendSpoken(text))
}

// Whether a message typed in the chat is only a stop phrase while voice mode is on there, which ends voice chat instead of being sent
func (v *voicing) typedStop(c *chat, text string) bool {
	return v.owner == c && v.mode.Settings().GetMode().GetInput() && v.mode.StopPhrase(text)
}

// A stop phrase ended voice chat: voice mode and spoken replies off and kept off
func (v *voicing) stopped() tea.Cmd {
	return tea.Batch(v.note("voice: stop phrase — voice chat ended"), v.off(true, false))
}

// Whether the next message cuts a spoken reply off or follows one that was, which every message tells the model; speech still playing is cut
func (v *voicing) cut() bool {
	return v.mode.TakeInterrupted()
}

// Turns a voice setting of the chat's profile on or off, then reads the settings again and follows them
func (v *voicing) set(c *chat, key string, on bool) tea.Cmd {
	return v.change(c, map[string]bool{key: on}, nil)
}

// Saves voice settings of the chat's profile, reads the settings again and follows them, then runs after on what they came to
func (v *voicing) change(c *chat, values map[string]bool, after func(settings *speechv1.VoiceReply) tea.Cmd) tea.Cmd {
	v.track()
	profile := c.on()
	ctx := v.desk.kit.Env.Ctx
	return tui.Async(func() func() tea.Cmd {
		var err error
		for key, on := range values {
			value, _ := yaml.Marshal(on)
			if _, err = v.desk.settings.Set(ctx, &configv1.SetRequest{Key: key, Value: string(value), Profile: profile}); err != nil {
				break
			}
		}
		var settings *speechv1.VoiceReply
		if err == nil {
			settings, err = v.mode.Refresh(ctx)
		}
		return func() tea.Cmd {
			if err != nil {
				c.log.note("voice error: " + tui.Reason(err))
				return nil
			}
			cmds := []tea.Cmd{v.settle()}
			if after != nil {
				cmds = append(cmds, after(settings))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Voice mode off in this window: recording stopped, the reply's speech silenced or, when asked, left to finish, the voice no longer kept
// warm; voice input and spoken replies saved off when asked
func (v *voicing) off(persist, finish bool) tea.Cmd {
	c := v.owner
	ctx := v.desk.kit.Env.Ctx
	if finish {
		v.mode.Stop(ctx, false)
		v.finish()
	} else {
		v.mode.Hush()
		v.mode.Off(ctx)
		v.turn = nil
	}
	v.owner, v.adopt, v.state, v.partial = nil, false, "idle", ""
	v.track()
	cmds := []tea.Cmd{v.warm(false)}
	if persist && c != nil {
		cmds = append(cmds, v.change(c, map[string]bool{speechconfig.Input: false, speechconfig.SpeakReplies: false}, nil))
	}
	return tea.Batch(cmds...)
}

// Opens the speech of a reply the owner's session starts, cuts it on a restart, feeds it the reply's words and ends it with the reply
func (v *voicing) reply(c *chat, response *agentv1.WatchResponse, before, after bool) {
	if c != v.owner {
		return
	}
	ctx := v.desk.kit.Env.Ctx
	switch {
	case !before && after:
		v.turn = v.mode.Reply(ctx)
	case response.GetDelta().GetRestart() && v.turn != nil:
		v.mode.Cut()
		v.turn = v.mode.Reply(ctx)
	}
	if text := response.GetDelta().GetText(); text != "" && v.turn != nil {
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

// Mark of voice mode on the bar for the focused chat: dim while off, cool while on, warm while transcribing, the live mark while recording,
// the speech mark after it while replies are spoken; a click records while voice mode is on
func (v *voicing) dial(status *wm.Status) {
	glyphs := v.desk.kit.Env.Theme.Glyphs
	c, _ := v.desk.focused()
	on := c != nil && c == v.owner && v.mode.Settings().GetMode().GetInput()
	mark, style := cmp.Or(glyphs.Voice, "~"), wm.Dim
	switch {
	case on && v.mode.Recording():
		mark, style = glyphs.Live, wm.Bad
	case on && (v.mode.Transcribing() || v.state == "transcribing"):
		style = wm.Warm
	case on:
		style = wm.Cool
	}
	if on && v.mode.Settings().GetMode().GetSpeak() {
		mark += glyphs.Speak
	}
	segment := wm.Segment{Text: mark, Style: style}
	if on {
		segment.Action = voiceAction
	}
	status.Dials = append(status.Dials, segment)
}

// /voice: voice mode of the chat on or off, spoken replies toggled, how it stands, or the voice and recognition set up
func (v *voicing) command(kit *tui.Kit, argument string) tea.Cmd {
	action := strings.ToLower(strings.TrimSpace(argument))
	if action == "setup" {
		return kit.Open(speechapp.Voices, "")
	}
	if action != "on" && action != "off" && action != "tts" {
		action = "status"
	}
	c, ok := v.desk.focused()
	if !ok && action != "status" {
		return toast(kit, "no active session — voice mode belongs to a session; start or resume one first")
	}
	say := func(lines ...string) tea.Cmd {
		if c == nil {
			return toast(kit, strings.Join(lines, " · "))
		}
		c.log.note(strings.Join(lines, "\n"))
		return nil
	}
	switch action {
	case "on":
		v.claim(c)
		return v.change(c, map[string]bool{speechconfig.Input: true}, func(settings *speechv1.VoiceReply) tea.Cmd {
			lines := []string{"Voice mode enabled"}
			if settings.GetMode().GetSpeak() {
				lines[0] = "Voice mode enabled (TTS enabled)"
			}
			lines = append(lines, "  "+v.keyLabel()+" to start/stop recording")
			if hint := v.mode.StopHint(); hint != "" {
				lines = append(lines, "  "+hint)
			}
			return say(append(lines, "  /voice tts  to toggle speech output", "  /voice off  to disable voice mode")...)
		})
	case "off":
		v.owner = c
		return tea.Batch(say("Voice mode disabled."), v.off(true, false))
	case "tts":
		if v.owner != c || !v.mode.Settings().GetMode().GetInput() {
			return say("enable voice mode first: /voice on")
		}
		speak := !v.mode.Settings().GetMode().GetSpeak()
		if !speak {
			v.mode.Hush()
		}
		return v.change(c, map[string]bool{speechconfig.SpeakReplies: speak}, func(settings *speechv1.VoiceReply) tea.Cmd {
			if settings.GetMode().GetSpeak() {
				return say("Voice TTS enabled.")
			}
			return say("Voice TTS disabled.")
		})
	}
	return tui.Async(func() func() tea.Cmd {
		settings, err := v.mode.Refresh(kit.Env.Ctx)
		return func() tea.Cmd {
			if err != nil {
				return say("voice error: " + tui.Reason(err))
			}
			mode := settings.GetMode()
			if c != nil && mode.GetInput() {
				v.claim(c)
			}
			word := map[bool]string{true: "ON", false: "OFF"}
			lines := []string{"Voice Mode Status", "  Mode:       " + word[mode.GetInput()], "  TTS:        " + word[mode.GetSpeak()],
				"  Record key: " + v.keyLabel()}
			if req := v.mode.Requirements(); req.Details != "" {
				lines = append(lines, "", "  Requirements:")
				for _, line := range strings.Split(req.Details, "\n") {
					if strings.TrimSpace(line) != "" {
						lines = append(lines, "    "+line)
					}
				}
			}
			return tea.Batch(v.settle(), say(lines...))
		}
	})
}

// " for “phrase” · engine" of a wake line, each part left out when empty
func wakeDetail(phrase, engine string) string {
	out := ""
	if phrase != "" {
		out = " for “" + phrase + "”"
	}
	if engine != "" {
		out += " · " + engine
	}
	return out
}

// Line with the hint after it when there is one
func withHint(line, hint string) string {
	if hint = strings.TrimSpace(hint); hint != "" {
		return line + " — " + hint
	}
	return line
}

// Arms the wake word on this app; the person turning it on is told how it went
func (v *voicing) arm(asked bool) tea.Cmd {
	v.listening = true
	return tui.Async(func() func() tea.Cmd {
		start := v.mode.StartWake(v.desk.kit.Env.Ctx, "tui", asked)
		return func() tea.Cmd {
			v.listening = start.Started
			if !asked {
				return nil
			}
			return v.note(v.started(start, false))
		}
	})
}

// What arming the wake word came to, as /wake on says it
func (v *voicing) started(start voice.WakeStart, saved bool) string {
	if !start.Started {
		reason, hint := start.Reason, start.Hint
		switch reason {
		case voice.WakeDisabled:
			reason = "disabled (config wake_word.enabled)"
		case voice.WakeElsewhere:
			reason = "scoped to another surface (config wake_word.surface)"
		case voice.WakeOwned:
			reason, hint = "another surface owns the listener", ""
		case voice.WakeUnavailable:
			reason = "unavailable"
		case "":
			reason = "unknown"
		}
		return withHint("wake: not started — "+reason, hint)
	}
	line := "wake: listening" + wakeDetail(start.Phrase, start.Engine)
	if saved {
		line += " · enabled in config"
	}
	return line
}

// /wake: the wake word listener armed or stopped, kept in the settings, or how it stands
func (v *voicing) wake(kit *tui.Kit, argument string) tea.Cmd {
	c, _ := v.desk.focused()
	ctx := kit.Env.Ctx
	persist := func(on bool) bool {
		value, _ := yaml.Marshal(on)
		profile := ""
		if c != nil {
			profile = c.on()
		}
		_, err := v.desk.settings.Set(ctx, &configv1.SetRequest{Key: speechconfig.WakeEnabled, Value: string(value), Profile: profile})
		if err != nil {
			slog.Warn(fmt.Sprintf("wake: failed to persist %s=%t: %s", speechconfig.WakeEnabled, on, err))
		}
		return err == nil
	}
	switch strings.ToLower(strings.TrimSpace(argument)) {
	case "on":
		v.quiet, v.listening = false, true
		return tui.Async(func() func() tea.Cmd {
			check := v.mode.WakeCheck()
			if !check.Available {
				return func() tea.Cmd {
					v.listening = false
					return v.note(v.started(voice.WakeStart{Reason: voice.WakeUnavailable, Hint: check.Hint}, false))
				}
			}
			saved := !v.mode.Settings().GetWake().GetEnabled() && persist(true)
			if saved {
				v.mode.Refresh(ctx)
			}
			start := v.mode.StartWake(ctx, "tui", true)
			return func() tea.Cmd {
				v.listening = start.Started
				return v.note(v.started(start, saved))
			}
		})
	case "off":
		v.quiet = true
		return tui.Async(func() func() tea.Cmd {
			phrase, engine, stopped := v.mode.StopWake()
			saved := v.mode.Settings().GetWake().GetEnabled() && persist(false)
			if saved {
				v.mode.Refresh(ctx)
			}
			if !stopped {
				phrase, engine = v.mode.Settings().GetWake().GetPhrase(), ""
			}
			detail := wakeDetail(phrase, engine)
			if saved {
				detail += " · disabled in config"
			}
			return func() tea.Cmd {
				v.listening = false
				if stopped {
					return v.note("wake: listener off" + detail)
				}
				return v.note("wake: nothing to stop — not running" + detail)
			}
		})
	case "", "status":
		return tui.Async(func() func() tea.Cmd {
			state := v.mode.WakeStatus()
			detail := wakeDetail(state.Phrase, state.Engine)
			line := "wake: off" + detail + " · /wake on to arm"
			switch {
			case state.Listening && state.Silent:
				line = withHint("wake: listening"+detail+" · ⚠ mic delivers only silence", state.Hint)
			case state.Listening:
				line = "wake: listening" + detail
			case !state.Check.Available:
				line = withHint("wake: unavailable", state.Hint)
			}
			return func() tea.Cmd { return v.note(line) }
		})
	}
	return v.note("usage: /wake [on|off|status]")
}

// The wake phrase was heard: another profile's told of, else voice mode on in the focused chat, or a new one when the settings say so or
// none is open, and a recording started there
func (v *voicing) woke(phrase, profile string, fresh bool) tea.Cmd {
	own := cmp.Or(v.profile(), v.mode.Settings().GetProfile(), "default")
	if profile != "" && profile != own {
		v.mode.ResumeWake()
		return v.note(fmt.Sprintf("wake phrase for profile '%s' — /profiles switches to it", profile))
	}
	c, ok := v.desk.focused()
	if fresh || !ok {
		c = v.desk.start(nil, profile)
		v.claim(c)
		return tea.Batch(v.desk.begin(), v.desk.kit.WM.OpenHere(c), v.listen(c))
	}
	v.claim(c)
	return v.listen(c)
}

// Records in the chat after the wake phrase, voice input saved on first when it is off; a failure told, the wake word listening again
func (v *voicing) listen(c *chat) tea.Cmd {
	ctx := v.desk.kit.Env.Ctx
	profile := c.on()
	return tui.Async(func() func() tea.Cmd {
		var err error
		if !v.mode.Settings().GetMode().GetInput() {
			if _, err = v.desk.settings.Set(ctx, &configv1.SetRequest{Key: speechconfig.Input, Value: "true", Profile: profile}); err == nil {
				_, err = v.mode.Refresh(ctx)
			}
		}
		started := false
		if err == nil {
			started, err = v.mode.Record(ctx, false)
		}
		if err != nil || !started {
			v.mode.ResumeWake()
		}
		return func() tea.Cmd {
			cmds := []tea.Cmd{v.settle()}
			if err != nil {
				cmds = append(cmds, v.note("wake: "+tui.Reason(err)))
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
