// Page of the key proxy, opened from Sandbox as the control of its switch: hiding the keys turns on by setting the proxy up on a page that
// shows how it goes, and off by stopping it; starting a proxy that stopped, setting it up again, and its options
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	egressv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1/egressv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := egressv1connect.NewEgressServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID: "proxy",
		Pickers: map[string]tui.Picker{ironconfig.Proxy: {
			Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
				return (&page{kit: kit, hub: hub, service: service, settings: settings, profile: ask.Profile}).open()
			},
			Beside: func(key string) []string {
				if key == ironconfig.Enabled {
					return ironconfig.Options
				}
				return nil
			},
			Shown: func(ask tui.Ask) string {
				var on bool
				yaml.Unmarshal([]byte(ask.Values[ironconfig.Enabled]), &on)
				return strs.OnOff(on)
			},
		}},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return kit.Open(sandboxScreen, profile.Join(name, ironconfig.Key))
		},
	}}
}

// Screen of Sandbox, whose page opens this one at the word proxy
const sandboxScreen = "sandbox"

// The page of the key proxy for one profile
type page struct {
	kit      *tui.Kit
	hub      *cards.Hub
	service  egressv1connect.EgressServiceClient
	settings configv1connect.ConfigServiceClient
	profile  string
	list     *cards.List
	options  *configapp.Rows
	status   *egressv1.StatusResponse
	loaded   bool
	err      error
}

// Row of the page: the switch, starting the proxy, setting it up again, an option, or the row of the advanced options
type row struct {
	hide, start, again bool
	setting            *configv1.Setting
	advanced           bool
}

func (p *page) open() tea.Cmd {
	var shown []row
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Key proxy",
		Glyph:   p.kit.Env.Theme.Glyphs.Settings,
		Key:     "proxy",
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
			if shown[i].setting != nil || shown[i].advanced {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string { return p.detail(shown[i], p.list.Cut(i, 2)) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.hide && p.on():
				return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
			case r.hide:
				return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
			case r.start:
				return []wm.Hint{{Action: "hub.use", Label: "start"}}
			case r.again, r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i].setting)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.hide && p.on():
					return p.off()
				case r.hide:
					return p.turn(&egressv1.TurnRequest{On: true}, "Setting up")
				case r.start:
					return p.start()
				case r.again:
					return p.turn(&egressv1.TurnRequest{On: true, Again: true}, "Set up again")
				case r.setting != nil:
					return p.options.Change(r.setting)
				}
				_, open, _ := p.options.OwnCoveredAdvanced(ironconfig.Key, ironconfig.Enabled)
				return open()
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	p.list.ToggleDetail()
	p.options = configapp.NewRows(p.kit, p.hub, p.settings, p.profile)
	p.options.Watch(p.list)
	return tea.Batch(p.hub.Push(p.list), p.load())
}

// Whether the keys are hidden, as last read
func (p *page) on() bool { return p.status.GetEnabled() }

// Whether the proxy runs and listens, as last read
func (p *page) running() bool { return p.status.GetPid() != 0 && p.status.GetListening() }

// Whether the proxy is set up: its settings file, certificate and tokens written
func (p *page) configured() bool {
	return p.status.GetConfig() != "" && p.status.GetAuthority() != "" && len(p.status.GetMappings()) > 0
}

// What the page is, then while the keys are hidden but something keeps the proxy from hiding them, what and what to do
func (p *page) intro() []string {
	t := p.kit.Env.Theme
	lines := []string{"Keeps your API keys out of Docker sandboxes."}
	switch {
	case !p.loaded || p.err != nil || !p.on():
	case !p.configured():
		lines = append(lines, render.Warm("The proxy is not set up, so Docker sandboxes can't start. Set up again sets it up.", t))
	case !p.running():
		lines = append(lines, render.Warm("The proxy is not running, so Docker sandboxes can't start. Start the proxy starts it.", t))
	}
	return lines
}

