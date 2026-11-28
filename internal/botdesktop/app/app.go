// Page of Agent desktop in the settings menu: the profile's own hidden desktop, its state as a sentence, the acts that apply to it now
// (start and stop, taking it over and handing it back, a screenshot, installing its programs and the image of its sandbox), and its
// options
package app

import (
	"cmp"
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

	"github.com/nickheyer/icebath/internal/app"
	botdesktopconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	botdesktopv1 "github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1/botdesktopv1connect"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Agent desktop, the page of its area
const Screen = "desktop"

// What /desktop takes after its name
const subcommands = "status, start, stop, take, release, shot, install, switch or keep"

// Acts of the agent desktop, each a row while it applies
const (
	actStart   = "start"
	actStop    = "stop"
	actTake    = "take"
	actRelease = "release"
	actShot    = "shot"
	actInstall = "install"
	actSwitch  = "switch"
	actKeep    = "keep"
)

// Names of the acts as their rows say them
var actTitles = map[string]string{actStart: "Start", actStop: "Stop", actTake: "Take over", actRelease: "Hand back", actShot: "Take a screenshot",
	actInstall: "Install", actSwitch: "Use the new image", actKeep: "Keep this image"}

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
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:      Screen,
		Group:   tui.GroupAbilities,
		Order:   80,
		Title:   "Agent desktop",
		Short:   "Start, stop or take over the agent desktop",
		Slash:   []string{"desktop"},
		Aliases: []string{"screen"},
		Usage:   "[status|start|stop [force]|take [reason]|release [force]|shot|install|switch|keep] [@profile]",
		Area:    botdesktopconfig.Key,
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			v := &view{kit: kit, service: service, settings: settings, profile: name, viewer: s.id()}
			return v.open(strings.TrimSpace(rest))
		},
	}}
}

type view struct {
	kit      *tui.Kit
	service  botdesktopv1connect.DesktopServiceClient
	settings configv1connect.ConfigServiceClient
	profile  string
	viewer   string
	hub      *cards.Hub
	list     *cards.List
	options  *configapp.Rows
	status   *botdesktopv1.Screen
	loaded   bool
	err      error
	// Ends the watch of the desktop as the page leaves the screen, nil while nothing watches
	stop context.CancelFunc
}

// Row of the page: an act that applies now, or an option
type row struct {
	act     string
	setting *configv1.Setting
}

func (v *view) open(argument string) tea.Cmd {
	var shown []row
	v.list = cards.NewList(v.kit.Env, cards.ListSpec{
		Title:   "Agent desktop",
		Glyph:   v.kit.Env.Theme.Glyphs.Terminal,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   v.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = v.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				if r.setting != nil {
					cells[i] = v.options.Cells(r.setting)
					continue
				}
				cells[i] = []string{actTitles[r.act], "", ""}
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].setting != nil {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			if r := shown[i]; r.setting != nil {
				return v.options.Detail(r.setting, v.list.Cut(i, 2))
			}
			return v.detail(shown[i].act)
		},
		Empty: func() string { return tui.Waiting(v.loaded, v.err, "") },
		Free:  map[string]bool{"hub.refresh": true},
		// Back in view after the page left the screen, which ended its watch: the desktop is read again and watched again
		OnFocus: func(*cards.List) tea.Cmd {
			if v.hub == nil || v.stop != nil {
				return nil
			}
			v.follow()
			return v.load(nil)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			if r := shown[i]; r.setting != nil {
				return v.options.Hints(r.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: strings.ToLower(actTitles[shown[i].act])}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if r := shown[i]; r.setting != nil {
					return v.options.Change(r.setting)
				}
				return v.do(shown[i].act, false, "")
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return v.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return v.load(nil) },
		},
	})
	v.list.ToggleDetail()
	hub, cmd := v.kit.Show(Screen, v.list)
	v.hub = hub
	v.options = configapp.NewRows(v.kit, hub, v.settings, v.profile)
	v.options.Watch(v.list)
	v.hub.OnClose(func() tea.Cmd {
		if v.stop != nil {
			v.stop()
			v.stop = nil
		}
		return nil
	})
	v.follow()
	return tea.Batch(cmd, v.load(func() tea.Cmd { return v.run(argument) }))
}

