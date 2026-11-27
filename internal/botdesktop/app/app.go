// The agent desktop: the profile's own hidden desktop, how it stands, start and stop, a screenshot of it, taking it over and handing it
// back, the install of its programs and the sandbox image it needs
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/internal/app"
	botdesktopconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	botdesktopv1 "github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1/botdesktopv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const screenContext = "tui.screen"

// What /screen takes after its name
const subcommands = "status, start, stop, take, release, shot, install, switch or keep"

var actions = []keys.Action{
	{ID: "tui.screen.main", Context: screenContext, Label: "do", Doc: "Run the first action the agent desktop shows, such as starting it", Default: []string{"enter"}},
	{ID: "tui.screen.start", Context: screenContext, Label: "start", Doc: "Start the agent desktop", Default: []string{"s"}},
	{ID: "tui.screen.stop", Context: screenContext, Label: "stop", Doc: "Hand the agent desktop back and stop it", Default: []string{"x"}},
	{ID: "tui.screen.take", Context: screenContext, Label: "take over", Doc: "Take over the agent desktop, so the agent waits until you hand it back", Default: []string{"t"}},
	{ID: "tui.screen.release", Context: screenContext, Label: "hand back", Doc: "Hand the agent desktop back to the agent", Default: []string{"r"}},
	{ID: "tui.screen.shot", Context: screenContext, Label: "screenshot", Doc: "Save a screenshot of the agent desktop", Default: []string{"p"}},
	{ID: "tui.screen.install", Context: screenContext, Label: "install", Doc: "Install the programs the agent desktop needs", Default: []string{"i"}},
	{ID: "tui.screen.switch", Context: screenContext, Label: "switch image", Doc: "Move the sandbox to an image that has a desktop", Default: []string{"w"}},
	{ID: "tui.screen.keep", Context: screenContext, Label: "keep image", Doc: "Keep the sandbox on the image it runs now", Default: []string{"n"}},
	{ID: "tui.screen.settings", Context: screenContext, Label: "settings", Doc: "Open the settings of the agent desktop", Default: []string{"e"}},
	{ID: "tui.screen.refresh", Context: screenContext, Label: "refresh", Doc: "Check the agent desktop again", Default: []string{"ctrl+r"}},
}

type screens struct {
	mu sync.Mutex
	// Id this app takes the desktop under, random and kept for its life, so only it hands back what it took
	viewer string
}

func Register(host *app.App) {
	host.Register(&screens{})
}

func (s *screens) id() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.viewer == "" {
		raw := make([]byte, 16)
		rand.Read(raw)
		s.viewer = base64.RawURLEncoding.EncodeToString(raw)
	}
	return s.viewer
}

func (s *screens) Screens(conn *connect.Client) []tui.Screen {
	service := botdesktopv1connect.NewDesktopServiceClient(conn)
	return []tui.Screen{{
		ID:      "screen",
		Group:   tui.GroupAbilities,
		Order:   80,
		Title:   "Agent desktop",
		Short:   "Start, stop or take over the agent desktop",
		Slash:   []string{"screen", "desktop"},
		Usage:   "[status|start|stop [force]|take [reason]|release [force]|shot|install|switch|keep] [@profile]",
		Actions: actions,
		Where:   map[string]string{screenContext: "agent desktop"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			v := &view{kit: kit, service: service, profile: name, viewer: s.id()}
			return v.open(strings.TrimSpace(rest))
		},
	}}
}

type view struct {
	kit     *tui.Kit
	service botdesktopv1connect.DesktopServiceClient
	profile string
	viewer  string
	hub     *cards.Hub
	page    *cards.Page
	status  *botdesktopv1.Screen
	// What the actions said, newest last, the install's output among them
	log    []string
	loaded bool
	err    error
	// Ends the watch of the desktop, once the page is gone
	stop context.CancelFunc
}

