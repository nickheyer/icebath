package tui

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const again = 2 * time.Second

var unserved = []string{"tui.agents", "tui.editor", "tui.paste.expand", "tui.paste.image", "tui.queue.delete", "tui.voice"}

type Screen struct {
	ID       string
	Title    string
	Short    string
	Startup  bool
	Commands []string
	Open     func(kit *Kit) tea.Cmd
}

type Kit struct {
	Env     *cards.Env
	WM      *wm.Model
	History cards.History

	screens []Screen
}

type ThemeMsg struct {
	Name string
}

type applyMsg func() tea.Cmd

type appliedMsg struct{}

func Theme(name string) tea.Cmd {
	return func() tea.Msg {
		return ThemeMsg{Name: name}
	}
}

func Async(work func() func() tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		return applyMsg(work())
	}
}

func (k *Kit) Apply(apply func() tea.Cmd) {
	k.Env.Send(applyMsg(apply))
}

func (k *Kit) Hub(key string, root wm.Card) *cards.Hub {
	return cards.NewHub(k.Env, key, root, k.WM.Size)
}

func (k *Kit) Open(id string) tea.Cmd {
	for _, screen := range k.screens {
		if screen.ID == id {
			return screen.Open(k)
		}
	}
	return nil
}

func (k *Kit) Focused() wm.Card {
	focused := k.WM.Focused()
	if tabs, ok := focused.(*wm.Tabs); ok {
		focused, _ = tabs.Active()
	}
	return focused
}

type history struct {
	mu      sync.Mutex
	entries []cards.Entry
}

func (h *history) Entries() []cards.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.entries)
}

func (h *history) Add(entry cards.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(slices.DeleteFunc(h.entries, func(e cards.Entry) bool { return e.Text == entry.Text }), entry)
}

func Run(ctx context.Context, screens []Screen) error {
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	bindings := keys.Default()
	cards.RegisterHubKeys(bindings)
	for _, id := range unserved {
		if err := bindings.Unbind("leader." + id); err != nil {
			return err
		}
	}
	lang.InstallKeys(bindings.Display)
	look, _ := theme.Named(theme.DefaultName)
	m := &model{screens: screens, kit: &Kit{
		screens: screens,
		History: new(history),
		Env: &cards.Env{
			Theme:      look,
			Keys:       bindings,
			Ctx:        ctx,
			WorkDir:    func() string { return directory },
			ProjectDir: directory,
			Root:       directory,
		},
		WM: wm.New(wm.WithTheme(look), wm.WithKeys(bindings), wm.WithWindowTitle("icebath")),
	}}
	m.kit.WM.SetCommandSource(m.commands)
	m.kit.WM.SetEmptyHints("palette")
	program := tty.NewProgram(context.WithoutCancel(ctx), m)
	m.kit.Env.Send = program.Send
	defer context.AfterFunc(ctx, func() { program.Send(tea.Quit()) })()
	_, err = program.Run()
	if m.ended || ctx.Err() != nil {
		return nil
	}
	return err
}

type model struct {
	kit         *Kit
	screens     []Screen
	spaces      int
	interrupted time.Time
	ended       bool
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.kit.WM.Init(), m.space())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tty.TerminateMsg, tty.GoneMsg:
		m.ended = true
		return m, tea.Quit
	case ThemeMsg:
		if look, known := theme.Named(msg.Name); known {
			m.kit.Env.Theme = look
			m.kit.WM.SetTheme(look)
		}
	case applyMsg:
		var cmd tea.Cmd
		if msg != nil {
			cmd = msg()
		}
		_, repaint := m.kit.WM.Update(appliedMsg{})
		return m, tea.Batch(cmd, repaint)
	}
	_, cmd := m.kit.WM.Update(msg)
	return m, cmd
}

func (m *model) View() tea.View {
	return m.kit.WM.View()
}

func (m *model) space() tea.Cmd {
	m.spaces++
	workspace, open := m.kit.WM.AddWorkspace(strconv.Itoa(m.spaces))
	cmds := []tea.Cmd{open, m.kit.WM.Switch(m.kit.WM.IndexOf(workspace))}
	started := false
	for _, screen := range m.screens {
		if screen.Startup {
			cmds, started = append(cmds, screen.Open(m.kit)), true
		}
	}
	if !started {
		cmds = append(cmds, m.home())
	}
	return tea.Batch(cmds...)
}

func (m *model) quit() tea.Cmd {
	m.ended = true
	return tea.Quit
}

func (m *model) interrupt() tea.Cmd {
	if time.Since(m.interrupted) < again {
		return m.quit()
	}
	m.interrupted = time.Now()
	return tea.Batch(m.kit.WM.Fire("session.stop"), m.kit.WM.Toast("Press "+m.kit.Env.Keys.Display("interrupt")+" again to quit", wm.Info, again))
}

func (m *model) commands() []wm.Command {
	commands := []wm.Command{
		{ID: "quit", Title: "Quit", Run: m.quit},
		{ID: "interrupt", Title: "Cancel", Run: m.interrupt},
		{ID: "workspace.new", Title: "New space", Run: m.space},
		{
			ID:    "workspace.close",
			Title: "Close space",
			When:  func() bool { return len(m.kit.WM.Workspaces()) > 1 },
			Run:   func() tea.Cmd { return m.kit.WM.RemoveWorkspace(m.kit.WM.ActiveIndex()) },
		},
		{ID: "tui.help", Title: "Help", Run: m.kit.WM.OpenPalette},
		{ID: "open.hubs", Title: lang.T(txt.HubsTitleHubs), Text: func() string { return lang.T(txt.HubsTitleHubs) }, Run: m.home},
	}
	for _, screen := range m.screens {
		open := func() tea.Cmd { return screen.Open(m.kit) }
		commands = append(commands, wm.Command{ID: "open." + screen.ID, Title: screen.Title, Text: func() string { return screen.Title }, Run: open})
		for _, id := range screen.Commands {
			commands = append(commands, wm.Command{ID: id, Title: screen.Title, Text: func() string { return screen.Title }, Run: open})
		}
	}
	return commands
}

func (m *model) home() tea.Cmd {
	var hub *cards.Hub
	hub = m.kit.Hub("hubs", cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   lang.T(txt.HubsTitleHubs),
		Glyph:   m.kit.Env.Theme.Glyphs.Ext,
		Key:     "hubs",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: lang.T(txt.HubsColScreen)}, {Title: lang.T(txt.HubsColWhatItCovers), Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(m.screens))
			for i, screen := range m.screens {
				rows[i] = []string{screen.Title, screen.Short}
			}
			return rows
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: lang.T(txt.HintsLabelOpen)}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(m.kit.WM.DismissCard(hub), m.screens[i].Open(m.kit))
			},
		},
	}))
	return m.kit.WM.ShowOverlay(hub)
}