// Watches the desktop until the page leaves the screen
func (v *view) follow() {
	var watched context.Context
	watched, v.stop = context.WithCancel(v.kit.Env.Ctx)
	v.watch(watched)
}

// Runs the act the command names, nothing more for the state
func (v *view) run(argument string) tea.Cmd {
	fields := strings.Fields(argument)
	if len(fields) == 0 {
		return nil
	}
	force := len(fields) > 1 && strings.EqualFold(fields[1], "force")
	switch action := strings.ToLower(fields[0]); action {
	case "status":
		return nil
	case actStart, actStop, actShot, actInstall, actSwitch, actKeep, actRelease:
		return v.do(action, force, "")
	case actTake:
		return v.do(action, false, strings.Join(strings.Fields(argument)[1:], " "))
	}
	return v.hub.Fail("There is no /desktop " + fields[0] + ". It takes " + subcommands + ".")
}

// What the desktop is: its state as a sentence, then while something keeps it from starting, what
func (v *view) intro() []string {
	t := v.kit.Env.Theme
	s := v.status
	if s == nil {
		return []string{"A hidden desktop of its own that the agent can open programs and click around in."}
	}
	lines := []string{v.state(s)}
	if blocker := s.GetBlocker(); blocker != "" && !s.GetRunning() {
		lines = append(lines, render.Warm(strs.Full(blocker), t))
	}
	return lines
}

// State of the desktop as a sentence
func (v *view) state(s *botdesktopv1.Screen) string {
	t := v.kit.Env.Theme
	switch {
	case s.GetRunning() && human(s) && s.GetLease().GetReason() != "":
		return render.Good("Running. A person is in control, for "+s.GetLease().GetReason()+", and the agent waits.", t)
	case s.GetRunning() && human(s):
		return render.Good("Running. A person is in control, and the agent waits.", t)
	case s.GetRunning():
		return render.Good("Running. The agent is in control.", t)
	case s.GetImageSwitch() != nil:
		return render.Warm("Stopped. The sandbox runs "+s.GetImageSwitch().GetCurrentImage()+", which has no desktop.", t)
	case !s.GetSupported():
		return render.Bad("Can't run here. The agent desktop runs only on Linux.", t)
	case !s.GetInstalled() && !sandboxed(s):
		return render.Warm("Not installed. It needs "+lacking(s.GetMissing())+".", t)
	}
	return "Stopped. It starts when a tool needs a screen while Start when needed is on, or when you start it."
}

// Whether a person drives the desktop
func human(s *botdesktopv1.Screen) bool { return s.GetLease().GetHolder() == "human" }

// Whether the desktop runs in the sandbox of the agent's commands, where its programs come with the image
func sandboxed(s *botdesktopv1.Screen) bool { return strings.HasPrefix(s.GetPlacement(), "terminal") }

// Acts that apply to the desktop as it stands, each a row
func (v *view) acts() []string {
	s := v.status
	switch {
	case s == nil:
		return nil
	case s.GetRunning() && human(s):
		return []string{actRelease, actShot, actStop}
	case s.GetRunning():
		return []string{actTake, actShot, actStop}
	case s.GetImageSwitch() != nil:
		return []string{actSwitch, actKeep}
	case !s.GetSupported():
		return nil
	case !s.GetInstalled() && !sandboxed(s):
		return []string{actInstall}
	}
	return []string{actStart}
}

func (v *view) rows() []row {
	if !v.loaded || v.err != nil {
		return nil
	}
	var rows []row
	for _, act := range v.acts() {
		rows = append(rows, row{act: act})
	}
	everyday, advanced := v.options.Section(botdesktopconfig.Key)
	for _, setting := range append(everyday, advanced...) {
		rows = append(rows, row{setting: setting})
	}
	return rows
}