func (v *view) open(argument string) tea.Cmd {
	run := map[string]func() tea.Cmd{
		"tui.screen.start":    v.start,
		"tui.screen.stop":     func() tea.Cmd { return v.halt(false) },
		"tui.screen.take":     func() tea.Cmd { return v.take("") },
		"tui.screen.release":  func() tea.Cmd { return v.release(false) },
		"tui.screen.shot":     v.shot,
		"tui.screen.install":  v.install,
		"tui.screen.switch":   func() tea.Cmd { return v.choose(true) },
		"tui.screen.keep":     func() tea.Cmd { return v.choose(false) },
		"tui.screen.settings": v.settings,
		"tui.screen.refresh":  v.load,
	}
	// A key runs its action only while the desktop offers it, so a key of an action that does not apply does nothing
	handlers := map[string]func(*cards.Page) tea.Cmd{}
	for id, fn := range run {
		handlers[id] = func(*cards.Page) tea.Cmd {
			if id != "tui.screen.settings" && id != "tui.screen.refresh" && !slices.Contains(v.offered(), id) {
				return nil
			}
			return fn()
		}
	}
	handlers["tui.screen.main"] = func(*cards.Page) tea.Cmd {
		if offered := v.offered(); len(offered) > 0 {
			return run[offered[0]]()
		}
		return nil
	}
	title := "Agent desktop"
	if v.profile != "" {
		title += " · " + v.profile
	}
	v.page = cards.NewPage(v.kit.Env, cards.PageSpec{
		Title:   title,
		Key:     "screen",
		Context: screenContext,
		Follow:  true,
		Lines:   v.lines,
		Hints:   v.hints,
		Actions: handlers,
	})
	v.hub = v.kit.Hub("screen", v.page)
	var watched context.Context
	watched, v.stop = context.WithCancel(v.kit.Env.Ctx)
	v.hub.OnClose(func() tea.Cmd {
		v.stop()
		return nil
	})
	v.watch(watched)
	return tea.Batch(v.kit.WM.ShowOverlay(v.hub), v.run(argument))
}

// Runs the action the command names, the status when it names none
func (v *view) run(argument string) tea.Cmd {
	fields := strings.Fields(strings.ToLower(argument))
	action := "status"
	if len(fields) > 0 {
		action = fields[0]
	}
	second := len(fields) > 1 && fields[1] == "force"
	switch action {
	case "status":
		return v.load()
	case "start":
		return v.start()
	case "stop":
		return v.halt(second)
	case "take":
		reason := ""
		if words := strings.Fields(argument); len(words) > 1 {
			reason = strings.Join(words[1:], " ")
		}
		return v.take(reason)
	case "release":
		return v.release(second)
	case "shot":
		return v.shot()
	case "install":
		return v.install()
	case "switch", "keep":
		return v.choose(action == "switch")
	}
	v.say(render.Warm("There is no /screen "+action+". It takes "+subcommands+".", v.kit.Env.Theme))
	return v.load()
}

// Notes a line under the status
func (v *view) say(text string) {
	v.log = append(v.log, text)
}

// Who drives the desktop, in words
func holder(l *botdesktopv1.Lease) string {
	if l.GetHolder() == "human" {
		return "A person"
	}
	return "The agent"
}

// Whether a person drives the desktop
func human(s *botdesktopv1.Screen) bool { return s.GetLease().GetHolder() == "human" }

// How the desktop stands in a word or two
func state(s *botdesktopv1.Screen) string {
	switch {
	case s.GetRunning():
		return "Running"
	case !s.GetSupported():
		return "Can't run here"
	case !s.GetInstalled():
		return "Not installed"
	}
	return "Stopped"
}

// Where the desktop runs, in words: this computer, or the sandbox of the agent's commands
func placement(where string) string {
	backend, sandboxed := strings.CutPrefix(where, "terminal:")
	switch {
	case where == "":
		return ""
	case !sandboxed, backend == "local":
		return "On this computer"
	}
	return "In the " + backend + " sandbox, with the agent's commands"
}

// Memory in MB in the largest unit that fits
func memory(mb int32) string {
	return config.Size(float64(mb) * 1024 * 1024)
}

