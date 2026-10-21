package deck

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Arrangement of a deck as it is saved between sessions
type Layout struct {
	Order []string `json:"order"`
	Focus string   `json:"focus,omitempty"`
	Ratio float64  `json:"ratio"`
	Mode  string   `json:"mode"`
	Hints bool     `json:"hints"`
	Theme string   `json:"theme,omitempty"`
}

// Current arrangement keyed by card identity
func (m *Model) Layout() Layout {
	l := Layout{Ratio: m.ratio, Mode: m.mode.String(), Hints: m.hints, Theme: m.theme.Name}
	for _, e := range m.cards {
		l.Order = append(l.Order, keyOf(e.card))
	}
	if len(m.cards) > 0 {
		l.Focus = keyOf(m.cards[m.focus].card)
	}
	return l
}

// Reorders the cards it can find by key, cards it cannot keeping their relative order at the end, then applies mode and focus, and ratio, hints and theme unless an option fixed them
func (m *Model) Restore(l Layout) tea.Cmd {
	byKey := map[string]*entry{}
	for _, e := range m.cards {
		if _, dup := byKey[keyOf(e.card)]; !dup {
			byKey[keyOf(e.card)] = e
		}
	}
	var ordered []*entry
	used := map[*entry]bool{}
	for _, k := range l.Order {
		if e, ok := byKey[k]; ok && !used[e] {
			ordered = append(ordered, e)
			used[e] = true
		}
	}
	for _, e := range m.cards {
		if !used[e] {
			ordered = append(ordered, e)
		}
	}
	focused := (*entry)(nil)
	if len(m.cards) > 0 {
		focused = m.cards[m.focus]
	}
	m.cards = ordered
	for i, e := range m.cards {
		if e == focused {
			m.focus = i
		}
	}
	var cmds []tea.Cmd
	if l.Ratio > 0 && !m.ratioSet {
		m.ratio = clampRatio(l.Ratio)
	}
	if mode, ok := ParseMode(l.Mode); ok {
		m.mode = mode
	}
	if !m.hintsSet {
		m.hints = l.Hints
	}
	if th, ok := theme.Named(l.Theme); ok && !m.themeSet {
		m.SetTheme(th)
	}
	if e, ok := byKey[l.Focus]; ok {
		for i, c := range m.cards {
			if c == e {
				cmds = append(cmds, m.Focus(i))
			}
		}
	}
	if m.focus > 0 {
		m.stackMemo = m.focus
	}
	m.retarget()
	m.snap()
	return tea.Batch(append(cmds, m.wake())...)
}
