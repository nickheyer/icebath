// The browser page: whether the profile's browser tools drive a live browser over DevTools, attaching them to one and detaching them, and
// closing the browser that holds the user's own profile
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

const usage = "usage: /browser [connect [url]|disconnect|status|close-profile]; /settings browser keeps an address"

// Address /browser connect tries when given none
const defaultURL = "http://127.0.0.1:9222"

var actions = []keys.Action{
	{ID: "tui.browser.connect", Context: browserContext, Label: "connect", Doc: "attach the browser tools to a browser with remote debugging",
		Default: []string{"c"}},
	{ID: "tui.browser.disconnect", Context: browserContext, Label: "disconnect", Doc: "detach the browser tools from the live browser",
		Default: []string{"d"}},
	{ID: "tui.browser.close", Context: browserContext, Label: "close profile", Doc: "close the browser that holds your own profile",
		Default: []string{"x"}},
	{ID: "tui.browser.refresh", Context: browserContext, Label: "refresh", Doc: "read the browser status again", Default: []string{"ctrl+r"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := browserv1connect.NewBrowserServiceClient(conn)
	return []tui.Screen{{
		ID:      "browser",
		Title:   "Browser",
		Short:   "connect the agent to your browser, or close the browser holding your profile",
		Slash:   []string{"browser"},
		Usage:   "[connect [url]|disconnect|status|close-profile] [@profile]",
		Actions: actions,
		Where:   map[string]string{browserContext: "browser"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			v := &view{kit: kit, service: service, profile: name}
			return v.open(strings.TrimSpace(rest))
		},
	}}
}

type view struct {
	kit     *tui.Kit
	service browserv1connect.BrowserServiceClient
	profile string
	hub     *cards.Hub
	page    *cards.Page
	status  *browserv1.StatusResponse
	log     []string
	loaded  bool
	err     error
}

func (v *view) open(argument string) tea.Cmd {
	v.page = cards.NewPage(v.kit.Env, cards.PageSpec{
		Title:   "Browser",
		Key:     "tui:browser",
		Context: browserContext,
		Follow:  true,
		Lines:   v.lines,
		Hints:   v.hints,
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"tui.browser.connect":    func(*cards.Page) tea.Cmd { return v.ask() },
			"tui.browser.disconnect": func(*cards.Page) tea.Cmd { return v.disconnect() },
			"tui.browser.close":      func(*cards.Page) tea.Cmd { return v.confirmClose() },
			"tui.browser.refresh":    func(*cards.Page) tea.Cmd { return v.load() },
		},
	})
	v.hub = v.kit.Hub("browser", v.page)
	return tea.Batch(v.kit.WM.ShowOverlay(v.hub), v.run(argument))
}

// Runs what the command names, the status when it names nothing
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
		return v.disconnect()
	case "close-profile":
		return tea.Batch(v.load(), v.confirmClose())
	}
	v.say(usage)
	return v.load()
}

func (v *view) say(text string) { v.log = append(v.log, text) }

func (v *view) lines(w int) []string {
	t := v.kit.Env.Theme
	width := max(w, 1)
	var lines []string
	add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
	switch {
	case v.status == nil:
		add(tui.Waiting(v.loaded, v.err, ""))
	case v.status.GetConnected():
		add("browser connected: " + v.status.GetUrl())
	default:
		add(render.Dim("browser not connected (try /browser connect <url> or set browser.cdp_url in config.yaml)", t))
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
	out := []wm.Hint{{Action: "tui.browser.connect"}}
	if v.status.GetConnected() {
		out = append(out, wm.Hint{Action: "tui.browser.disconnect"})
	}
	return append(out, wm.Hint{Action: "tui.browser.close"}, wm.Hint{Action: "tui.browser.refresh"})
}

func (v *view) fail(err error) tea.Cmd {
	v.loaded, v.err = true, err
	v.say(render.Bad(tui.Reason(err), v.kit.Env.Theme))
	return nil
}

func (v *view) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		status, err := v.service.Status(v.kit.Env.Ctx, &browserv1.StatusRequest{Profile: v.profile})
		return func() tea.Cmd {
			if err != nil {
				return v.fail(err)
			}
			v.loaded, v.err, v.status = true, nil, status
			return nil
		}
	})
}

// Form asking the address to connect to, the local debugging port when left as it is
func (v *view) ask() tea.Cmd {
	return v.hub.Push(cards.NewForm(v.kit.Env, cards.FormSpec{
		Title:  "Connect the browser tools",
		Intro:  []string{"Address of a Chromium-family browser with remote debugging; the local one on port 9222 is found or started"},
		Fields: []cards.Field{{ID: "url", Label: "address", Value: defaultURL}},
		Submit: "connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(v.hub.Pop(), v.connect(strings.TrimSpace(values["url"])))
		},
		Cancel: func(*cards.Form) tea.Cmd { return v.hub.Pop() },
	}))
}

// Connects, each step said on the page as the server tells it
func (v *view) connect(address string) tea.Cmd {
	if address == "" {
		address = defaultURL
	}
	v.say("checking Chromium-family browser remote debugging at " + address + "...")
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
					if kind.Progress.GetLevel() == "error" {
						v.say(render.Warm(kind.Progress.GetMessage(), v.kit.Env.Theme))
					} else {
						v.say(kind.Progress.GetMessage())
					}
					return nil
				})
			case *browserv1.ConnectEvent_Connected:
				v.kit.Apply(func() tea.Cmd {
					if kind.Connected.GetConnected() {
						url := kind.Connected.GetUrl()
						if url == "" {
							url = "(url unavailable)"
						}
						v.say(render.Good("Browser connected to live Chromium-family browser via CDP", v.kit.Env.Theme))
						v.say("Endpoint: " + url)
						v.say("next browser tool call will use this CDP endpoint")
					}
					return v.load()
				})
				return
			}
		}
	}()
	return nil
}

func (v *view) disconnect() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := v.service.Disconnect(v.kit.Env.Ctx, &browserv1.DisconnectRequest{Profile: v.profile})
		return func() tea.Cmd {
			if err != nil {
				return v.fail(err)
			}
			v.say("browser disconnected")
			return v.load()
		}
	})
}

// Asks before closing the browser that holds the user's own profile, since its unsaved tabs go with it
func (v *view) confirmClose() tea.Cmd {
	return v.hub.Push(cards.NewForm(v.kit.Env, cards.FormSpec{
		Title:  "Close your browser?",
		Intro:  []string{"Closes the browser that holds your own profile so the agent can use it. Tabs you did not save are lost."},
		Submit: "Close it",
		Done:   func(*cards.Form, map[string]string) tea.Cmd { return tea.Batch(v.hub.Pop(), v.closeProfile()) },
		Cancel: func(*cards.Form) tea.Cmd {
			v.say("Leave it open")
			return v.hub.Pop()
		},
	}))
}

func (v *view) closeProfile() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		reply, err := v.service.CloseProfile(v.kit.Env.Ctx, &browserv1.CloseProfileRequest{Profile: v.profile})
		return func() tea.Cmd {
			if err != nil {
				return v.fail(err)
			}
			if reply.GetClosed() {
				v.say(render.Good("✓ "+reply.GetMessage(), v.kit.Env.Theme))
			} else {
				v.say(render.Bad("✗ "+reply.GetMessage(), v.kit.Env.Theme))
			}
			return nil
		}
	})
}