func (v *view) lines(w int) []string {
	t := v.kit.Env.Theme
	width := max(w, 1)
	var lines []string
	add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
	// What the screen is about, for a person who meets it for the first time
	add(render.Dim("A hidden desktop of its own that the agent can open programs and click around in.", t))
	lines = append(lines, "")
	if v.status == nil {
		switch {
		case v.err != nil:
		case !v.loaded:
			add(render.Dim("Checking the agent desktop…", t))
		}
	} else {
		s := v.status
		rows := [][2]string{{"State", state(s)}, {"In control", holder(s.GetLease())}}
		if reason := s.GetLease().GetReason(); human(s) && reason != "" {
			rows = append(rows, [2]string{"Taken for", reason})
		}
		rows = append(rows, [2]string{"Profile", s.GetProfile()})
		if len(s.GetMissing()) > 0 {
			rows = append(rows, [2]string{"Missing", strings.Join(s.GetMissing(), ", ")})
		}
		for _, row := range [][2]string{{"Display", s.GetDisplay()}, {"Screen size", s.GetGeometry()}, {"Runs", placement(s.GetPlacement())},
			{"Browser", s.GetBrowser()}} {
			if row[1] != "" {
				rows = append(rows, row)
			}
		}
		if free := s.GetMemoryAvailableMb(); free >= 0 {
			said := memory(free)
			if limit := s.GetMemoryLimitMb(); limit > 0 {
				said += " of " + memory(limit)
			}
			rows = append(rows, [2]string{"Free memory", said})
		}
		label := 0
		for _, row := range rows {
			label = max(label, ansi.StringWidth(row[0]))
		}
		for _, row := range rows {
			add(render.Dim(row[0]+strings.Repeat(" ", label-ansi.StringWidth(row[0])+2), t) + row[1])
		}
		if blocker := s.GetBlocker(); blocker != "" {
			lines = append(lines, "")
			add(render.Warm(blocker, t))
		}
		if sw := s.GetImageSwitch(); sw != nil {
			lines = append(lines, "")
			add("The sandbox still runs " + sw.GetCurrentImage() + ", which has no desktop. Switch image moves it to " + sw.GetTargetImage() +
				" and keeps the files in /root and /workspace. Keep image leaves it on " + sw.GetCurrentImage() + ".")
		}
	}
	if len(v.log) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		for _, line := range v.log {
			add(line)
		}
	}
	return lines
}

// Actions the desktop offers as it stands, the one enter runs first
func (v *view) offered() []string {
	s := v.status
	switch {
	case s == nil:
		return nil
	case s.GetRunning() && human(s):
		return []string{"tui.screen.release", "tui.screen.shot", "tui.screen.stop"}
	case s.GetRunning():
		return []string{"tui.screen.take", "tui.screen.shot", "tui.screen.stop"}
	case s.GetImageSwitch() != nil:
		return []string{"tui.screen.switch", "tui.screen.keep"}
	case !s.GetSupported():
		return []string{"tui.screen.settings"}
	case !s.GetInstalled() && !strings.HasPrefix(s.GetPlacement(), "terminal"):
		return []string{"tui.screen.install"}
	}
	return []string{"tui.screen.start"}
}

// Buttons of the page: what enter runs first, then the other actions the desktop offers, its settings, and checking again once a check
// failed
func (v *view) hints(*cards.Page) []wm.Hint {
	var out []wm.Hint
	offered := v.offered()
	for i, id := range offered {
		if i == 0 {
			label := id
			if a, ok := v.kit.Env.Keys.Action(id); ok {
				label = a.LabelText()
			}
			out = append(out, wm.Hint{Action: "tui.screen.main", Label: label})
			continue
		}
		out = append(out, wm.Hint{Action: id})
	}
	if len(offered) > 0 && offered[0] != "tui.screen.settings" {
		out = append(out, wm.Hint{Action: "tui.screen.settings"})
	}
	if v.status == nil || v.err != nil {
		out = append(out, wm.Hint{Action: "tui.screen.refresh"})
	}
	return out
}