// Lines under the rows about an act: what it does, then what only reports of the desktop where it belongs to that act
func (v *view) detail(act string) []string {
	t := v.kit.Env.Theme
	s := v.status
	switch act {
	case actStart:
		lines := []string{strings.TrimSpace("Start the agent desktop now. " + where(s))}
		if said := memoryLine(s); said != "" {
			lines = append(lines, render.Dim(said, t))
		}
		return lines
	case actStop:
		return append([]string{"Hand the desktop back to the agent and stop it."}, v.facts(s)...)
	case actTake:
		lines := []string{"Take the desktop over, so what the agent does on it waits until you hand it back."}
		if said := v.watchLine(s); said != "" {
			lines = append(lines, render.Dim(said, t))
		}
		return lines
	case actRelease:
		return []string{"Hand the desktop back to the agent, which goes on with what it waited to do."}
	case actShot:
		return []string{"Save a picture of the desktop as it is now to the image cache."}
	case actInstall:
		lines := []string{"Install " + lacking(s.GetMissing()) + " with the package manager of this computer, asking your sudo password."}
		if command := s.GetInstallCommand(); command != "" {
			lines = append(lines, render.Dim("It runs: "+command, t))
		}
		return lines
	case actSwitch:
		sw := s.GetImageSwitch()
		return []string{"Move the sandbox to " + sw.GetTargetImage() + ", which has a desktop. The files in /root and /workspace carry over."}
	case actKeep:
		sw := s.GetImageSwitch()
		return []string{"Keep the sandbox on " + sw.GetCurrentImage() + ", which has no desktop, and stop asking. The agent desktop can't run there."}
	}
	return nil
}

// Where the desktop runs, as a sentence
func where(s *botdesktopv1.Screen) string {
	backend, ok := strings.CutPrefix(s.GetPlacement(), "terminal:")
	switch {
	case s.GetPlacement() == "":
		return ""
	case !ok, backend == "local":
		return "It runs on this computer."
	}
	return "It runs in the sandbox of " + cmp.Or(s.GetSandbox(), backend) + ", with the agent's commands."
}

// Free memory and the least a start needs, as a line
func memoryLine(s *botdesktopv1.Screen) string {
	free := s.GetMemoryAvailableMb()
	if free < 0 {
		return ""
	}
	said := memory(free) + " of memory is free"
	if limit := s.GetMemoryLimitMb(); limit > 0 {
		said += ", of " + memory(limit)
	}
	return said + "."
}

// What the desktop shows of itself while it runs, each a line
func (v *view) facts(s *botdesktopv1.Screen) []string {
	t := v.kit.Env.Theme
	var lines []string
	for _, fact := range [][2]string{{"Display", s.GetDisplay()}, {"Screen size", s.GetGeometry()}, {"Browser", s.GetBrowser()}} {
		if fact[1] != "" {
			lines = append(lines, render.Dim(fact[0]+": "+fact[1], t))
		}
	}
	if said := where(s); said != "" {
		lines = append(lines, render.Dim(said, t))
	}
	if said := memoryLine(s); said != "" {
		lines = append(lines, render.Dim(said, t))
	}
	return lines
}

// Line saying a person can watch the desktop through its socket, for one who takes it over
func (v *view) watchLine(s *botdesktopv1.Screen) string {
	if socket := s.GetSocket(); socket != "" {
		return "A viewer reaches it at " + socket + "."
	}
	return ""
}

// Memory in MB in the largest unit that fits
func memory(mb int32) string {
	return config.Size(float64(mb) * 1024 * 1024)
}

// Programs the agent desktop lacks as one list, the words for them when none is named
func lacking(names []string) string {
	if len(names) == 0 {
		return "programs it lacks"
	}
	return strs.And(names)
}

// Reads the desktop, then the options, then draws the page again and runs then
func (v *view) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Status(v.kit.Env.Ctx, &botdesktopv1.StatusRequest{Profile: v.profile})
		return func() tea.Cmd {
			// What was read of the desktop shows whether or not the options can be read after it
			v.loaded, v.err = true, err
			if err == nil {
				v.status = status
			}
			v.list.Refresh()
			return v.options.Load(func() tea.Cmd {
				v.list.Refresh()
				if then == nil || err != nil {
					return nil
				}
				return then()
			})
		}
	})
}

// Takes the status an act answered, saying what was done, or why it failed
func (v *view) show(status *botdesktopv1.Screen, err error, note string) tea.Cmd {
	if err != nil {
		return v.hub.Fail(strs.Full(tui.Reason(err)))
	}
	if status != nil {
		v.status = status
	}
	v.list.Refresh()
	if note == "" {
		return nil
	}
	return v.hub.Done(note)
}

