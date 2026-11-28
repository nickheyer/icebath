package app

import (
	"slices"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	agentlaunch "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/launch"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/speech"
	speechapp "github.com/nickheyer/icebath/internal/speech/app"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1/slashv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of past sessions
const sessionsScreen = "sessions"

type desk struct {
	kit       *tui.Kit
	sessions  sessionsv1connect.SessionServiceClient
	agents    agentv1connect.AgentServiceClient
	slash     slashv1connect.SlashServiceClient
	providers providerv1connect.ProviderServiceClient
	settings  configv1connect.ConfigServiceClient
	tools     toolsv1connect.ToolServiceClient
	views     *toolsapp.Registry
	// What plugins dock above the prompt of a chat for its session
	docks []toolsapp.Dock
	// What runs a line a person types after a mark of its own, such as a line of their shell after !
	liners []toolsapp.Liner
	// Values of the settings the tool views read, by key
	shown  map[string]string
	chats  []*chat
	opened int
	// Id of this window, which the server counts the sessions it holds open by
	client string
	// Sessions chats here left running on the server, by id, with the label notices name them by, and what every open session did at the
	// last look
	left   map[string]string
	phases map[string]string
	// Open sessions no window here shows
	unseen int
	// This terminal keeps a breadcrumb of the session it shows, to continue it there
	crumbs bool
	// The session the app was launched to resume is open
	resumed bool
	// The look at what the server's open sessions do is running
	polling bool
	// What the launch of the app asked of the sessions it starts or resumes, and the sessions told so already
	asked    *agentlaunch.Asked
	mu       sync.Mutex
	launched map[string]bool
	// Speech of this machine, whose players and wake engines voice input runs on, and voice input itself
	speech *speech.Speech
	voice  *voicing
}

func Register(registry *app.Registry, views *toolsapp.Registry, asked *agentlaunch.Asked, sp *speech.Speech) {
	views.Register(delegateView{})
	registry.Register(&desk{views: views, asked: asked, launched: map[string]bool{}, speech: sp})
}

func (d *desk) Screens(conn *connect.Client) []tui.Screen {
	d.sessions = sessionsv1connect.NewSessionServiceClient(conn)
	d.agents = agentv1connect.NewAgentServiceClient(conn)
	d.slash = slashv1connect.NewSlashServiceClient(conn)
	d.providers = providerv1connect.NewProviderServiceClient(conn)
	d.settings = configv1connect.NewConfigServiceClient(conn)
	d.tools = toolsv1connect.NewToolServiceClient(conn)
	d.client = windowID()
	d.docks = d.views.Docks(conn)
	d.liners = d.views.Lines(conn)
	d.left, d.phases, d.shown = map[string]string{}, map[string]string{}, map[string]string{}
	d.voice = d.voicing(conn, d.speech)
	follows := map[string]func(*tui.Kit, string) tea.Cmd{sessionsconfig.TerminalContinue: d.follow,
		speechconfig.Input: func(kit *tui.Kit, value string) tea.Cmd { return d.at(kit).voice.follow(value) }}
	// Voice input follows the settings it reads as they change
	for _, key := range []string{speechconfig.SpeakReplies, speechconfig.SubmitMode, speechconfig.BargeIn, speechconfig.BargeInGrace,
		speechconfig.BargeInMultiplier, speechconfig.Beep, speechconfig.BeepVolume, speechconfig.Debug, speechconfig.MaxRecording,
		speechconfig.SilenceDuration, speechconfig.SilenceThreshold, speechconfig.StopPhrases, speechconfig.ThinkingSound, speechconfig.TTSProvider,
		speechconfig.StreamingMinLen, speechconfig.STTEnabled, speechconfig.STTProvider, speechconfig.Streaming, speechconfig.WakeEnabled,
		speechconfig.WakeSurface, speechconfig.WakeInput, speechconfig.WakeProvider, speechconfig.WakePhrase, speechconfig.WakeSensitivity,
		speechconfig.WakeConfirmationFrames, speechconfig.WakeNewSession, speechconfig.WakeProfileRouting} {
		follows[key] = func(kit *tui.Kit, _ string) tea.Cmd { return d.at(kit).voice.refresh() }
	}
	for _, key := range d.views.Settings() {
		follows[key] = func(_ *tui.Kit, value string) tea.Cmd {
			d.shown[key] = value
			return nil
		}
	}
	chatPlaces := map[string]string{approvalContext: "approvals", questionContext: "agent's questions", rewindContext: "rewind list",
		spawnContext: "spawn tree"}
	return []tui.Screen{
		{
			ID:      "chat",
			Group:   tui.GroupSessions,
			Order:   10,
			Title:   "New chat",
			Short:   "Open a new chat on this profile or another",
			Keys:    []string{"alt+n"},
			Actions: slices.Concat(approvalKeys, questionKeys, rewindKeys, dockKeys, compactKeys, spawnKeys, voiceKeys),
			Where:   chatPlaces,
			Startup: true,
			Suggest: true,
			Follows: follows,
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				d.at(kit)
				if id := sessions.Resumed(kit.Env.Ctx); id != "" && !d.resumed {
					d.resumed = true
					return tea.Batch(d.begin(), kit.WM.OpenHere(d.start(&sessionsv1.Session{Id: id}, "")))
				}
				name, _ := profile.Split(argument)
				return tea.Batch(d.begin(), kit.WM.OpenHere(d.start(nil, name)))
			},
		},
		// The page of Voice, which the voice input of this app is shown and set up on
		speechapp.Screen(conn, d.speech, d.voice.mode),
		// The commands of Voice, whose area the palette lists: /voice and /wake, then /recognition and /speech, more names of /voice that
		// the slash list leaves out and that open their pages
		{
			ID:    "voice.command",
			Short: "Talk to the agent and hear its replies",
			Slash: []string{"voice"},
			Usage: "[on|off|tts]",
			Open:  func(kit *tui.Kit, argument string) tea.Cmd { return d.at(kit).voice.command(kit, argument) },
		},
		{
			ID:    "wake.command",
			Short: "Say a phrase to start talking, hands-free",
			Slash: []string{"wake"},
			Usage: "[on|off]",
			Open:  func(kit *tui.Kit, argument string) tea.Cmd { return d.at(kit).voice.wake(kit, argument) },
		},
		{
			ID:      "voice.recognition",
			Aliases: []string{"recognition"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				return d.at(kit).voice.pageOf(argument, speechapp.AtRecognition)
			},
		},
		{
			ID:      "voice.speech",
			Aliases: []string{"speech"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				return d.at(kit).voice.pageOf(argument, speechapp.AtSpeech)
			},
		},
		{
			ID:      sessionsScreen,
			Group:   tui.GroupSessions,
			Order:   20,
			Title:   "Sessions",
			Short:   "Find a session and open, rename or archive it",
			Slash:   []string{"sessions", "switch", "session"},
			Usage:   "[words to search]",
			Keys:    []string{"alt+s"},
			Actions: sessionsKeys,
			Where:   map[string]string{sessionsContext: "sessions"},
			Suggest: true,
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				from, _ := kit.Focused().(*chat)
				browse := &browser{desk: d.at(kit), from: from}
				// One long word is taken for a session id, which opens straight away when one matches
				if words := strings.Fields(argument); len(words) == 1 && len(words[0]) >= 16 && from != nil {
					return tea.Batch(d.begin(), browse.resume(words[0]))
				}
				return tea.Batch(d.begin(), browse.open(argument))
			},
		},
		{
			ID:    "model",
			Group: tui.GroupModels,
			Order: 10,
			Title: "Model",
			Short: "Switch the model of this chat",
			Keys:  []string{"alt+m"},
			// The job /model opens: the model of the chat in hand, or of new chats with no chat in hand
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				if target, ok := kit.Focused().(*chat); ok {
					cmd, _ := target.model("")
					return cmd
				}
				return kit.Open(agentScreen, jobModel)
			},
		},
		d.agentArea(),
		d.helperArea(),
		d.subagentArea(),
	}
}