// Shows the status the call answered, or why it failed
func (v *view) show(status *botdesktopv1.Screen, err error) tea.Cmd {
	v.loaded = true
	if err != nil {
		v.err = err
		v.say(render.Bad(sentence(tui.Reason(err))+" "+v.kit.Env.Keys.Display("tui.screen.refresh")+" checks again.", v.kit.Env.Theme))
		return nil
	}
	v.err, v.status = nil, status
	return nil
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

func (v *view) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Status(v.kit.Env.Ctx, &botdesktopv1.StatusRequest{Profile: v.profile})
		return func() tea.Cmd { return v.show(status, err) }
	})
}

func (v *view) start() tea.Cmd {
	v.say("Starting the agent desktop…")
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Start(v.kit.Env.Ctx, &botdesktopv1.StartRequest{Profile: v.profile})
		return func() tea.Cmd { return v.show(status, err) }
	})
}

// Stops the desktop; while a person holds it, asks first and stops it anyway on yes
func (v *view) halt(force bool) tea.Cmd {
	if !force && human(v.status) {
		v.page.Confirm("Stop the agent desktop? A person holds it now, and stopping ends what they are doing on it.", func() tea.Cmd { return v.halt(true) })
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.Stop(v.kit.Env.Ctx, &botdesktopv1.StopRequest{Profile: v.profile, Force: force})
		return func() tea.Cmd {
			if err != nil {
				return v.show(nil, err)
			}
			if reply.GetStopped() {
				v.say("The agent desktop stopped.")
			}
			return v.show(reply.GetStatus(), nil)
		}
	})
}

func (v *view) take(reason string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := v.service.Take(v.kit.Env.Ctx, &botdesktopv1.TakeRequest{Profile: v.profile, Viewer: v.viewer, Reason: reason})
		return func() tea.Cmd {
			if err != nil {
				return v.show(nil, err)
			}
			v.say("You hold the agent desktop now. What the agent does on it waits until you hand it back.")
			return v.load()
		}
	})
}

func (v *view) release(force bool) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := v.service.Release(v.kit.Env.Ctx, &botdesktopv1.ReleaseRequest{Profile: v.profile, Viewer: v.viewer, Force: force})
		return func() tea.Cmd {
			if err != nil {
				return v.show(nil, err)
			}
			v.say("You handed the agent desktop back to the agent.")
			return v.load()
		}
	})
}

func (v *view) shot() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.Thumbnail(v.kit.Env.Ctx, &botdesktopv1.ThumbnailRequest{Profile: v.profile})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return v.show(nil, err)
			case reply.GetSuppressed() != "":
				v.say("A person holds the agent desktop, so no screenshot is taken until it is handed back.")
			case reply.GetPath() == "":
				v.say("The agent desktop is not running. Start it first.")
			default:
				v.say("Saved a screenshot of the agent desktop to " + reply.GetPath() + ".")
			}
			return nil
		}
	})
}

func (v *view) choose(approve bool) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.Switch(v.kit.Env.Ctx, &botdesktopv1.SwitchRequest{Profile: v.profile, Approve: approve})
		return func() tea.Cmd {
			if err != nil {
				return v.show(nil, err)
			}
			v.say("The sandbox image is now " + reply.GetImage() + ".")
			return v.show(reply.GetStatus(), nil)
		}
	})
}

// Opens the settings of the agent desktop in place of this screen
func (v *view) settings() tea.Cmd {
	return tea.Batch(v.kit.WM.DismissCard(v.hub), v.kit.Open(configapp.Settings, profile.Join(v.profile, botdesktopconfig.Key)))
}

// Line of how the install ended
func ended(code int32) string {
	switch code {
	case 0:
		return "The programs of the agent desktop are installed. Start runs the desktop."
	case -1:
		return "The install was cancelled."
	case -2:
		return "No sudo password was given. Run the install command shown above in a shell."
	}
	return "The install failed with exit code " + strconv.Itoa(int(code)) + "."
}