// Does an act; one the desktop does not offer now says why and does nothing
func (v *view) do(act string, force bool, reason string) tea.Cmd {
	if v.status != nil && !slices.Contains(v.acts(), act) && !(act == actStop && v.status.GetRunning()) && !(act == actRelease && force) {
		return v.hub.Say(actTitles[act]+" does not apply to the agent desktop now.", cards.NoteInfo)
	}
	ctx := v.kit.Env.Ctx
	switch act {
	case actStart:
		v.hub.Busy("Starting the agent desktop…")
		return tui.Async(func() func() tea.Cmd {
			status, err := v.service.Start(ctx, &botdesktopv1.StartRequest{Profile: v.profile})
			return func() tea.Cmd { return v.show(status, err, "The agent desktop is running.") }
		})
	case actStop:
		if !force && human(v.status) {
			v.list.Confirm(cards.Question{Text: "The agent desktop stops while a person holds it. What they are doing on it ends.", Verb: "Stop"},
				func() tea.Cmd { return v.do(actStop, true, "") })
			return nil
		}
		v.hub.Busy("Stopping the agent desktop…")
		return tui.Async(func() func() tea.Cmd {
			reply, err := v.service.Stop(ctx, &botdesktopv1.StopRequest{Profile: v.profile, Force: force})
			return func() tea.Cmd {
				note := "The agent desktop was not running."
				if reply.GetStopped() {
					note = "The agent desktop stopped."
				}
				return v.show(reply.GetStatus(), err, note)
			}
		})
	case actTake:
		return tui.Async(func() func() tea.Cmd {
			_, err := v.service.Take(ctx, &botdesktopv1.TakeRequest{Profile: v.profile, Viewer: v.viewer, Reason: reason})
			return func() tea.Cmd {
				if err != nil {
					return v.show(nil, err, "")
				}
				return tea.Batch(v.hub.Done("You hold the agent desktop now. What the agent does on it waits until you hand it back."), v.load(nil))
			}
		})
	case actRelease:
		return tui.Async(func() func() tea.Cmd {
			_, err := v.service.Release(ctx, &botdesktopv1.ReleaseRequest{Profile: v.profile, Viewer: v.viewer, Force: force})
			return func() tea.Cmd {
				if err != nil {
					return v.show(nil, err, "")
				}
				return tea.Batch(v.hub.Done("You handed the agent desktop back to the agent."), v.load(nil))
			}
		})
	case actShot:
		return tui.Async(func() func() tea.Cmd {
			reply, err := v.service.Thumbnail(ctx, &botdesktopv1.ThumbnailRequest{Profile: v.profile})
			return func() tea.Cmd {
				switch {
				case err != nil:
					return v.show(nil, err, "")
				case reply.GetSuppressed() != "":
					return v.hub.Say("A person holds the agent desktop, so no screenshot is taken until it is handed back.", cards.NoteInfo)
				case reply.GetPath() == "":
					return v.hub.Say("The agent desktop is not running, so there is nothing to take a screenshot of.", cards.NoteInfo)
				}
				return v.hub.Done("Saved a screenshot of the agent desktop to " + reply.GetPath() + ".")
			}
		})
	case actInstall:
		return v.install()
	case actSwitch, actKeep:
		return tui.Async(func() func() tea.Cmd {
			reply, err := v.service.Switch(ctx, &botdesktopv1.SwitchRequest{Profile: v.profile, Approve: act == actSwitch})
			return func() tea.Cmd {
				note := "The sandbox now runs " + reply.GetImage() + ", which has a desktop, from its next start."
				if act == actKeep {
					note = "The sandbox keeps " + reply.GetImage() + ", which has no desktop."
				}
				return v.show(reply.GetStatus(), err, note)
			}
		})
	}
	return nil
}

// Line of how the install ended
func ended(code int32) (string, bool) {
	switch code {
	case 0:
		return "The programs of the agent desktop are installed. Start runs it.", true
	case -1:
		return "The install was cancelled.", false
	case -2:
		return "This computer has no sudo, so nothing was installed. Run the install command yourself.", false
	}
	return "The install failed with exit code " + strconv.Itoa(int(code)) + ". The lines above say why.", false
}

