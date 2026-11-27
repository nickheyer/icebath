package app

import (
	"errors"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the backends list
const backendsContext = "tui.backends"

var backendActions = []keys.Action{
	{ID: "tui.backends.read", Context: backendsContext, Label: "read", Doc: "read the checks of the backend and how to set it up", Default: []string{"enter"}},
	{ID: "tui.backends.use", Context: backendsContext, Label: "use", Doc: "run the agent's commands on the backend", Default: []string{"u"}},
	{ID: "tui.backends.prepare", Context: backendsContext, Label: "prepare", Doc: "ready the backend ahead of its first command, such as building its image", Default: []string{"p"}},
	{ID: "tui.backends.settings", Context: backendsContext, Label: "settings", Doc: "open the settings of the backend", Default: []string{"s"}},
}

func backendsScreen(conn backendsConn) tui.Screen {
	return tui.Screen{
		ID:      "backends",
		Title:   "Terminal backends",
		Short:   "Where the agent's commands can run: what each needs on this machine, whether it is ready, and readying one",
		Slash:   []string{"backends"},
		Usage:   "[@profile]",
		Actions: backendActions,
		Where:   map[string]string{backendsContext: "terminal backends"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return (&backends{kit: kit, conn: conn, profile: name}).open()
		},
	}
}

// Clients the backends screen calls
type backendsConn struct {
	backends terminalv1connect.BackendServiceClient
	config   configv1connect.ConfigServiceClient
}

type backends struct {
	kit  *tui.Kit
	conn backendsConn
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	rows    []*terminalv1.Backend
	loaded  bool
	err     error
}

func (b *backends) open() tea.Cmd {
	title := "Terminal backends"
	if b.profile != "" {
		title += " of " + b.profile
	}
	b.list = cards.NewList(b.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   b.kit.Env.Theme.Glyphs.Terminal,
		Key:     "tui:backends",
		Context: backendsContext,
		Primary: "tui.backends.read",
		Columns: []render.Column{{Title: "Backend"}, {Title: "State"}, {Title: "About", Flex: 3}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, backend := range b.rows {
				rows[i] = []string{backend.GetTitle(), b.state(backend), backend.GetAbout()}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(b.rows) {
				return nil
			}
			return b.checks(b.rows[i])
		},
		Empty:   func() string { return tui.Waiting(b.loaded, b.err, "No terminal backends.") },
		OnFocus: func(*cards.List) tea.Cmd { return b.load() },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.backends.read":     func(_ *cards.List, i int) tea.Cmd { return b.read(b.rows[i]) },
			"tui.backends.use":      func(_ *cards.List, i int) tea.Cmd { return b.use(b.rows[i]) },
			"tui.backends.prepare":  func(_ *cards.List, i int) tea.Cmd { return b.prepare(b.rows[i]) },
			"tui.backends.settings": func(_ *cards.List, i int) tea.Cmd { return b.settings(b.rows[i]) },
		},
	})
	b.list.ToggleDetail()
	b.hub = b.kit.Hub("backends", b.list)
	return tea.Batch(b.kit.WM.ShowOverlay(b.hub), b.load())
}

// Reads the backends as the profile sees them
func (b *backends) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := b.conn.backends.Backends(b.kit.Env.Ctx, &terminalv1.BackendsRequest{Profile: b.profile})
		return func() tea.Cmd {
			b.err, b.loaded, b.rows = err, true, response.GetBackends()
			b.list.Refresh()
			return nil
		}
	})
}

// Whether the profile's commands run on the backend, and whether it can run, coloured by how it stands
func (b *backends) state(backend *terminalv1.Backend) string {
	t := b.kit.Env.Theme
	switch {
	case backend.GetUnavailable() != "" && backend.GetSelected():
		return render.Bad("in use · unavailable", t)
	case backend.GetUnavailable() != "":
		return render.Dim("unavailable", t)
	case backend.GetSelected():
		return render.Good("in use", t)
	}
	for _, check := range backend.GetChecks() {
		if !check.GetOk() {
			return render.Warm("needs setup", t)
		}
	}
	return render.Dim("ready", t)
}

// Checks of the backend as lines, why it can't run first
func (b *backends) checks(backend *terminalv1.Backend) []string {
	t := b.kit.Env.Theme
	var lines []string
	if reason := backend.GetUnavailable(); reason != "" {
		lines = append(lines, render.Bad("Unavailable: "+reason, t))
	}
	for _, check := range backend.GetChecks() {
		line := strings.TrimSpace(check.GetLabel() + " " + check.GetDetail())
		if check.GetOk() {
			lines = append(lines, render.Good("✓ ", t)+line)
		} else {
			lines = append(lines, render.Bad("✗ ", t)+line)
		}
	}
	return lines
}

