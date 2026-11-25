package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

type pane struct {
	model  *model
	screen Screen
	back   *pane
	ctx    context.Context
	cancel context.CancelFunc
	width  int
	height int

	loaded bool
	err    error
	note   string

	items  []Screen
	cursor int

	blocks   []string
	rendered map[string]string
	renderer *glamour.TermRenderer
	updates  chan blocksMsg
	view     viewport.Model
	input    textarea.Model
}

type openedMsg struct {
	pane   *pane
	screen Screen
	err    error
}

type itemsMsg struct {
	pane  *pane
	items []Screen
	err   error
}

type blocksMsg struct {
	pane   *pane
	blocks []string
	err    error
	ended  bool
}

type sentMsg struct {
	pane *pane
	text string
	err  error
}

func newPane(ctx context.Context, m *model, screen Screen, back *pane) (*pane, tea.Cmd) {
	ctx, cancel := context.WithCancel(ctx)
	p := &pane{model: m, screen: screen, back: back, ctx: ctx, cancel: cancel, updates: make(chan blocksMsg, 1), view: viewport.New(), input: textarea.New()}
	p.view.SoftWrap = true
	p.view.FillHeight = true
	p.input.ShowLineNumbers = false
	p.input.DynamicHeight = true
	p.input.MinHeight = 1
	p.input.MaxHeight = 8
	p.input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	p.restyle()
	return p, p.load()
}

func (p *pane) load() tea.Cmd {
	if p.screen.Open != nil {
		return func() tea.Msg {
			screen, err := p.screen.Open(p.ctx)
			return openedMsg{p, screen, err}
		}
	}
	if p.screen.Screens != nil {
		return func() tea.Msg {
			items, err := p.screen.Screens(p.ctx)
			return itemsMsg{p, items, err}
		}
	}
	watch := p.screen.Watch
	if watch == nil {
		watch = func(ctx context.Context, show func([]string)) error {
			if p.screen.Text == nil {
				return nil
			}
			text, err := p.screen.Text(ctx)
			if err == nil {
				show([]string{text})
			}
			return err
		}
	}
	go func() {
		err := watch(p.ctx, p.show)
		select {
		case p.updates <- blocksMsg{pane: p, err: err, ended: true}:
		case <-p.ctx.Done():
		}
	}()
	return p.listen
}

func (p *pane) show(blocks []string) {
	for {
		select {
		case p.updates <- blocksMsg{pane: p, blocks: blocks}:
			return
		default:
		}
		select {
		case <-p.updates:
		default:
		}
	}
}

func (p *pane) listen() tea.Msg {
	select {
	case msg := <-p.updates:
		return msg
	case <-p.ctx.Done():
		return nil
	}
}

func (p *pane) opened(msg openedMsg) tea.Cmd {
	if msg.err != nil {
		p.loaded, p.err = true, msg.err
		p.refresh(false)
		return nil
	}
	p.screen = msg.screen
	return p.load()
}

func (p *pane) listed(msg itemsMsg) tea.Cmd {
	p.items, p.err, p.loaded = msg.items, msg.err, true
	p.cursor = max(min(p.cursor, len(p.items)-1), 0)
	return nil
}

func (p *pane) shown(msg blocksMsg) tea.Cmd {
	follow := p.follow()
	p.loaded = true
	if msg.ended {
		p.err = msg.err
		p.refresh(follow)
		return nil
	}
	p.blocks, p.err = msg.blocks, nil
	p.refresh(follow)
	return p.listen
}

func (p *pane) sent(msg sentMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		p.note = "error: " + strings.ReplaceAll(msg.err.Error(), "\n", " ")
		if p.input.Value() == "" {
			p.input.SetValue(msg.text)
		}
		p.fit()
	case p.screen.Watch == nil:
		return p.load()
	}
	return nil
}

