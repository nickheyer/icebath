package app

import (
	"errors"
	"io"
	"strconv"
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

// Keymap context of the places to run commands
const backendsContext = "tui.backends"

var backendActions = []keys.Action{
	{ID: "tui.backends.use", Context: backendsContext, Label: "choose", Doc: "Run the agent's commands in the highlighted place", Default: []string{"enter"}},
	{ID: "tui.backends.prepare", Context: backendsContext, Label: "prepare", Doc: "Get the highlighted place ready, such as building its image", Default: []string{"p"}},
	{ID: "tui.backends.read", Context: backendsContext, Label: "how to set up", Doc: "Read what the highlighted place needs and how to set it up", Default: []string{"r"}},
	{ID: "tui.backends.settings", Context: backendsContext, Label: "open settings", Doc: "Open the settings of the highlighted place", Default: []string{"s"}},
}

func backendsScreen(conn backendsConn) tui.Screen {
	return tui.Screen{
		ID:      "backends",
		Group:   tui.GroupAbilities,
		Order:   50,
		Title:   "Places to run commands",
		Short:   "Choose and prepare where the agent's commands run",
		Slash:   []string{"backends"},
		Usage:   "[@profile]",
		Actions: backendActions,
		Where:   map[string]string{backendsContext: "places to run commands"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return (&backends{kit: kit, conn: conn, profile: name}).open()
		},
	}
}

// Clients the screen of the places to run commands calls
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
	title := "Places to run commands"
	if b.profile != "" {
		title += " · " + b.profile
	}
	t := b.kit.Env.Theme
	b.list = cards.NewList(b.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Terminal,
		Key:     "backends",
		Context: backendsContext,
		Primary: "tui.backends.use",
		Intro:   func() []string { return []string{"Choose where the agent runs its commands."} },
		Columns: []render.Column{{}, {}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, backend := range b.rows {
				rows[i] = []string{b.kit.Mark(true, backend.GetSelected()), backend.GetTitle(), b.state(backend), render.Dim(backend.GetAbout(), t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			backend := b.rows[i]
			var lines []string
			if !b.list.Shows(3) {
				lines = append(lines, backend.GetAbout())
			}
			return append(lines, b.facts(backend)...)
		},
		Empty:   func() string { return b.waiting() },
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   b.hints,
		OnFocus: func(*cards.List) tea.Cmd { return b.load() },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.backends.use":      func(_ *cards.List, i int) tea.Cmd { return b.use(b.rows[i]) },
			"tui.backends.prepare":  func(_ *cards.List, i int) tea.Cmd { return b.prepare(b.rows[i]) },
			"tui.backends.read":     func(_ *cards.List, i int) tea.Cmd { return b.read(b.rows[i]) },
			"tui.backends.settings": func(_ *cards.List, i int) tea.Cmd { return b.settings(b.rows[i]) },
			"hub.refresh":           func(*cards.List, int) tea.Cmd { return b.load() },
		},
	})
	b.list.ToggleDetail()
	b.hub = b.kit.Hub("backends", b.list)
	return tea.Batch(b.kit.WM.ShowOverlay(b.hub), b.load())
}

// Buttons for the place under the cursor: choosing it unless the commands run there already, readying it, reading about it and its
// settings when it has its own
func (b *backends) hints(_ *cards.List, i int) []wm.Hint {
	if i < 0 || i >= len(b.rows) {
		return nil
	}
	backend := b.rows[i]
	var out []wm.Hint
	if !backend.GetSelected() {
		out = append(out, wm.Hint{Action: "tui.backends.use"})
	}
	// Preparing is offered where there is something to prepare
	if backend.GetPrepares() {
		out = append(out, wm.Hint{Action: "tui.backends.prepare"})
	}
	out = append(out, wm.Hint{Action: "tui.backends.read"})
	if backend.GetSection() != "" {
		out = append(out, wm.Hint{Action: "tui.backends.settings", Label: "settings"})
	}
	return out
}

// What the list says while it has no rows
func (b *backends) waiting() string {
	switch {
	case b.err != nil:
		return "Could not check the places to run commands. " + sentence(tui.Reason(b.err)) + " " + b.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !b.loaded:
		return "Checking the places to run commands…"
	}
	return "No places to run commands are installed."
}

// Reads the places as the profile sees them
func (b *backends) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := b.conn.backends.Backends(b.kit.Env.Ctx, &terminalv1.BackendsRequest{Profile: b.profile})
		return func() tea.Cmd {
			b.err, b.loaded = err, true
			if err == nil {
				b.rows = response.GetBackends()
			}
			b.list.Refresh()
			return nil
		}
	})
}

// Whether the place can run commands now, colored by how it stands; the one in use is marked before its name
func (b *backends) state(backend *terminalv1.Backend) string {
	t := b.kit.Env.Theme
	switch {
	case backend.GetUnavailable() != "":
		return render.Bad("Can't run", t)
	case !ready(backend):
		return render.Warm("Needs setup", t)
	}
	return render.Good("Ready", t)
}

// Whether every check of the place passed
func ready(backend *terminalv1.Backend) bool {
	for _, check := range backend.GetChecks() {
		if !check.GetOk() {
			return false
		}
	}
	return true
}