// Pages what the backend needs, how it stands and how to set it up
func (b *backends) read(backend *terminalv1.Backend) tea.Cmd {
	t := b.kit.Env.Theme
	page := cards.NewPage(b.kit.Env, cards.PageSpec{
		Title: backend.GetTitle(),
		Key:   "tui:backends:" + backend.GetName(),
		Lines: func(w int) []string {
			width := max(w, 1)
			var lines []string
			add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
			add(backend.GetAbout())
			add(render.Dim("terminal.backend: "+backend.GetName(), t))
			if backend.GetSelected() {
				add(render.Good("The agent's commands run here.", t))
			}
			lines = append(lines, "")
			if checks := b.checks(backend); len(checks) > 0 {
				lines = append(lines, render.Heading("Checks", t))
				for _, check := range checks {
					add(check)
				}
				lines = append(lines, "")
			}
			if setup := backend.GetSetup(); len(setup) > 0 {
				lines = append(lines, render.Heading("Setting it up", t))
				for _, line := range setup {
					add("• " + line)
				}
				lines = append(lines, "")
			}
			add(render.Dim("u runs the agent's commands here · p readies it · s opens its settings", t))
			return lines
		},
	})
	return b.hub.Push(page)
}

// Runs the profile's commands on the backend from the next one on
func (b *backends) use(backend *terminalv1.Backend) tea.Cmd {
	if backend.GetSelected() {
		return wm.Notify("The agent's commands already run on "+backend.GetTitle(), wm.Info)
	}
	return tui.Call(b.hub, func() (*configv1.SetResponse, error) {
		return b.conn.config.Set(b.kit.Env.Ctx, &configv1.SetRequest{Profile: b.profile, Key: terminalconfig.Backend, Value: backend.GetName()})
	}, func(*configv1.SetResponse) tea.Cmd {
		note := "The agent's commands now run on " + backend.GetTitle()
		if reason := backend.GetUnavailable(); reason != "" {
			return tea.Batch(b.load(), wm.Notify(note+", which can't run yet: "+reason, wm.Warn))
		}
		return tea.Batch(b.load(), wm.Notify(note, wm.Success))
	})
}

// Opens the backend's own settings
func (b *backends) settings(backend *terminalv1.Backend) tea.Cmd {
	section := backend.GetSection()
	if section == "" {
		return wm.Notify(backend.GetTitle()+" has no settings of its own", wm.Info)
	}
	argument := section
	if b.profile != "" {
		argument = "@" + b.profile + " " + section
	}
	return tea.Batch(b.kit.WM.DismissCard(b.hub), b.kit.Open(configapp.Settings, argument))
}

// Readies the backend on a page that follows how it goes, carried on to its end when the page is closed and told then
func (b *backends) prepare(backend *terminalv1.Backend) tea.Cmd {
	var lines []string
	ended := false
	page := cards.NewPage(b.kit.Env, cards.PageSpec{
		Title:  "Readying " + backend.GetTitle(),
		Key:    "tui:backends:prepare:" + backend.GetName(),
		Follow: true,
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
			}
			if len(out) == 0 {
				out = append(out, render.Dim("Starting…", b.kit.Env.Theme))
			}
			return out
		},
	})
	ctx := b.kit.Env.Ctx
	finish := func(text string, failed bool) {
		b.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended = true
			if failed {
				lines = append(lines, render.Bad(text, b.kit.Env.Theme))
				return tea.Batch(b.load(), b.hub.Fail(text))
			}
			lines = append(lines, render.Good(text, b.kit.Env.Theme))
			return tea.Batch(b.load(), b.hub.Done(text))
		})
	}
	go func() {
		stream, err := b.conn.backends.Prepare(ctx, &terminalv1.PrepareRequest{Backend: backend.GetName(), Profile: b.profile})
		if err != nil {
			finish(tui.Reason(err), true)
			return
		}
		defer stream.Close()
		for {
			response, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish(backend.GetTitle()+" is ready", false)
				return
			case err != nil:
				if ctx.Err() == nil {
					finish(tui.Reason(err), true)
				}
				return
			case response.GetDone() && response.GetError() != "":
				finish(response.GetError(), true)
				return
			case response.GetDone():
				finish(backend.GetTitle()+" is ready", false)
				return
			}
			b.kit.Apply(func() tea.Cmd {
				lines = append(lines, response.GetLine())
				return nil
			})
		}
	}()
	return b.hub.Push(page)
}