func (p *pane) key(msg tea.KeyPressMsg) (*pane, tea.Cmd) {
	switch pressed := msg.String(); {
	case (pressed == "esc" || pressed == "ctrl+c") && p.screen.Stop != nil:
		return p, func() tea.Msg {
			return sentMsg{pane: p, err: p.screen.Stop(p.ctx)}
		}
	case pressed == "esc" && p.back != nil:
		p.cancel()
		return p.back, p.back.load()
	case p.screen.Screens != nil:
		switch pressed {
		case "up", "k":
			p.cursor = max(p.cursor-1, 0)
		case "down", "j":
			p.cursor = max(min(p.cursor+1, len(p.items)-1), 0)
		case "enter":
			if p.cursor < len(p.items) {
				return newPane(p.ctx, p.model, p.items[p.cursor], p)
			}
		}
		return p, nil
	case p.screen.Send == nil:
		var cmd tea.Cmd
		p.view, cmd = p.view.Update(msg)
		return p, cmd
	case pressed == "pgup":
		p.view.PageUp()
		return p, nil
	case pressed == "pgdown":
		p.view.PageDown()
		return p, nil
	case pressed == "enter":
		text := strings.TrimSpace(p.input.Value())
		if text == "" {
			return p, nil
		}
		p.note = ""
		p.input.Reset()
		p.fit()
		return p, func() tea.Msg {
			return sentMsg{p, text, p.screen.Send(p.ctx, text)}
		}
	}
	return p, p.update(msg)
}

func (p *pane) update(msg tea.Msg) tea.Cmd {
	if p.screen.Send == nil {
		return nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.fit()
	return cmd
}

func (p *pane) focus() tea.Cmd {
	if p.screen.Send == nil {
		return nil
	}
	return p.input.Focus()
}

func (p *pane) follow() bool {
	return p.screen.Watch != nil && p.view.AtBottom()
}

func (p *pane) restyle() {
	p.input.SetStyles(textarea.DefaultStyles(p.model.dark))
	p.renderer, p.rendered = nil, nil
	p.refresh(p.follow())
}

func (p *pane) resize(width, height int) {
	follow := p.follow()
	if width = max(width, 0); width != p.width {
		p.renderer, p.rendered = nil, nil
	}
	p.width, p.height = width, max(height, 0)
	p.input.SetWidth(p.width)
	p.view.SetWidth(p.width)
	p.fit()
	p.refresh(follow)
}

func (p *pane) fit() {
	height := p.height
	if p.screen.Send != nil {
		height -= p.input.Height() + 1
	}
	p.view.SetHeight(max(height, 0))
}

func (p *pane) refresh(follow bool) {
	parts := make([]string, 0, len(p.blocks)+1)
	rendered := make(map[string]string, len(p.blocks))
	for _, block := range p.blocks {
		text, ok := p.rendered[block]
		if !ok {
			text = p.markdown(block)
		}
		rendered[block] = text
		parts = append(parts, text)
	}
	p.rendered = rendered
	switch {
	case p.err != nil:
		parts = append(parts, "error: "+p.err.Error())
	case !p.loaded:
		parts = append(parts, "loading…")
	}
	p.view.SetContent(strings.Join(parts, "\n\n"))
	if follow {
		p.view.GotoBottom()
	}
}

func (p *pane) markdown(block string) string {
	if p.renderer == nil {
		style := "light"
		if p.model.dark {
			style = "dark"
		}
		renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(p.width))
		if err != nil {
			return block
		}
		p.renderer = renderer
	}
	text, err := p.renderer.Render(block)
	if err != nil {
		return block
	}
	return strings.Trim(text, "\n")
}

func (p *pane) render() string {
	switch {
	case p.screen.Screens == nil && p.screen.Send == nil:
		return p.view.View()
	case p.screen.Screens == nil:
		return p.view.View() + "\n" + lipgloss.NewStyle().Faint(true).MaxWidth(p.width).Render(p.note) + "\n" + p.input.View()
	case !p.loaded:
		return "loading…"
	case p.err != nil:
		return "error: " + p.err.Error()
	case len(p.items) == 0:
		return "nothing to show"
	}
	first := max(p.cursor-p.height+1, 0)
	lines := make([]string, 0, p.height)
	for i, item := range p.items[first:min(first+p.height, len(p.items))] {
		line := "  " + item.Title
		if first+i == p.cursor {
			line = "> " + item.Title
		}
		lines = append(lines, lipgloss.NewStyle().MaxWidth(p.width).Render(line))
	}
	return strings.Join(lines, "\n")
}

func (p *pane) hints() string {
	hints := "↑/↓ scroll"
	switch {
	case p.screen.Screens != nil:
		hints = "↑/↓ move · enter open"
	case p.screen.Send != nil:
		hints = "enter send · alt+enter new line · pgup/pgdn scroll"
	}
	switch {
	case p.screen.Stop != nil:
		hints += " · esc stop"
	case p.back != nil:
		hints += " · esc back"
	}
	return hints
}