func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	rows := []row{{hide: true}}
	if p.on() && p.configured() && !p.running() {
		rows = append(rows, row{start: true})
	}
	if p.on() {
		rows = append(rows, row{again: true})
	}
	// The options are kept beside the switch as one choice of Sandbox, so they are rows of their own only here
	everyday, rare := p.options.OwnCovered(ironconfig.Key, ironconfig.Enabled)
	_, _, apart := p.options.OwnCoveredAdvanced(ironconfig.Key, ironconfig.Enabled)
	if !apart {
		everyday = append(everyday, rare...)
	}
	for _, setting := range everyday {
		rows = append(rows, row{setting: setting})
	}
	if apart {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.hide:
		return []string{"Hide your keys", "", strs.OnOff(p.on())}
	case r.start:
		return []string{"Start the proxy", "", ""}
	case r.again:
		return []string{"Set up again", opens, ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	cells, _, _ := p.options.OwnCoveredAdvanced(ironconfig.Key, ironconfig.Enabled)
	return cells
}

// Lines under the rows about the row in hand
func (p *page) detail(r row, cut bool) []string {
	t := p.kit.Env.Theme
	switch {
	case r.hide && p.on() && p.running():
		return []string{fmt.Sprintf("Docker sandboxes reach providers through the proxy, which runs as process %d on port %d and puts your real "+
			"keys in on the way out.", p.status.GetPid(), p.status.GetPort()), render.Dim("Turning it off stops the proxy.", t)}
	case r.hide && p.on():
		return []string{"Docker sandboxes reach providers through the proxy, which puts your real keys in on the way out.",
			render.Dim("Turning it off stops the proxy.", t)}
	case r.hide:
		return []string{"Send the traffic of Docker sandboxes through a proxy on this computer that puts your real API keys in on the way out, " +
			"so a sandbox never holds them.", render.Dim("Turning it on sets the proxy up first, on a page that shows how it goes.", t)}
	case r.start:
		return []string{"Start the proxy with the settings it was set up with, so Docker sandboxes can start."}
	case r.again:
		return []string{"Make tokens for the providers you added since, and write the settings of the proxy again.",
			render.Dim("Tokens you have are kept, so running sandboxes keep working.", t)}
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	}
	return []string{"Settings of the key proxy that few people change."}
}

// Reads how the proxy stands and the options, then draws the page again
func (p *page) load() tea.Cmd {
	read := tui.Async(func() func() tea.Cmd {
		status, err := p.service.Status(p.kit.Env.Ctx, &egressv1.StatusRequest{Profile: p.profile})
		return func() tea.Cmd {
			p.loaded, p.err = true, err
			if err == nil {
				p.status = status
			}
			p.list.Refresh()
			return nil
		}
	})
	return tea.Batch(read, p.options.Load(nil))
}

// Stops hiding the keys, the proxy stopped with it
func (p *page) off() tea.Cmd {
	p.hub.Busy("Turning off…")
	ctx := p.kit.Env.Ctx
	request := &egressv1.TurnRequest{Profile: p.profile}
	go func() {
		done, err := p.receive(request, func(*egressv1.TurnEvent) {})
		p.kit.Apply(func() tea.Cmd { return p.ended(done, err, ctx) })
	}()
	return nil
}

// Says how a turn ended under the page, tells the screens that follow the switch of the profile in use, and reads the page again
func (p *page) ended(done *egressv1.TurnDone, err error, ctx context.Context) tea.Cmd {
	switch {
	case err != nil && ctx.Err() != nil:
		return nil
	case err != nil:
		return tea.Batch(p.hub.Fail(tui.Reason(err)), p.load())
	case !done.GetOk():
		return tea.Batch(p.hub.Fail(done.GetNote()), p.load())
	}
	var changed tea.Cmd
	if p.profile == "" {
		changed = p.kit.Changed(ironconfig.Enabled, strconv.FormatBool(done.GetOn()))
	}
	return tea.Batch(p.hub.Done(done.GetNote()), p.load(), changed)
}

// Runs a turn of the server, handing each event to each, until it says how it ended
func (p *page) receive(request *egressv1.TurnRequest, each func(*egressv1.TurnEvent)) (*egressv1.TurnDone, error) {
	request.Profile = p.profile
	stream, err := p.service.Turn(p.kit.Env.Ctx, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		switch {
		case errors.Is(err, io.EOF):
			return &egressv1.TurnDone{Note: "The key proxy stopped answering before it said whether it worked."}, nil
		case err != nil:
			return nil, err
		}
		if done := event.GetDone(); done != nil {
			return done, nil
		}
		each(event)
	}
}

// Hides the keys on a page that shows each step of the setup as it comes, carried on to its end when the page is closed; how it ended is
// the last line of the page while it shows, and the note of the page of the key proxy once it closes
func (p *page) turn(request *egressv1.TurnRequest, title string) tea.Cmd {
	t := p.kit.Env.Theme
	var lines []string
	var done *egressv1.TurnDone
	var failure error
	ended, closed := false, false
	view := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title:  title,
		Key:    "proxy:turn",
		Follow: true,
		Lines: func(int) []string {
			if len(lines) == 0 && !ended {
				return []string{render.Dim("Starting…", t)}
			}
			return lines
		},
		Closed: func(*cards.Page) tea.Cmd {
			closed = true
			if ended {
				return p.ended(done, failure, p.kit.Env.Ctx)
			}
			return nil
		},
	})
	ctx := p.kit.Env.Ctx
	go func() {
		answer, err := p.receive(request, func(event *egressv1.TurnEvent) {
			said := shown(event, t)
			p.kit.Apply(func() tea.Cmd { lines = append(lines, said...); return nil })
		})
		p.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended, done, failure = true, answer, err
			switch {
			case err != nil && ctx.Err() == nil:
				lines = append(lines, "", render.Bad(tui.Reason(err), t))
			case err == nil && done.GetOk():
				lines = append(lines, "", render.Good(done.GetNote(), t))
			case err == nil:
				lines = append(lines, "", render.Bad(done.GetNote(), t))
			}
			if closed {
				return p.ended(done, err, ctx)
			}
			return p.load()
		})
	}()
	return p.hub.Push(view)
}

