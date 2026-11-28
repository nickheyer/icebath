// Page of Voice in the settings menu: voice input and spoken replies, the pages of speech to text, text to speech and the wake word with
// their services and listeners, each set up there, and the options of each
package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1/packagesv1connect"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

const (
	// Page of Voice, the area of the settings menu
	Voice = "voice"
	// Action of the key that starts and stops a recording, which the page of Voice shows and Keyboard shortcuts changes
	RecordAction = "tui.voice"
)

// Where the page of Voice opens, as the word after the profile in its argument
const (
	// On Voice input, saying how to talk once it is on
	AtInput = "input"
	// On Text to speech, under the line that says what keeps replies from being read aloud
	AtSpeaking = "speaking"
	// The page of Speech to text
	AtRecognition = "recognition"
	// The page of Text to speech
	AtSpeech = "speech"
	// The page of the wake word
	AtWake = "wake"
	// The page of the wake word as turning it on lands there with a step missing: on the row that does the step, saying what to do first
	AtWakeOn = "wake-on"
)

// Screen of the keyboard shortcuts, where the record key is changed
const keysScreen = "keys"

// The page of Voice as the screen of its area, over the speech service of the connection, the speech of this machine and the voice input
// of this app, whose wake word listens for the profile it follows
func Screen(conn *connect.Client, sp *speech.Speech, mode *voice.Mode) tui.Screen {
	service := speechv1connect.NewSpeechServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	packages := packagesv1connect.NewPackageServiceClient(conn)
	return tui.Screen{
		ID:    Voice,
		Group: tui.GroupVoice,
		Order: 10,
		Title: "Voice",
		Short: "Talk to the agent and hear its replies",
		Area:  speechconfig.Voice,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return voicePages(kit, service, sp, name) },
		Pickers: map[string]tui.Picker{
			speechconfig.Microphone: {Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return microphones(kit, hub, ask) }},
		},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			p := &page{kit: kit, sp: sp, mode: mode, service: service, settings: settings, packages: packages, profile: name}
			return p.open(strings.ToLower(strings.TrimSpace(target)))
		},
	}
}

// The page of Voice for one profile, and what every page inside it reads
type page struct {
	kit      *tui.Kit
	sp       *speech.Speech
	mode     *voice.Mode
	service  speechv1connect.SpeechServiceClient
	settings configv1connect.ConfigServiceClient
	packages packagesv1connect.PackageServiceClient
	// Profile the page is of, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	// Option rows, as Settings draws and changes them
	options *configapp.Rows
	// Lists over the hub, read again whenever the page is, and the page of the wake word while it is open
	views    cards.Watched
	wakeList *cards.List
	// What the server answered of voice and of its services, and what this machine adds: whether voice can hear you and the wake word
	// listen here
	voice    *speechv1.VoiceReply
	services *speechv1.ServicesReply
	hearing  voice.Hearing
	wake     voice.WakeCheck
	// The wake word of this app as it listens now, nil while it follows another profile
	live   *voice.WakeState
	loaded bool
	err    error
}

// Reads voice, its services and the option rows, then draws every list again and runs then once both are read, so the rows it goes to are
// the rows shown
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	ctx := p.kit.Env.Ctx
	key := p.kit.Env.Keys.Display(RecordAction)
	waiting := 2
	done := func() tea.Cmd {
		if waiting--; waiting > 0 || then == nil || p.err != nil {
			return nil
		}
		return then()
	}
	read := tui.Async(func() func() tea.Cmd {
		reply, err := p.service.Voice(ctx, &speechv1.VoiceRequest{Profile: p.profile})
		var services *speechv1.ServicesReply
		if err == nil {
			services, err = p.service.Services(ctx, &speechv1.ServicesRequest{Profile: p.profile})
		}
		var hearing voice.Hearing
		var wake voice.WakeCheck
		var live *voice.WakeState
		if err == nil {
			hearing, wake = voice.Hears(reply, key), voice.CheckWake(p.sp, reply)
			if p.mode != nil && p.mode.Settings().GetProfile() == reply.GetProfile() {
				state := p.mode.WakeStatus()
				live = &state
			}
		}
		return func() tea.Cmd {
			p.loaded, p.err = true, err
			if err == nil {
				p.voice, p.services, p.hearing, p.wake, p.live = reply, services, hearing, wake, live
			}
			p.refresh()
			return done()
		}
	})
	return tea.Batch(read, p.options.Load(done))
}

// Draws every list of the page again
func (p *page) refresh() {
	p.views.Refresh()
}

// Keeps a list among those drawn again on every read
func (p *page) watch(view *cards.List) {
	p.views.Add(view)
	p.options.Watch(view)
}

// Saves values of settings by key, a nil value putting the default back, then says the note and reads the page again
func (p *page) write(note string, values map[string]any) tea.Cmd {
	return tui.Call(p.hub, func() (bool, error) {
		ctx := p.kit.Env.Ctx
		for key, value := range values {
			if value == nil {
				if _, err := p.settings.Unset(ctx, &configv1.UnsetRequest{Key: key, Profile: p.profile}); err != nil {
					return false, err
				}
				continue
			}
			text, err := encode(value)
			if err != nil {
				return false, err
			}
			if _, err := p.settings.Set(ctx, &configv1.SetRequest{Key: key, Value: text, Profile: p.profile}); err != nil {
				return false, err
			}
		}
		return true, nil
	}, func(bool) tea.Cmd {
		return tea.Batch(p.hub.Done(note), p.load(nil), p.follow(values))
	})
}

// Tells the screens that follow the settings what the keys hold now, when the voice input of this app follows the profile of the page
func (p *page) follow(values map[string]any) tea.Cmd {
	if p.mode == nil || p.voice == nil || p.mode.Settings().GetProfile() != p.voice.GetProfile() {
		return nil
	}
	var cmds []tea.Cmd
	for key, value := range values {
		text := "null"
		if value != nil {
			text, _ = encode(value)
		}
		cmds = append(cmds, p.kit.Changed(key, text))
	}
	return tea.Batch(cmds...)
}
