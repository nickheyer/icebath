// Page of Browser in the settings menu: your own browser, connected and disconnected there and kept for every session, closing the browser
// that holds your profile, where pages open with the page of each service, and the options of the browser
package app

import (
	"errors"
	"io"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Browser, the page of its area
const Screen = "browser"

// Address a connect tries when given none: the debugging port of a browser on this computer
const defaultURL = "http://127.0.0.1:9222"

// Options of Browser in the order a person sets them up; any other everyday option of the browser follows them
var firstOptions = []string{browserconfig.CloudProvider, browserconfig.Engine, browserconfig.Headed, browserconfig.UseRealProfile,
	browserconfig.DialogPolicy}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := browserv1connect.NewBrowserServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupAbilities,
		Order: 70,
		Title: "Browser",
		Short: "Connect the agent to your own browser",
		Slash: []string{"browser"},
		Usage: "[connect [address]|disconnect|status|close-profile|service] [@profile]",
		Area:  browserconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return servicePages(kit, service, name) },
		Pickers: map[string]tui.Picker{
			browserconfig.PickPlace:   {Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return pickPlace(kit, hub, service, ask) }},
			browserconfig.PickProgram: {Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return pickProgram(kit, hub, service, ask) }},
			browserconfig.PickProfile: {Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd { return pickProfile(kit, hub, service, ask) }},
		},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			p := &page{kit: kit, service: service, settings: settings, profile: name}
			return p.open(strings.TrimSpace(rest))
		},
	}}
}

// The page of Browser for one profile
type page struct {
	kit      *tui.Kit
	service  browserv1connect.BrowserServiceClient
	settings configv1connect.ConfigServiceClient
	profile  string
	hub      *cards.Hub
	root     *cards.List
	options  *configapp.Rows
	views    cards.Watched
	status   *browserv1.StatusResponse
	places   *browserv1.ServicesResponse
	loaded   bool
	err      error
	// A view was opened over the first view, which reads again once it is back on top
	opened bool
}

// Row of the first view: your own browser, closing it, an option, the page of a service, or the row of the advanced options
type row struct {
	own, close bool
	setting    *configv1.Setting
	place      *browserv1.Service
	advanced   bool
}

func (p *page) open(argument string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Browser",
		Glyph:   t.Glyphs.Fetch,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group: func(i int) string {
			if r := shown[i]; r.own || r.close {
				return ""
			}
			return "Options"
		},
		Detail: func(i int) []string { return p.detail(shown[i], p.root.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.own && p.status.GetConnected():
				return []wm.Hint{{Action: "hub.use", Label: "disconnect"}}
			case r.own:
				return []wm.Hint{{Action: "hub.use", Label: "connect"}}
			case r.close:
				return []wm.Hint{{Action: "hub.use", Label: "close"}}
			case r.place != nil, r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i].setting)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.own && p.status.GetConnected():
					return p.disconnect()
				case r.own:
					return p.ask()
				case r.close:
					return p.confirmClose(l)
				case r.setting != nil:
					p.opened = r.setting.GetPicks() != ""
					return p.options.Change(r.setting)
				case r.place != nil:
					p.opened = true
					return p.placePage(r.place.GetName())
				}
				if _, _, open := p.browserOptions(); open != nil {
					p.opened = true
					return open()
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(nil) },
		},
		OnFocus: func(*cards.List) tea.Cmd {
			if !p.opened || p.hub.Depth() > 0 {
				return nil
			}
			p.opened = false
			return p.load(nil)
		},
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.run(argument) }))
}

// What the page is, then while a variable of the environment names the browser, that it wins
func (p *page) intro() []string {
	lines := []string{"The browser the agent uses to open web pages."}
	if variable := p.status.GetVariable(); p.loaded && variable != "" {
		lines = append(lines, render.Warm(variable+" names the browser the agent uses, which wins while it is set.", p.kit.Env.Theme))
	}
	return lines
}