// Lines a step of a turn shows on its page
func shown(event *egressv1.TurnEvent, t theme.Theme) []string {
	switch kind := event.GetKind().(type) {
	case *egressv1.TurnEvent_Line:
		text := kind.Line.GetText()
		switch kind.Line.GetTone() {
		case egressv1.Tone_TONE_STEP:
			return []string{"", render.Heading(text, t)}
		case egressv1.Tone_TONE_GOOD:
			return []string{render.Good(t.Glyphs.Success+" ", t) + text}
		case egressv1.Tone_TONE_WARN:
			return []string{render.Warm(text, t)}
		case egressv1.Tone_TONE_BAD:
			return []string{render.Bad(text, t)}
		case egressv1.Tone_TONE_DIM:
			return []string{render.Dim(text, t)}
		}
		return []string{text}
	case *egressv1.TurnEvent_Minted:
		var lines []string
		for _, m := range kind.Minted.GetMappings() {
			lines = append(lines, "A stand-in token for "+m.GetEnv()+".")
		}
		if uncovered := kind.Minted.GetUncovered(); len(uncovered) > 0 {
			lines = append(lines, render.Warm("The proxy can't cover "+strs.And(uncovered)+", whose real keys stay inside the sandbox.", t))
		}
		return lines
	}
	return nil
}

// Starts the proxy with what it was set up with, saying why it did not when it did not
func (p *page) start() tea.Cmd {
	p.hub.Busy("Starting the proxy…")
	return tui.Call(p.hub, func() (*egressv1.StartResponse, error) {
		return p.service.Start(p.kit.Env.Ctx, &egressv1.StartRequest{Profile: p.profile})
	}, func(started *egressv1.StartResponse) tea.Cmd {
		if refusal := started.GetRefusal(); refusal != "" {
			return tea.Batch(p.hub.Fail("The proxy did not start. "+refusal+" "+started.GetHint()), p.load())
		}
		return tea.Batch(p.hub.Done(fmt.Sprintf("The proxy runs as process %d on port %d.", started.GetPid(), started.GetPort())), p.load())
	})
}