func (d *desk) at(kit *tui.Kit) *desk {
	if d.kit == nil {
		d.kit = kit
		kit.Report(d.report)
	}
	return d
}

// Opens a chat of the session, or of a session to be made on the profile
func (d *desk) start(session *sessionsv1.Session, profile string) *chat {
	d.opened++
	c := newChat(d, "chat:"+strconv.Itoa(d.opened), session, profile)
	d.chats = append(d.chats, c)
	return c
}

func (d *desk) closed(c *chat) tea.Cmd {
	d.chats = slices.DeleteFunc(d.chats, func(open *chat) bool { return open == c })
	// Voice lets go of the window: a recording into it ends with its setting kept, and its reply is no longer read aloud
	return d.voice.closed(c)
}

// Lets go of what the app holds as it exits: the microphone, the speakers and the wake word
func (d *desk) Close() error {
	if d.voice != nil {
		d.voice.close()
	}
	return nil
}

// Opens the session in a window of its own, or focuses the window that already shows it
func (d *desk) open(session *sessionsv1.Session) tea.Cmd {
	for _, c := range d.chats {
		if c.id == session.GetId() {
			return d.kit.WM.Focus(c)
		}
	}
	return d.kit.WM.OpenHere(d.start(session, ""))
}

func (d *desk) report(status *wm.Status) {
	for _, c := range d.chats {
		space := d.kit.WM.WorkspaceOf(c)
		if space == nil {
			continue
		}
		signal := wm.Idle
		switch {
		case c.log.busy:
			signal = wm.Working
		case c.unseen:
			signal = wm.Fresh
		}
		if lane := status.Lanes[space]; signal > lane.Signal {
			lane.Signal = signal
			status.Lanes[space] = lane
		}
	}
	d.voice.dial(status)
	d.dial(status)
}

// Whether the session was told already what the launch of the app asked of it, which the first message this app sends it carries
func (d *desk) told(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	told := d.launched[id]
	d.launched[id] = true
	return told
}