// Runs what the command names: connecting, disconnecting, closing your browser, or the page of a service; nothing more for the state
func (p *page) run(argument string) tea.Cmd {
	words := strings.Fields(argument)
	if len(words) == 0 {
		return nil
	}
	switch action := strings.ToLower(words[0]); action {
	case "status":
		return nil
	case "connect":
		address := ""
		if len(words) > 1 {
			address = strings.Join(words[1:], " ")
		}
		return p.connect(address)
	case "disconnect":
		return p.disconnect()
	case "close-profile":
		return p.confirmClose(p.root)
	default:
		for _, place := range p.places.GetServices() {
			if strings.EqualFold(place.GetName(), action) || strings.EqualFold(place.GetTitle(), argument) {
				return p.placePage(place.GetName())
			}
		}
		return p.hub.Fail("/browser takes connect, disconnect, status, close-profile or the name of a service, not " + words[0] + ".")
	}
}

// Rows of the first view
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	rows := []row{{own: true}, {close: true}}
	shown, advanced, _ := p.browserOptions()
	for _, setting := range shown {
		rows = append(rows, row{setting: setting})
	}
	for _, place := range p.places.GetServices() {
		if place.GetSection() != "" {
			rows = append(rows, row{place: place})
		}
	}
	if advanced != nil {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Options of the browser in the order a person sets them up, and the cells of the row of the advanced ones with what it opens; the advanced
// ones in place, no row, while they are few
func (p *page) browserOptions() (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Section(browserconfig.Key)
	advanced, open, apart := p.options.Advanced(browserconfig.Key)
	if !apart {
		everyday = append(everyday, rare...)
	}
	ordered := slices.Clone(everyday)
	rank := func(s *configv1.Setting) int {
		if at := slices.Index(firstOptions, s.GetKey()); at >= 0 {
			return at
		}
		return len(firstOptions)
	}
	slices.SortStableFunc(ordered, func(a, b *configv1.Setting) int { return rank(a) - rank(b) })
	return ordered, advanced, open
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.own && p.status.GetConnected():
		return []string{"Your own browser", "", render.Good("Connected to "+p.status.GetUrl(), t)}
	case r.own:
		return []string{"Your own browser", "", "Not connected"}
	case r.close:
		return []string{"Close your browser", "", ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.place != nil:
		return []string{r.place.GetTitle(), opens, stateWords(r.place, t)}
	}
	_, advanced, _ := p.browserOptions()
	return advanced
}

// State of a place pages open, in the color of how it stands
func stateWords(place *browserv1.Service, t theme.Theme) string {
	if place.GetReady() {
		return place.GetState()
	}
	return render.Warm(place.GetState(), t)
}

// Lines under the rows about the row in hand
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.own && p.status.GetConnected():
		return []string{"Every session of the agent browses in your browser, signed in as you are.",
			render.Dim("Disconnecting lets the agent open a browser of its own again.", t)}
	case r.own:
		return []string{"Let the agent browse in a browser of yours, such as Chrome with remote debugging on, where you are signed in."}
	case r.close:
		return []string{"Close the browser that holds your profile, so the agent can copy your sign-ins.",
			render.Dim("It asks first, since tabs you didn't save are lost.", t)}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	case r.place != nil:
		return []string{r.place.GetAbout() + "."}
	}
	return []string{"Settings of the browser that few people change."}
}

// Reads the browser and where pages open, then the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := p.kit.Env.Ctx
		status, err := p.service.Status(ctx, &browserv1.StatusRequest{Profile: p.profile})
		var places *browserv1.ServicesResponse
		if err == nil {
			places, err = p.service.Services(ctx, &browserv1.ServicesRequest{Profile: p.profile})
		}
		return func() tea.Cmd {
			// What was read of the browser shows whether or not the options can be read after it
			p.loaded, p.err = true, err
			if err == nil {
				p.status, p.places = status, places
			}
			p.refresh()
			return p.options.Load(func() tea.Cmd {
				p.refresh()
				if then == nil || err != nil {
					return nil
				}
				return then()
			})
		}
	})
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

// Form asking the address of your browser, empty to find or start the one on this computer
func (p *page) ask() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title: "Connect",
		Fields: []cards.Field{{ID: "url", Label: "Address", Help: "The address of a Chrome, Edge or Brave browser with remote debugging on, " +
			"such as " + defaultURL + ". Leave it empty to find or start the one on this computer."}},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(p.hub.Close(form), p.connect(strings.TrimSpace(values["url"])))
		},
	})
	return p.hub.Push(form)
}

