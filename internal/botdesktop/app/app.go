// The Bot screen: the profile's own headless desktop, its status, start and stop, a picture of it, taking it over and handing it back, the
// install of its packages and the sandbox image it needs
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
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

const usage = "usage: /screen [status|start|stop [force]|take [reason]|release [force]|shot|install|switch|keep]"

var actions = []keys.Action{
	{ID: "tui.screen.start", Context: screenContext, Label: "start", Doc: "start the bot screen", Default: []string{"s"}},
	{ID: "tui.screen.stop", Context: screenContext, Label: "stop", Doc: "stop the bot screen, handing it back to the agent first", Default: []string{"x"}},
	{ID: "tui.screen.take", Context: screenContext, Label: "take over", Doc: "take the bot screen over: the agent's actions on it wait until you hand it back", Default: []string{"t"}},
	{ID: "tui.screen.release", Context: screenContext, Label: "hand back", Doc: "hand the bot screen back to the agent", Default: []string{"r"}},
	{ID: "tui.screen.shot", Context: screenContext, Label: "picture", Doc: "save a picture of the bot screen", Default: []string{"p"}},
	{ID: "tui.screen.install", Context: screenContext, Label: "install", Doc: "install the screen packages with the package manager", Default: []string{"i"}},
	{ID: "tui.screen.switch", Context: screenContext, Label: "switch image", Doc: "move the sandbox to the image with the desktop", Default: []string{"w"}},
	{ID: "tui.screen.keep", Context: screenContext, Label: "keep image", Doc: "keep the sandbox on the image it runs", Default: []string{"k"}},
	{ID: "tui.screen.refresh", Context: screenContext, Label: "refresh", Doc: "read how the bot screen stands again", Default: []string{"ctrl+r"}},
}

type screens struct {
	mu sync.Mutex
	// Id this app takes the screen under, random and kept for its life, so only it hands back what it took
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
		ID:    "screen",
		Title: "Bot screen",
		Short: "the bot screen of this profile, its status, start, stop, a picture of it, taking it over and handing it back, install and sandbox " +
			"image switch",
		Slash:   []string{"screen"},
		Usage:   "[status|start|stop [force]|take [reason]|release [force]|shot|install|switch|keep] [@profile]",
		Actions: actions,
		Where:   map[string]string{screenContext: "bot screen"},
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
	// Ends the watch of the screen, once the page is gone
	stop context.CancelFunc
}

func (v *view) open(argument string) tea.Cmd {
	v.page = cards.NewPage(v.kit.Env, cards.PageSpec{
		Title:   "Bot screen",
		Key:     "tui:screen",
		Context: screenContext,
		Follow:  true,
		Lines:   v.lines,
		Hints:   v.hints,
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"tui.screen.start":   func(*cards.Page) tea.Cmd { return v.start() },
			"tui.screen.stop":    func(*cards.Page) tea.Cmd { return v.halt(false) },
			"tui.screen.take":    func(*cards.Page) tea.Cmd { return v.take("") },
			"tui.screen.release": func(*cards.Page) tea.Cmd { return v.release(false) },
			"tui.screen.shot":    func(*cards.Page) tea.Cmd { return v.shot() },
			"tui.screen.install": func(*cards.Page) tea.Cmd { return v.install() },
			"tui.screen.switch":  func(*cards.Page) tea.Cmd { return v.choose(true) },
			"tui.screen.keep":    func(*cards.Page) tea.Cmd { return v.choose(false) },
			"tui.screen.refresh": func(*cards.Page) tea.Cmd { return v.load() },
		},
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
	v.say(usage)
	return v.load()
}

// Notes a line under the status
func (v *view) say(text string) {
	v.log = append(v.log, text)
}

func yes(on bool) string {
	if on {
		return "yes"
	}
	return "no"
}

func holder(l *botdesktopv1.Lease) string {
	if l.GetHolder() == "human" {
		return "a person"
	}
	return "the agent"
}

func (v *view) lines(w int) []string {
	t := v.kit.Env.Theme
	width := max(w, 1)
	var lines []string
	add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
	if v.status == nil {
		add(tui.Waiting(v.loaded, v.err, ""))
	} else {
		s := v.status
		rows := [][2]string{{"profile", s.GetProfile()}, {"supported", yes(s.GetSupported())}, {"installed", yes(s.GetInstalled())},
			{"running", yes(s.GetRunning())}}
		if len(s.GetMissing()) > 0 {
			rows = append(rows, [2]string{"missing", strings.Join(s.GetMissing(), ", ")})
		}
		for _, row := range [][2]string{{"display", s.GetDisplay()}, {"size", s.GetGeometry()}, {"runs on", s.GetPlacement()}, {"browser", s.GetBrowser()}} {
			if row[1] != "" {
				rows = append(rows, row)
			}
		}
		rows = append(rows, [2]string{"in control", holder(s.GetLease())})
		for _, row := range rows {
			add(render.Dim(fmt.Sprintf("%-11s", row[0]), t) + row[1])
		}
		if blocker := s.GetBlocker(); blocker != "" {
			lines = append(lines, "")
			add(render.Warm(blocker, t))
		}
		if sw := s.GetImageSwitch(); sw != nil {
			lines = append(lines, "")
			add(fmt.Sprintf("The sandbox still runs %s, which has no desktop. /screen switch moves it to %s, files in /root and /workspace stay. "+
				"/screen keep stays on %s.", sw.GetCurrentImage(), sw.GetTargetImage(), sw.GetCurrentImage()))
		}
	}
	if len(v.log) > 0 {
		lines = append(lines, "")
		for _, line := range v.log {
			add(line)
		}
	}
	return lines
}

func (v *view) hints(*cards.Page) []wm.Hint {
	s := v.status
	var out []wm.Hint
	switch {
	case s == nil:
	case s.GetRunning():
		out = append(out, wm.Hint{Action: "tui.screen.shot"})
		if s.GetLease().GetHolder() == "human" {
			out = append(out, wm.Hint{Action: "tui.screen.release"})
		} else {
			out = append(out, wm.Hint{Action: "tui.screen.take"})
		}
		out = append(out, wm.Hint{Action: "tui.screen.stop"})
	case !s.GetInstalled() && s.GetSupported() && !strings.HasPrefix(s.GetPlacement(), "terminal"):
		out = append(out, wm.Hint{Action: "tui.screen.install"})
	case s.GetImageSwitch() != nil:
		out = append(out, wm.Hint{Action: "tui.screen.switch"}, wm.Hint{Action: "tui.screen.keep"})
	default:
		out = append(out, wm.Hint{Action: "tui.screen.start"})
	}
	return append(out, wm.Hint{Action: "tui.screen.refresh"})
}

// Shows the status the call answered, or why it failed
func (v *view) show(status *botdesktopv1.Screen, err error) tea.Cmd {
	v.loaded = true
	if err != nil {
		v.err = err
		v.say(render.Bad(tui.Reason(err), v.kit.Env.Theme))
		return nil
	}
	v.err, v.status = nil, status
	return nil
}

func (v *view) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Status(v.kit.Env.Ctx, &botdesktopv1.StatusRequest{Profile: v.profile})
		return func() tea.Cmd { return v.show(status, err) }
	})
}

