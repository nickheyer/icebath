package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the model mixes screen
const mixesContext = "tui.mixes"

// Keys of the model mixes screen, the shared hub keys and none of its own
func registerMixesKeys(km *keys.Map) {
	if keys.ContextDocs[mixesContext] == "" {
		if err := keys.AddContext(mixesContext, "model mixes screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "mixes", order: 32, title: txt.HubsTitleMixes, short: txt.HubsShortMixes, slash: "mixes",
		open: func(a *app, _ string) tea.Cmd { return a.openMixes() }, keys: registerMixesKeys})
}

// State of the open model mixes screen: the mixes as last read and the hub they show in
type mixesHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	rows    []tuigateway.MixRow
	loading bool
}

// Model mixes screen: every saved mix with the model that acts and the models that advise it
func (a *app) openMixes() tea.Cmd {
	m := &mixesHub{a: a, loading: true}
	m.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleMixes), Glyph: a.toolkit.Glyphs.Server, Key: "tui:mixes", Context: mixesContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColName), Max: 24}, {Title: tr(txt.HubsMixesColActing), Max: 44}, {Title: tr(txt.HubsMixesColAdvisers), Flex: 1}},
		Yield:   []int{3, 2},
		Rows:    m.cells,
		Detail:  m.detail,
		Status: func() string {
			if len(m.rows) == 0 {
				return ""
			}
			return tr(txt.HubsMixesLegend)
		},
		Empty: func() string {
			if m.loading {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsMixesEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if row, ok := m.at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)})
				if !row.Default {
					out = append(out, wm.Hint{Action: "hub.default"})
				}
				if len(m.rows) > 1 {
					out = append(out, wm.Hint{Action: "hub.remove"})
				}
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return m.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return m.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return m.add() },
			"hub.default": func(_ *cards.List, i int) tea.Cmd { return m.makeDefault(i) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return m.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return m.load() },
	})
	m.h = a.newHub("tui:mixes", m.list)
	return a.wm.ShowOverlay(m.h)
}

// Reads the mixes again, a failure staying in the hub
func (m *mixesHub) load() tea.Cmd {
	return hubCall(m.a, m.h, "", tuigateway.MixesList, tuigateway.MixesListParams{}, func(r *tuigateway.MixesListResult) tea.Cmd {
		m.loading, m.rows = false, r.Mixes
		m.list.Refresh()
		return nil
	})
}

// Sends one change and shows the mixes it answers, back on the list, the note saying what was done
func (m *mixesHub) change(params tuigateway.MixesChangeParams, done string) tea.Cmd {
	back := m.h.PopTo(m.list)
	return tea.Batch(back, hubCall(m.a, m.h, tr(txt.HubsProfilesSaving), tuigateway.MixesChange, params, func(r *tuigateway.MixesListResult) tea.Cmd {
		m.loading, m.rows = false, r.Mixes
		m.list.Refresh()
		return m.h.Done(done)
	}))
}

// Stores the advising models and the acting model of a mix
func (m *mixesHub) save(name string, advisers []tuigateway.MixSlot, acting tuigateway.MixSlot) tea.Cmd {
	return m.change(tuigateway.MixesChangeParams{Verb: "save", Name: name, Advisers: advisers, Acting: &acting}, tr(txt.HubsCronSaved, name))
}

// Row at i, false past the rows
func (m *mixesHub) at(i int) (tuigateway.MixRow, bool) {
	if i < 0 || i >= len(m.rows) {
		return tuigateway.MixRow{}, false
	}
	return m.rows[i], true
}

// Words for one model of a mix: its model and provider, its reasoning effort when one is set and that it is off when it is
func mixSlotText(slot tuigateway.MixSlot) string {
	text := slot.Model + " · " + slot.Provider
	if slot.ReasoningEffort != "" {
		text += " · " + slot.ReasoningEffort
	}
	if slot.Off {
		text += " (" + tr(txt.HubsSettingsOff) + ")"
	}
	return text
}

// Cells of every mix: the default marked, the model that acts and the models that advise it
func (m *mixesHub) cells() [][]string {
	out := make([][]string, len(m.rows))
	for i, row := range m.rows {
		mark := ""
		if row.Default {
			mark = "★"
		}
		advisers := make([]string, 0, len(row.Advisers))
		for _, slot := range row.Advisers {
			if !slot.Off {
				advisers = append(advisers, slot.Model)
			}
		}
		out[i] = []string{mark, row.Name, row.Acting.Model + " · " + row.Acting.Provider, strings.Join(advisers, ", ")}
	}
	return out
}

// Lines of a mix's detail pane: the model that acts, every model that advises it and what the billing looks like
func (m *mixesHub) detail(i int) []string {
	row, ok := m.at(i)
	if !ok {
		return nil
	}
	out := []string{row.Name, tr(txt.HubsMixesDetailActing, mixSlotText(row.Acting))}
	for _, slot := range row.Advisers {
		out = append(out, tr(txt.HubsMixesDetailAdviser, mixSlotText(slot)))
	}
	if row.Notice != "" {
		out = append(out, row.Notice)
	}
	return out
}