// Connects on a page that shows each step as the server tells it and how it ended, carried on to its end when the page is closed; the
// address is kept for every session once it connects
func (p *page) connect(address string) tea.Cmd {
	t := p.kit.Env.Theme
	shownAddress := address
	if shownAddress == "" {
		shownAddress = defaultURL
	}
	lines := []string{render.Dim("Looking for a browser with remote debugging at "+shownAddress+"…", t)}
	ended, closed, good := false, false, false
	note := ""
	say := func() tea.Cmd {
		if good {
			return p.hub.Done(note)
		}
		return p.hub.Fail(note)
	}
	view := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  "Connect",
		Key:    Screen + ":connect",
		Follow: true,
		Lines:  func(int) []string { return lines },
		Closed: func(*cards.Page) tea.Cmd {
			closed = true
			if ended {
				return say()
			}
			return nil
		},
	})
	// How the connect ended is the last line of the page while it shows, and the note of the page of Browser once it closes
	finish := func(text string, ok bool) {
		p.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended, good, note = true, ok, text
			if ok {
				lines = append(lines, "", render.Good(text, t))
			} else {
				lines = append(lines, "", render.Bad(text, t))
			}
			if closed {
				return tea.Batch(say(), p.load(nil))
			}
			return p.load(nil)
		})
	}
	ctx := p.kit.Env.Ctx
	go func() {
		stream, err := p.service.Connect(ctx, &browserv1.ConnectRequest{Profile: p.profile, Url: address})
		if err != nil {
			finish(tui.Reason(err), false)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish("The connect stopped without saying whether it worked.", false)
				return
			case err != nil:
				if ctx.Err() == nil {
					finish(tui.Reason(err), false)
				}
				return
			}
			switch kind := event.GetEvent().(type) {
			case *browserv1.ConnectEvent_Progress:
				p.kit.Apply(func() tea.Cmd {
					text := kind.Progress.GetMessage()
					if kind.Progress.GetLevel() == "error" {
						text = render.Warm(text, t)
					}
					lines = append(lines, text)
					return nil
				})
			case *browserv1.ConnectEvent_Connected:
				// The address that was tried, when the note of how it ended leaves it out
				note := kind.Connected.GetNote()
				if at := kind.Connected.GetUrl(); at != "" && !strings.Contains(note, at) {
					note = strings.TrimSpace(note + " The address tried was " + at + ".")
				}
				finish(note, kind.Connected.GetConnected())
				return
			}
		}
	}()
	return p.hub.Push(view)
}

// Disconnects your browser and forgets its address, saying so and that a variable of the environment wins when it names one
func (p *page) disconnect() tea.Cmd {
	p.hub.Busy("Disconnecting…")
	return tui.Call(p.hub, func() (*browserv1.DisconnectResponse, error) {
		return p.service.Disconnect(p.kit.Env.Ctx, &browserv1.DisconnectRequest{Profile: p.profile})
	}, func(response *browserv1.DisconnectResponse) tea.Cmd {
		said := p.hub.Done(response.GetNote())
		if response.GetHeld() {
			said = p.hub.Say(response.GetNote(), cards.NoteInfo)
		}
		return tea.Batch(said, p.load(nil))
	})
}

// Asks before closing the browser that holds your profile, since its unsaved tabs go with it
func (p *page) confirmClose(l *cards.List) tea.Cmd {
	l.Confirm(cards.Question{Text: "Your browser closes with every window it has open, so the agent can copy your sign-ins. Tabs you didn't " +
		"save are lost.", Verb: "Close", Deletes: true}, p.closeProfile)
	return nil
}

func (p *page) closeProfile() tea.Cmd {
	p.hub.Busy("Closing your browser…")
	return tui.Call(p.hub, func() (*browserv1.CloseProfileResponse, error) {
		return p.service.CloseProfile(p.kit.Env.Ctx, &browserv1.CloseProfileRequest{Profile: p.profile})
	}, func(reply *browserv1.CloseProfileResponse) tea.Cmd {
		if reply.GetClosed() {
			return p.hub.Done(reply.GetMessage())
		}
		return p.hub.Fail(reply.GetMessage())
	})
}