// Facts a person acts on about the place: whether the commands run there, why it can't run, what was checked and where the steps to set
// it up are
func (b *backends) facts(backend *terminalv1.Backend) []string {
	t := b.kit.Env.Theme
	var lines []string
	if backend.GetSelected() {
		lines = append(lines, render.Good("The agent's commands run here.", t))
	}
	if reason := backend.GetUnavailable(); reason != "" {
		lines = append(lines, render.Bad("Can't run now. "+sentence(reason), t))
	}
	lines = append(lines, b.checks(backend)...)
	// The steps to set a place up are a page of their own: the pane points at it while the place is not ready, and says nothing of
	// setting up one that is
	if len(backend.GetSetup()) > 0 && (backend.GetUnavailable() != "" || !ready(backend)) {
		lines = append(lines, b.kit.Env.Keys.Display("tui.backends.read")+" shows how to set it up.")
	}
	return lines
}

// Checks of the place as lines, each marked passed or failed
func (b *backends) checks(backend *terminalv1.Backend) []string {
	t := b.kit.Env.Theme
	var lines []string
	// A failed check that says what the reason the place can't run says already is said once, by the reason
	reason := sentence(backend.GetUnavailable())
	for _, check := range backend.GetChecks() {
		line := strings.TrimSpace(check.GetLabel() + " " + check.GetDetail())
		if !check.GetOk() && backend.GetUnavailable() != "" && (strings.Contains(reason, line) || strings.Contains(line, strings.TrimSuffix(reason, "."))) {
			continue
		}
		if check.GetOk() {
			lines = append(lines, render.Good("✓ ", t)+line)
		} else {
			lines = append(lines, render.Bad("✗ ", t)+line)
		}
	}
	return lines
}

// Page of what the place is, how it stands, what was checked and how to set it up
func (b *backends) read(backend *terminalv1.Backend) tea.Cmd {
	t := b.kit.Env.Theme
	page := cards.NewPage(b.kit.Env, cards.PageSpec{
		Title: backend.GetTitle(),
		Key:   "backends:" + backend.GetName(),
		Lines: func(w int) []string {
			width := max(w, 1)
			var lines []string
			add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
			add(backend.GetAbout())
			if backend.GetSelected() {
				add(render.Good("The agent's commands run here.", t))
			}
			if reason := backend.GetUnavailable(); reason != "" {
				add(render.Bad("Can't run now. "+sentence(reason), t))
			}
			if checks := b.checks(backend); len(checks) > 0 {
				lines = append(lines, "", render.Heading("What was checked", t))
				for _, check := range checks {
					add(check)
				}
			}
			if setup := backend.GetSetup(); len(setup) > 0 {
				lines = append(lines, "", render.Heading("How to set it up", t))
				for _, step := range setup {
					add("• " + step)
				}
			}
			return lines
		},
	})
	return b.hub.Push(page)
}

// Runs the profile's commands in the place from the next one on
func (b *backends) use(backend *terminalv1.Backend) tea.Cmd {
	if backend.GetSelected() {
		return b.hub.Say("The agent's commands already run on "+backend.GetTitle()+".", cards.NoteInfo)
	}
	value := strconv.Quote(backend.GetName())
	return tui.Call(b.hub, func() (*configv1.SetResponse, error) {
		return b.conn.config.Set(b.kit.Env.Ctx, &configv1.SetRequest{Profile: b.profile, Key: terminalconfig.Backend, Value: value})
	}, func(*configv1.SetResponse) tea.Cmd {
		note := "The agent's commands now run on " + backend.GetTitle() + "."
		var changed tea.Cmd
		if b.profile == "" {
			changed = b.kit.Changed(terminalconfig.Backend, value)
		}
		if reason := backend.GetUnavailable(); reason != "" {
			return tea.Batch(changed, b.load(), b.hub.Fail(note+" It can't run yet. "+sentence(reason)))
		}
		return tea.Batch(changed, b.load(), b.hub.Done(note))
	})
}

// Opens the place's own settings
func (b *backends) settings(backend *terminalv1.Backend) tea.Cmd {
	section := backend.GetSection()
	if section == "" {
		return nil
	}
	return tea.Batch(b.kit.WM.DismissCard(b.hub), b.kit.Open(configapp.Settings, profile.Join(b.profile, section)))
}

// Readies the place on a page that follows how it goes, carried on to its end when the page is closed and told then
func (b *backends) prepare(backend *terminalv1.Backend) tea.Cmd {
	var lines []string
	ended := false
	page := cards.NewPage(b.kit.Env, cards.PageSpec{
		Title:  "Getting " + backend.GetTitle() + " ready",
		Key:    "backends:prepare:" + backend.GetName(),
		Follow: true,
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
			}
			if len(out) == 0 {
				out = append(out, render.Dim("Starting to get "+backend.GetTitle()+" ready…", b.kit.Env.Theme))
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
	failed := func(reason string) {
		finish("Could not get "+backend.GetTitle()+" ready. "+sentence(reason), true)
	}
	go func() {
		stream, err := b.conn.backends.Prepare(ctx, &terminalv1.PrepareRequest{Backend: backend.GetName(), Profile: b.profile})
		if err != nil {
			failed(tui.Reason(err))
			return
		}
		defer stream.Close()
		for {
			response, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish(backend.GetTitle()+" is ready.", false)
				return
			case err != nil:
				if ctx.Err() == nil {
					failed(tui.Reason(err))
				}
				return
			case response.GetDone() && response.GetError() != "":
				failed(response.GetError())
				return
			case response.GetDone():
				finish(backend.GetTitle()+" is ready.", false)
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