// Installs the programs, their output following on the page and the sudo password asked for in a masked form
func (v *view) install() tea.Cmd {
	ctx := v.kit.Env.Ctx
	go func() {
		stream, err := v.service.Install(ctx, &botdesktopv1.InstallRequest{Profile: v.profile})
		if err != nil {
			v.kit.Apply(func() tea.Cmd {
				if connect.CodeOf(err) == connect.CodeFailedPrecondition && v.status.GetInstallCommand() != "" {
					v.say("The install runs only on the computer where icebath's server runs. Run this there: " + v.status.GetInstallCommand())
				}
				return v.show(nil, err)
			})
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				if ctx.Err() == nil {
					v.kit.Apply(func() tea.Cmd { return v.show(nil, err) })
				}
				return
			}
			switch kind := event.GetKind().(type) {
			case *botdesktopv1.InstallEvent_Command:
				v.kit.Apply(func() tea.Cmd {
					v.say("Installing the programs of the agent desktop. Their output follows.")
					return nil
				})
			case *botdesktopv1.InstallEvent_Line:
				v.kit.Apply(func() tea.Cmd {
					v.say(render.Dim(kind.Line, v.kit.Env.Theme))
					return nil
				})
			case *botdesktopv1.InstallEvent_Password:
				v.kit.Apply(func() tea.Cmd { return v.password(kind.Password) })
			case *botdesktopv1.InstallEvent_Done:
				v.kit.Apply(func() tea.Cmd {
					v.say(ended(kind.Done.GetCode()))
					return v.show(kind.Done.GetStatus(), nil)
				})
				return
			}
		}
	}()
	return nil
}

// Masked form asking the sudo password the install waits on, cancelled when closed
func (v *view) password(id string) tea.Cmd {
	answer := func(password string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			_, err := v.service.Password(v.kit.Env.Ctx, &botdesktopv1.PasswordRequest{Id: id, Password: password})
			return func() tea.Cmd {
				if err != nil {
					return v.show(nil, err)
				}
				return nil
			}
		})
	}
	name := v.profile
	if v.status != nil {
		name = v.status.GetProfile()
	}
	about := "Installing the programs of the agent desktop needs sudo."
	if name != "" {
		about = "Installing the programs of the agent desktop for the profile " + name + " needs sudo."
	}
	var form *cards.Form
	form = cards.NewForm(v.kit.Env, cards.FormSpec{
		Title:  "Password for sudo",
		Intro:  []string{about + " Your password goes to sudo once and is never kept."},
		Fields: []cards.Field{{ID: "password", Label: "Password", Kind: cards.FieldSecret, Required: true}},
		Submit: "Install",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(v.hub.Close(form), answer(values["password"]))
		},
		Cancel: func(*cards.Form) tea.Cmd {
			v.say("No password was given, so the install stops.")
			return tea.Batch(v.hub.Close(form), answer(""))
		},
	})
	return v.hub.Push(form)
}

// Follows the changes of the desktop until the page closes, a start or stop and a change of hands said as they come
func (v *view) watch(ctx context.Context) {
	go func() {
		stream, err := v.service.Watch(ctx, &botdesktopv1.WatchRequest{Profile: v.profile})
		if err != nil {
			return
		}
		defer stream.Close()
		for {
			change, err := stream.Receive()
			if err != nil {
				return
			}
			v.kit.Apply(func() tea.Cmd {
				switch kind := change.GetKind().(type) {
				case *botdesktopv1.Change_Status:
					running := kind.Status.GetRunning()
					if v.status == nil || v.status.GetRunning() != running {
						if running {
							v.say("The agent desktop is running.")
						} else {
							v.say("The agent desktop stopped.")
						}
					}
					return v.show(kind.Status, nil)
				case *botdesktopv1.Change_Lease:
					if v.status != nil {
						v.status.Lease = kind.Lease
					}
					if kind.Lease.GetHolder() == "human" {
						v.say("A person now controls the agent desktop.")
					} else {
						v.say("The agent now controls the agent desktop.")
					}
				}
				return nil
			})
		}
	}()
}