// Actions screen of a mix
func (m *mixesHub) open(i int) tea.Cmd {
	row, ok := m.at(i)
	if !ok {
		return nil
	}
	return m.a.hubActions(m.h, row.Name, func() []string { return m.detail(i) }, func() []hubAction {
		acts := []hubAction{
			{label: tr(txt.HubsMixesSetActing), does: tr(txt.HubsMixesSetActingDoes), run: func() tea.Cmd { return m.setActing(row) }},
			{label: tr(txt.HubsMixesAddAdviser), does: tr(txt.HubsMixesAddAdviserDoes), run: func() tea.Cmd { return m.addAdviser(row) }},
			{label: tr(txt.HubsMixesToggleAdviser), does: tr(txt.HubsMixesToggleAdviserDoes), run: func() tea.Cmd { return m.toggleAdviser(row) }},
		}
		if len(row.Advisers) > 1 {
			acts = append(acts, hubAction{label: tr(txt.HubsMixesRemoveAdviser), does: tr(txt.HubsMixesRemoveAdviserDoes), run: func() tea.Cmd { return m.removeAdviser(row) }})
		}
		if !row.Default {
			acts = append(acts, hubAction{key: "hub.default", label: tr(txt.HubsMixesMakeDefault), does: tr(txt.HubsMixesMakeDefaultDoes), run: func() tea.Cmd { return m.makeDefault(i) }})
		}
		if len(m.rows) > 1 {
			acts = append(acts, hubAction{key: "hub.remove", label: tr(txt.HubsCronActRemove), does: tr(txt.HubsMixesRemoveDoes), confirm: tr(txt.HubsMixesRemoveConfirm, row.Name),
				run: func() tea.Cmd {
					return m.change(tuigateway.MixesChangeParams{Verb: "remove", Name: row.Name}, tr(txt.HubsFallbackRemoved, row.Name))
				}})
		}
		return acts
	})
}

// Makes a new mix: its name, one advising model and the acting model, more advising models added from its actions
func (m *mixesHub) add() tea.Cmd {
	return m.a.hubAsk(m.h, tr(txt.HubsMixesNew), tr(txt.HubsColName), "", false, func(name string) tea.Cmd {
		if name == "" {
			return nil
		}
		return m.a.hubPickModel(m.h, tr(txt.HubsMixesPickAdviser), func(p tuigateway.ModelOptionProvider, model string) tea.Cmd {
			adviser := tuigateway.MixSlot{Provider: p.Slug, Model: model}
			return m.a.hubPickModel(m.h, tr(txt.HubsMixesPickActing), func(p tuigateway.ModelOptionProvider, model string) tea.Cmd {
				return m.save(name, []tuigateway.MixSlot{adviser}, tuigateway.MixSlot{Provider: p.Slug, Model: model})
			})
		})
	})
}

// Picks another acting model for a mix
func (m *mixesHub) setActing(row tuigateway.MixRow) tea.Cmd {
	return m.a.hubPickModel(m.h, tr(txt.HubsMixesPickActing), func(p tuigateway.ModelOptionProvider, model string) tea.Cmd {
		return m.save(row.Name, row.Advisers, tuigateway.MixSlot{Provider: p.Slug, Model: model})
	})
}

// Picks one more advising model for a mix
func (m *mixesHub) addAdviser(row tuigateway.MixRow) tea.Cmd {
	return m.a.hubPickModel(m.h, tr(txt.HubsMixesPickAdviser), func(p tuigateway.ModelOptionProvider, model string) tea.Cmd {
		return m.save(row.Name, append(slices.Clone(row.Advisers), tuigateway.MixSlot{Provider: p.Slug, Model: model}), row.Acting)
	})
}

// Picks one advising model of a mix by its place, handed on
func (m *mixesHub) pickAdviser(row tuigateway.MixRow, title string, then func(at int) tea.Cmd) tea.Cmd {
	choices := make([]hubChoice, len(row.Advisers))
	for i, slot := range row.Advisers {
		choices[i] = hubChoice{id: string(rune('a' + i)), title: mixSlotText(slot)}
	}
	return m.a.hubPick(m.h, title, choices, "", func(c hubChoice) tea.Cmd {
		return then(slices.IndexFunc(choices, func(choice hubChoice) bool { return choice.id == c.id }))
	})
}

// Turns an advising model of a mix on or off, the model staying in the mix
func (m *mixesHub) toggleAdviser(row tuigateway.MixRow) tea.Cmd {
	return m.pickAdviser(row, tr(txt.HubsMixesToggleAdviser), func(at int) tea.Cmd {
		advisers := slices.Clone(row.Advisers)
		advisers[at].Off = !advisers[at].Off
		return m.save(row.Name, advisers, row.Acting)
	})
}

// Takes an advising model out of a mix
func (m *mixesHub) removeAdviser(row tuigateway.MixRow) tea.Cmd {
	return m.pickAdviser(row, tr(txt.HubsMixesRemoveAdviser), func(at int) tea.Cmd {
		return m.save(row.Name, slices.Delete(slices.Clone(row.Advisers), at, at+1), row.Acting)
	})
}

// Makes a mix the one a run uses when it names none
func (m *mixesHub) makeDefault(i int) tea.Cmd {
	row, ok := m.at(i)
	if !ok || row.Default {
		return nil
	}
	return m.change(tuigateway.MixesChangeParams{Verb: "default", Name: row.Name}, tr(txt.HubsCronSaved, row.Name))
}

// Deletes a mix after a confirmation, the only one staying
func (m *mixesHub) remove(l *cards.List, i int) tea.Cmd {
	row, ok := m.at(i)
	if !ok || len(m.rows) < 2 {
		return nil
	}
	l.Confirm(tr(txt.HubsMixesRemoveConfirm, row.Name), func() tea.Cmd {
		return m.change(tuigateway.MixesChangeParams{Verb: "remove", Name: row.Name}, tr(txt.HubsFallbackRemoved, row.Name))
	})
	return nil
}
