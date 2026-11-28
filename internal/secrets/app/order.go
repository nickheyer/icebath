package app

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Opens the order the stores are asked in: every store in its place, the one in hand moved up or down with the keys of a list whose order
// matters, and enter opening its page
func (p *page) orderPage() tea.Cmd {
	t := p.kit.Env.Theme
	var sources []*secretsv1.Source
	var view *cards.List
	// The store in hand moved by one place, up for a step of -1, and kept in hand once the order is kept
	move := func(i, step int) tea.Cmd {
		to := i + step
		if !movable(sources, i, to) {
			return nil
		}
		names := make([]string, len(sources))
		for at, source := range sources {
			names[at] = source.GetName()
		}
		names[i], names[to] = names[to], names[i]
		return p.change(func() (*secretsv1.ChangeResponse, error) {
			return p.service.Reorder(p.kit.Env.Ctx, &secretsv1.ReorderRequest{Profile: p.profile, Names: names})
		}, func() tea.Cmd {
			view.Select(to)
			return nil
		})
	}
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Store order",
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + OrderOf,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"The order the stores are asked in. The first store to give a variable wins it, and stores that fill named variables come " +
				"before the others."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			sources = p.state.GetSources()
			rows := make([][]string, len(sources))
			for i, source := range sources {
				rows[i] = []string{render.Dim(strconv.Itoa(i+1), t), source.GetLabel(), standing(source, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(sources) {
				return nil
			}
			return []string{ordinal(i+1) + " of " + strconv.Itoa(len(sources)) + "."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "No secret store is installed.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(sources) {
				return nil
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
			if movable(sources, i, i-1) {
				hints = append(hints, wm.Hint{Action: "hub.up", Label: "move up"})
			}
			if movable(sources, i, i+1) {
				hints = append(hints, wm.Hint{Action: "hub.down", Label: "move down"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(sources) {
					return nil
				}
				return p.storePage(sources[i].GetName())
			},
			"hub.up":   func(_ *cards.List, i int) tea.Cmd { return move(i, -1) },
			"hub.down": func(_ *cards.List, i int) tea.Cmd { return move(i, 1) },
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

// Whether the store at i can take the place at to: one of the same kind, as stores that fill named variables always come first
func movable(sources []*secretsv1.Source, i, to int) bool {
	return i >= 0 && i < len(sources) && to >= 0 && to < len(sources) && sources[i].GetShape() == sources[to].GetShape()
}

// Place in a list as a person says it: first, second, third, then 4th and on
func ordinal(n int) string {
	switch n {
	case 1:
		return "First"
	case 2:
		return "Second"
	case 3:
		return "Third"
	}
	return strconv.Itoa(n) + "th"
}