// Installs the programs on a page that follows their output, the sudo password asked in a form of its own field, carried on to its end
// when the page is closed
func (v *view) install() tea.Cmd {
	t := v.kit.Env.Theme
	var lines []string
	done, closed, good := false, false, false
	note := ""
	say := func() tea.Cmd {
		if good {
			return v.hub.Done(note)
		}
		return v.hub.Fail(note)
	}
	page := cards.NewPage(v.kit.Env, cards.PageSpec{
		Title:  "Install",
		Key:    Screen + ":install",
		Follow: true,
		Lines: func(int) []string {
			if len(lines) == 0 {
				return []string{render.Dim("Starting the install…", t)}
			}
			return lines
		},
		Closed: func(*cards.Page) tea.Cmd {
			closed = true
			if done {
				return say()
			}
			return nil
		},
	})
	// How the install ended is the last line of the page while it shows, and the note of the page of the desktop once it closes
	finish := func(text string, ok bool, status *botdesktopv1.Screen) tea.Cmd {
		if done {
			return nil
		}
		done, good, note = true, ok, text
		if status != nil {
			v.status = status
		}
		v.list.Refresh()
		if ok {
			lines = append(lines, "", render.Good(text, t))
		} else {
			lines = append(lines, "", render.Bad(text, t))
		}
		if closed {
			return say()
		}
		return nil
	}
	ctx := v.kit.Env.Ctx
	go func() {
		stream, err := v.service.Install(ctx, &botdesktopv1.InstallRequest{Profile: v.profile})
		if err != nil {
			v.kit.Apply(func() tea.Cmd {
				text := strs.Full(tui.Reason(err))
				if connect.CodeOf(err) == connect.CodeFailedPrecondition && v.status.GetInstallCommand() != "" {
					text = "The install runs only on the computer where icebath's server runs. Run this there: " + v.status.GetInstallCommand()
				}
				return finish(text, false, nil)
			})
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				v.kit.Apply(func() tea.Cmd { return finish("The install stopped without saying whether it worked.", false, nil) })
				return
			}
			if err != nil {
				if ctx.Err() == nil {
					v.kit.Apply(func() tea.Cmd { return finish(strs.Full(tui.Reason(err)), false, nil) })
				}
				return
			}
			switch kind := event.GetKind().(type) {
			case *botdesktopv1.InstallEvent_Command:
				v.kit.Apply(func() tea.Cmd { lines = append(lines, render.Dim("Running: "+kind.Command, t)); return nil })
			case *botdesktopv1.InstallEvent_Line:
				v.kit.Apply(func() tea.Cmd { lines = append(lines, kind.Line); return nil })
			case *botdesktopv1.InstallEvent_Password:
				v.kit.Apply(func() tea.Cmd { return v.password(kind.Password) })
			case *botdesktopv1.InstallEvent_Done:
				v.kit.Apply(func() tea.Cmd {
					text, ok := ended(kind.Done.GetCode())
					return finish(text, ok, kind.Done.GetStatus())
				})
				return
			}
		}
	}()
	return v.hub.Push(page)
}

// Form of one field asking the sudo password the install waits on; leaving it stops the install
func (v *view) password(id string) tea.Cmd {
	answer := func(password string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			_, err := v.service.Password(v.kit.Env.Ctx, &botdesktopv1.PasswordRequest{Id: id, Password: password})
			return func() tea.Cmd {
				if err != nil {
					return v.hub.Fail(strs.Full(tui.Reason(err)))
				}
				return nil
			}
		})
	}
	var form *cards.Form
	form = cards.NewForm(v.kit.Env, cards.FormSpec{
		Title: "Sudo password",
		Fields: []cards.Field{{ID: "password", Label: "Password", Kind: cards.FieldSecret, Required: true,
			Help: "The password you type for sudo on this computer. It goes to sudo once and is never kept."}},
		Submit: "Install",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(v.hub.Close(form), answer(values["password"]))
		},
		Cancel: func(*cards.Form) tea.Cmd {
			return tea.Batch(v.hub.Close(form), answer(""))
		},
	})
	return v.hub.Push(form)
}

// Follows the changes of the desktop until the page closes: a start or stop and a change of hands, each said as it comes
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
					v.status = kind.Status
				case *botdesktopv1.Change_Lease:
					if v.status != nil {
						v.status.Lease = kind.Lease
					}
				}
				v.list.Refresh()
				return nil
			})
		}
	}()
}
