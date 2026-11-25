package tui

import (
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type Screen struct {
	Title   string
	Open    func(ctx context.Context) (Screen, error)
	Text    func(ctx context.Context) (string, error)
	Screens func(ctx context.Context) ([]Screen, error)
	Watch   func(ctx context.Context, show func(blocks []string)) error
	Send    func(ctx context.Context, text string) error
	Stop    func(ctx context.Context) error
}

func Run(ctx context.Context, screens []Screen) error {
	root := Screen{Title: "icebath", Screens: func(context.Context) ([]Screen, error) {
		return screens, nil
	}}
	_, err := tea.NewProgram(&model{ctx: ctx, root: root, dark: true}, tea.WithContext(ctx)).Run()
	return err
}

const (
	wide = 80
	rows = 5
)

type model struct {
	ctx    context.Context
	root   Screen
	dark   bool
	width  int
	height int
	tiles  []*pane
	focus  int
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.open())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		for _, tile := range m.tiles {
			for ; tile != nil; tile = tile.back {
				tile.restyle()
			}
		}
		return m, nil
	case openedMsg:
		return m, tea.Batch(msg.pane.opened(msg), m.focusOn(m.focus))
	case itemsMsg:
		return m, msg.pane.listed(msg)
	case blocksMsg:
		return m, msg.pane.shown(msg)
	case sentMsg:
		return m, msg.pane.sent(msg)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+q":
			return m, tea.Quit
		case "ctrl+c":
			if m.tiles[m.focus].screen.Stop == nil {
				return m, tea.Quit
			}
		case "alt+n":
			if (m.height-1)/(len(m.tiles)+1) < rows {
				return m, nil
			}
			return m, m.open()
		case "alt+q":
			tile := m.tiles[m.focus]
			for tile.back != nil {
				tile = tile.back
			}
			tile.cancel()
			if m.tiles = slices.Delete(m.tiles, m.focus, m.focus+1); len(m.tiles) == 0 {
				return m, m.open()
			}
			return m, m.focusOn(min(m.focus, len(m.tiles)-1))
		case "alt+j", "alt+down":
			return m, m.focusOn((m.focus + 1) % len(m.tiles))
		case "alt+k", "alt+up":
			return m, m.focusOn((m.focus + len(m.tiles) - 1) % len(m.tiles))
		case "alt+m":
			m.tiles[0], m.tiles[m.focus] = m.tiles[m.focus], m.tiles[0]
			return m, m.focusOn(0)
		}
		tile, cmd := m.tiles[m.focus].key(msg)
		if tile == m.tiles[m.focus] {
			return m, cmd
		}
		m.tiles[m.focus] = tile
		return m, tea.Batch(cmd, m.focusOn(m.focus))
	}
	return m, m.tiles[m.focus].update(msg)
}

func (m *model) open() tea.Cmd {
	tile, cmd := newPane(m.ctx, m, m.root, nil)
	m.tiles = append(m.tiles, tile)
	return tea.Batch(cmd, m.focusOn(len(m.tiles)-1))
}

func (m *model) focusOn(focus int) tea.Cmd {
	for _, tile := range m.tiles {
		tile.input.Blur()
	}
	m.focus = focus
	m.layout()
	return m.tiles[focus].focus()
}

func (m *model) solo() bool {
	return len(m.tiles) == 1 || (m.height-1)/len(m.tiles) < rows
}

func (m *model) layout() {
	height, count := m.height-1, len(m.tiles)
	for i, tile := range m.tiles {
		w, h := m.width, height
		switch {
		case m.solo():
		case m.width < wide:
			if h = height / count; i == count-1 {
				h += height % count
			}
		case i == 0:
			w = m.width * 6 / 10
		default:
			w = m.width - m.width*6/10
			if h = height / (count - 1); i == count-1 {
				h += height % (count - 1)
			}
		}
		tile.resize(w-2, h-2)
	}
}

func (m *model) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	if m.width < 20 || m.height < rows+1 {
		return view
	}
	frames := make([]string, len(m.tiles))
	for i, tile := range m.tiles {
		frames[i] = m.frame(tile, i == m.focus)
	}
	body := frames[m.focus]
	switch {
	case m.solo():
	case m.width < wide:
		body = lipgloss.JoinVertical(lipgloss.Left, frames...)
	default:
		body = lipgloss.JoinHorizontal(lipgloss.Top, frames[0], lipgloss.JoinVertical(lipgloss.Left, frames[1:]...))
	}
	hints := m.tiles[m.focus].hints() + " · alt+n new · alt+q close · alt+j/k focus · alt+m main · ctrl+q quit"
	view.SetContent(body + "\n" + lipgloss.NewStyle().Faint(true).MaxWidth(m.width).Render(hints))
	return view
}

func (m *model) frame(tile *pane, focused bool) string {
	color := lipgloss.Color("8")
	if focused {
		color = lipgloss.Color("12")
	}
	width, height := tile.width+2, tile.height+2
	title := lipgloss.NewStyle().MaxWidth(width - 6).Render(tile.screen.Title)
	top := "╭─ " + title + " " + strings.Repeat("─", width-5-lipgloss.Width(title)) + "╮"
	body := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderTop(false).BorderForeground(color).
		Width(width).Height(height - 1).MaxHeight(height - 1).Render(tile.render())
	return lipgloss.NewStyle().Foreground(color).Render(top) + "\n" + body
}
