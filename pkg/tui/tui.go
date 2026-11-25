package tui

import (
	"context"
	"os"

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

type Screen struct {
	ID    string
	Title string
	Short string
	Open  func(kit *Kit) tea.Cmd
}

type Kit struct {
	Env *cards.Env
	WM  *wm.Model
}

type applyMsg func() tea.Cmd

type appliedMsg struct{}

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

func Run(ctx context.Context, screens []Screen) error {
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	bindings := keys.Default()
	cards.RegisterHubKeys(bindings)
	lang.InstallKeys(bindings.Display)
	look, _ := theme.Named(theme.DefaultName)
	m := &model{screens: screens, kit: &Kit{
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
	program := tty.NewProgram(ctx, m)
	m.kit.Env.Send = program.Send
	_, err = program.Run()
	return err
}

type model struct {
	kit     *Kit
	screens []Screen
}

func (m *model) Init() tea.Cmd {
	_, open := m.kit.WM.AddWorkspace("1", m.home())
	return tea.Batch(m.kit.WM.Init(), open)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	apply, ok := msg.(applyMsg)
	if !ok {
		_, cmd := m.kit.WM.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	if apply != nil {
		cmd = apply()
	}
	_, repaint := m.kit.WM.Update(appliedMsg{})
	return m, tea.Batch(cmd, repaint)
}

func (m *model) View() tea.View {
	return m.kit.WM.View()
}

func (m *model) commands() []wm.Command {
	commands := make([]wm.Command, len(m.screens))
	for i, screen := range m.screens {
		commands[i] = wm.Command{
			ID:    "open." + screen.ID,
			Title: screen.Title,
			Text:  func() string { return screen.Title },
			Run:   func() tea.Cmd { return screen.Open(m.kit) },
		}
	}
	return commands
}

func (m *model) home() wm.Card {
	return cards.NewList(m.kit.Env, cards.ListSpec{
		Title:   "icebath",
		Glyph:   m.kit.Env.Theme.Glyphs.Ext,
		Key:     "home",
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
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return m.screens[i].Open(m.kit) },
		},
	})
}
