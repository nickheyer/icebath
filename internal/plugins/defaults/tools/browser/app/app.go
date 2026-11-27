// The browser screen: whether the agent's browser steps use a browser of the user's own over remote debugging, connecting them to one and
// disconnecting them, and closing the browser that holds the user's own profile
package app

import (
	"errors"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const browserContext = "tui.browser"

// Address a connect tries when given none: the debugging port of a browser on this computer
const defaultURL = "http://127.0.0.1:9222"

// Column of the list that holds what each row does, shown whole or not at all
const aboutColumn = 1

var actions = []keys.Action{
	{ID: "tui.browser.connect", Context: browserContext, Label: "connect", Doc: "Connect the agent to a browser that has remote debugging on",
		Default: []string{"c"}},
	{ID: "tui.browser.disconnect", Context: browserContext, Label: "disconnect", Doc: "Disconnect the agent from the browser",
		Default: []string{"d"}},
	{ID: "tui.browser.close", Context: browserContext, Label: "close browser", Doc: "Close the browser that holds your own profile",
		Default: []string{"x"}},
	{ID: "tui.browser.refresh", Context: browserContext, Label: "refresh", Doc: "Check the browser again", Default: []string{"ctrl+r"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := browserv1connect.NewBrowserServiceClient(conn)
	return []tui.Screen{{
		ID:      "browser",
		Group:   tui.GroupAbilities,
		Order:   70,
		Title:   "Browser",
		Short:   "Connect the agent to your own browser",
		Slash:   []string{"browser"},
		Usage:   "[connect [address]|disconnect|status|close-profile] [@profile]",
		Actions: actions,
		Where:   map[string]string{browserContext: "browser"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			v := &view{kit: kit, service: service, profile: name}
			return v.open(strings.TrimSpace(rest))
		},
	}}
}

// Something the screen does, one row each
type step struct {
	title, about string
	// Word on the button that does it
	verb string
	run  func(l *cards.List) tea.Cmd
}

type view struct {
	kit     *tui.Kit
	service browserv1connect.BrowserServiceClient
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	status  *browserv1.StatusResponse
	// What the last connect said as it went
	log    []string
	loaded bool
	err    error
	steps  []step
}

func (v *view) open(argument string) tea.Cmd {
	t := v.kit.Env.Theme
	title := "Browser"
	if v.profile != "" {
		title += " · " + v.profile
	}
	v.list = cards.NewList(v.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Fetch,
		Key:     "tui:browser",
		Context: cards.HubContext,
		// Its own keys stay in force on every row, beside the buttons the rows show
		Shared:  []string{browserContext},
		Primary: "hub.use",
		Intro:   v.intro,
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			v.steps = v.offered()
			cells := make([][]string, len(v.steps))
			for i, st := range v.steps {
				cells[i] = []string{st.title, render.Dim(st.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if v.list.Shows(aboutColumn) || i < 0 || i >= len(v.steps) {
				return nil
			}
			return []string{v.steps[i].about + "."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(v.steps) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: v.steps[i].verb}}
		},
		Free: map[string]bool{"tui.browser.connect": true, "tui.browser.disconnect": true, "tui.browser.close": true, "tui.browser.refresh": true,
			"hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":                func(l *cards.List, i int) tea.Cmd { return v.steps[i].run(l) },
			"tui.browser.connect":    func(*cards.List, int) tea.Cmd { return v.ask() },
			"tui.browser.disconnect": func(*cards.List, int) tea.Cmd { return v.disconnect() },
			"tui.browser.close":      func(l *cards.List, _ int) tea.Cmd { return v.confirmClose(l) },
			"tui.browser.refresh":    func(*cards.List, int) tea.Cmd { return v.load() },
			"hub.refresh":            func(*cards.List, int) tea.Cmd { return v.load() },
		},
	})
	v.list.ToggleDetail()
	v.hub = v.kit.Hub("browser", v.list)
	return tea.Batch(v.kit.WM.ShowOverlay(v.hub), v.run(argument))
}

// What the screen offers as things stand: connecting, disconnecting while connected, and closing the browser that holds your profile
func (v *view) offered() []step {
	connect := step{"Connect to your browser", "Find or start a browser with remote debugging on, then let the agent use it", "connect",
		func(*cards.List) tea.Cmd { return v.ask() }}
	if v.status.GetConnected() {
		connect.title = "Connect to another browser"
	}
	out := []step{connect}
	if v.status.GetConnected() {
		out = append([]step{{"Disconnect", "Let the agent open a browser of its own again", "disconnect", func(*cards.List) tea.Cmd { return v.disconnect() }}}, out...)
	}
	return append(out, step{"Close your browser", "Close the browser that holds your profile, so the agent can copy your sign-ins", "close",
		func(l *cards.List) tea.Cmd { return v.confirmClose(l) }})
}

// Lines above the rows: what this is and how the browser stands, then what the last connect said as it went
func (v *view) intro() []string {
	lines := []string{"The agent can do its browsing in your own browser, such as Chrome with remote debugging on, where you are signed in."}
	switch {
	case v.status == nil:
		lines = append(lines, tui.Waiting(v.loaded, v.err, ""))
	case v.status.GetConnected():
		lines = append(lines, "Connected to the browser at "+v.status.GetUrl()+". The agent's browsing happens there.")
	default:
		lines = append(lines, "Not connected. The agent opens a browser of its own. Attach address in /settings under Browser connects every session.")
	}
	if len(v.log) > 0 {
		lines = append(append(lines, ""), v.log...)
	}
	return lines
}

// Runs what the command names, the state of the browser when it names nothing
func (v *view) run(argument string) tea.Cmd {
	words := strings.Fields(argument)
	action := "status"
	if len(words) > 0 {
		action = strings.ToLower(words[0])
	}
	switch action {
	case "status":
		return v.load()
	case "connect":
		address := defaultURL
		if len(words) > 1 {
			address = strings.Join(words[1:], " ")
		}
		return tea.Batch(v.load(), v.connect(address))
	case "disconnect":
		return tea.Batch(v.load(), v.disconnect())
	case "close-profile":
		v.confirmClose(v.list)
		return v.load()
	}
	return tea.Batch(v.load(), v.hub.Fail("/browser takes connect, disconnect, status or close-profile, not "+words[0]+"."))
}

func (v *view) say(text string) {
	v.log = append(v.log, text)
	v.list.Refresh()
}

// Says why something failed under the screen and keeps it with what the connect said
func (v *view) fail(err error) tea.Cmd {
	text := tui.Reason(err)
	v.say(text)
	return v.hub.Fail(text)
}

func (v *view) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Status(v.kit.Env.Ctx, &browserv1.StatusRequest{Profile: v.profile})
		return func() tea.Cmd {
			v.loaded, v.err = true, err
			if err == nil {
				v.status = status
			}
			v.list.Refresh()
			return nil
		}
	})
}