func (v *view) start() tea.Cmd {
	v.say("starting the bot screen…")
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Start(v.kit.Env.Ctx, &botdesktopv1.StartRequest{Profile: v.profile})
		return func() tea.Cmd { return v.show(status, err) }
	})
}

// Stops the screen; while a person holds it, asks first and stops it anyway on yes
func (v *view) halt(force bool) tea.Cmd {
	if !force && v.status.GetLease().GetHolder() == "human" {
		return v.hub.Push(cards.NewForm(v.kit.Env, cards.FormSpec{
			Title:  "Stop the bot screen?",
			Intro:  []string{"A person holds the bot screen; stopping it ends what they are doing on it."},
			Submit: "stop anyway",
			Done:   func(*cards.Form, map[string]string) tea.Cmd { return tea.Batch(v.hub.Pop(), v.halt(true)) },
			Cancel: func(*cards.Form) tea.Cmd { return v.hub.Pop() },
		}))
	}
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.Stop(v.kit.Env.Ctx, &botdesktopv1.StopRequest{Profile: v.profile, Force: force})
		return func() tea.Cmd {
			if err != nil {
				return v.show(nil, err)
			}
			if reply.GetStopped() {
				v.say("bot screen stopped")
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
			v.say("you hold the bot screen: the agent's actions on it wait until /screen release")
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
			v.say("bot screen handed back to the agent")
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
				v.say("a person holds the bot screen, no picture is taken until it is handed back with /screen release")
			case reply.GetPath() == "":
				v.say("the bot screen is not running, /screen start brings it up")
			default:
				v.say("picture of the bot screen saved to " + reply.GetPath())
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
			v.say("sandbox image: " + reply.GetImage())
			return v.show(reply.GetStatus(), nil)
		}
	})
}

// Line of how the install ended
func ended(code int32) string {
	switch code {
	case 0:
		return "screen packages installed, /screen start runs the screen"
	case -1:
		return "screen package install cancelled"
	case -2:
		return "no sudo password was given, run the install command printed above in a shell"
	}
	return fmt.Sprintf("screen package install failed with status %d", code)
}

// Installs the packages, their output following on the page and the sudo password asked for in a masked form
func (v *view) install() tea.Cmd {
	ctx := v.kit.Env.Ctx
	go func() {
		stream, err := v.service.Install(ctx, &botdesktopv1.InstallRequest{Profile: v.profile})
		if err != nil {
			v.kit.Apply(func() tea.Cmd {
				if connect.CodeOf(err) == connect.CodeFailedPrecondition && v.status.GetInstallCommand() != "" {
					v.say("the install runs only on the background service's computer, run there: " + v.status.GetInstallCommand())
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
					v.say("installing the screen packages, their output follows")
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

// Masked form asking the sudo password the install waits on, cancelled when left empty or closed
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
	form := cards.NewForm(v.kit.Env, cards.FormSpec{
		Title:  "sudo password required",
		Intro:  []string{"The screen packages of profile " + name + " install with sudo, the password goes to sudo once and is never stored"},
		Fields: []cards.Field{{ID: "password", Label: "password", Kind: cards.FieldSecret}},
		Submit: "install",
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(v.hub.Pop(), answer(values["password"]))
		},
		Cancel: func(f *cards.Form) tea.Cmd {
			v.say("sudo cancelled")
			return tea.Batch(v.hub.Pop(), answer(""))
		},
	})
	return v.hub.Push(form)
}

// Follows the changes of the screen until the page closes, a start or stop and a change of hands said as they come
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
							v.say("bot screen running")
						} else {
							v.say("bot screen stopped")
						}
					}
					return v.show(kind.Status, nil)
				case *botdesktopv1.Change_Lease:
					if v.status != nil {
						v.status.Lease = kind.Lease
					}
					v.say("bot screen: " + holder(kind.Lease) + " in control")
				}
				return nil
			})
		}
	}()
}