// Form asking the address to connect to, the debugging port of this computer when left as it is
func (v *view) ask() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(v.kit.Env, cards.FormSpec{
		Title: "Connect to your browser",
		Intro: []string{"The address of a Chrome, Edge or Brave browser with remote debugging on.",
			"Leave it as it is to find or start the one on this computer."},
		Fields: []cards.Field{{ID: "url", Label: "Address", Value: defaultURL, Required: true}},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(v.hub.Close(form), v.connect(strings.TrimSpace(values["url"])))
		},
	})
	return v.hub.Push(form)
}

// Connects, each step the server tells kept above the rows and shown under them as it comes
func (v *view) connect(address string) tea.Cmd {
	if address == "" {
		address = defaultURL
	}
	v.log = nil
	v.say("Looking for a browser with remote debugging at " + address + ".")
	v.hub.Busy("Connecting to " + address + "…")
	ctx := v.kit.Env.Ctx
	go func() {
		stream, err := v.service.Connect(ctx, &browserv1.ConnectRequest{Profile: v.profile, Url: address})
		if err != nil {
			v.kit.Apply(func() tea.Cmd { return v.fail(err) })
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				v.kit.Apply(func() tea.Cmd {
					if v.hub.Waiting() {
						return tea.Batch(v.load(), v.hub.Fail("The connect stopped without saying whether it worked."))
					}
					return nil
				})
				return
			}
			if err != nil {
				if ctx.Err() == nil {
					v.kit.Apply(func() tea.Cmd { return v.fail(err) })
				}
				return
			}
			switch kind := event.GetEvent().(type) {
			case *browserv1.ConnectEvent_Progress:
				v.kit.Apply(func() tea.Cmd {
					v.say(kind.Progress.GetMessage())
					v.hub.Busy(kind.Progress.GetMessage())
					return nil
				})
			case *browserv1.ConnectEvent_Connected:
				v.kit.Apply(func() tea.Cmd {
					url := kind.Connected.GetUrl()
					if !kind.Connected.GetConnected() {
						text := "Couldn't connect to the browser at " + address + ". The steps above say why."
						v.say(text)
						return tea.Batch(v.load(), v.hub.Fail(text))
					}
					text := "Connected to the browser at " + url + ". The agent's next browser step uses it."
					if url == "" {
						text = "Connected to your browser. The agent's next browser step uses it."
					}
					v.say(text)
					return tea.Batch(v.load(), v.hub.Done(text))
				})
				return
			}
		}
	}()
	return nil
}

func (v *view) disconnect() tea.Cmd {
	v.hub.Busy("Disconnecting…")
	return tui.Async(func() func() tea.Cmd {
		_, err := v.service.Disconnect(v.kit.Env.Ctx, &browserv1.DisconnectRequest{Profile: v.profile})
		return func() tea.Cmd {
			if err != nil {
				return v.fail(err)
			}
			v.log = nil
			return tea.Batch(v.load(), v.hub.Done("Disconnected. The agent opens a browser of its own again."))
		}
	})
}

// Asks before closing the browser that holds the user's own profile, since its unsaved tabs go with it
func (v *view) confirmClose(l *cards.List) tea.Cmd {
	l.Confirm("Close your browser? Tabs you didn't save are lost, and the agent can then copy your sign-ins.", v.closeProfile)
	return nil
}

func (v *view) closeProfile() tea.Cmd {
	v.hub.Busy("Closing your browser…")
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.CloseProfile(v.kit.Env.Ctx, &browserv1.CloseProfileRequest{Profile: v.profile})
		return func() tea.Cmd {
			if err != nil {
				return v.fail(err)
			}
			if reply.GetClosed() {
				return v.hub.Done(reply.GetMessage())
			}
			return v.hub.Fail(reply.GetMessage())
		}
	})
}
