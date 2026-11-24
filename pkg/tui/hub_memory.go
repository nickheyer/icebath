package tui

import (
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the memory provider screen
const memoryContext = "tui.memory"

// Longest setting a provider up may take, what it needs being installed first
const memorySetupWait = 15 * time.Minute

// Keys of the memory provider screen, the shared hub keys and none of its own
func registerMemoryKeys(km *keys.Map) {
	if keys.ContextDocs[memoryContext] == "" {
		if err := keys.AddContext(memoryContext, "memory provider screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "memory-provider", order: 33, title: txt.HubsTitleMemoryProvider, short: txt.HubsShortMemoryProvider, slash: "memory-provider",
		open: func(a *app, _ string) tea.Cmd { return a.openMemoryProvider() }, keys: registerMemoryKeys})
}

// State of the open memory provider screen: memory as last read and the hub it shows in
type memoryHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	state   tuigateway.MemoryOverviewResult
	loading bool
}

// Memory provider screen: the built-in memory first, then every outside memory store that can be set up, the one new sessions use marked
func (a *app) openMemoryProvider() tea.Cmd {
	m := &memoryHub{a: a, loading: true}
	m.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleMemoryProvider), Glyph: a.toolkit.Glyphs.Info, Key: "tui:memory", Context: memoryContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColProvider), Max: 32}, {Title: tr(txt.HubsMemoryColNeeds), Max: 28}, {Title: tr(txt.HubsColStatus), Flex: 1}},
		Yield:   []int{2, 3},
		Rows:    m.cells,
		Detail:  m.detail,
		Banner: func() string {
			if m.loading {
				return ""
			}
			return tr(txt.HubsMemoryBanner, switchWord(m.state.MemoryEnabled), switchWord(m.state.UserProfileEnabled), switchWord(m.state.ToolEnabled))
		},
		Empty: func() string { return tr(txt.HubsLoading) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{}
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}}
		},
		Free: map[string]bool{"hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return m.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return m.open(i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return m.load() },
	})
	m.h = a.newHub("tui:memory", m.list)
	return a.wm.ShowOverlay(m.h)
}

// The word on or off in the language of the screen
func switchWord(on bool) string {
	if on {
		return tr(txt.HubsSettingsOn)
	}
	return tr(txt.HubsSettingsOff)
}

// Reads memory again, a failure staying in the hub
func (m *memoryHub) load() tea.Cmd {
	return hubCall(m.a, m.h, "", tuigateway.MemoryOverview, tuigateway.MemoryOverviewParams{}, func(r *tuigateway.MemoryOverviewResult) tea.Cmd {
		m.loading, m.state = false, *r
		m.list.Refresh()
		return nil
	})
}

// Sends one change, which may take as long as installing what a provider needs, and shows memory after it, back on the list
func (m *memoryHub) change(params tuigateway.MemoryChangeParams, busy string, done func(r *tuigateway.MemoryChangeResult) string) tea.Cmd {
	back := m.h.PopTo(m.list)
	m.h.Busy(busy)
	return tea.Batch(back, callWithin(m.a.gw, tuigateway.MemoryChange, params, memorySetupWait, func(r *tuigateway.MemoryChangeResult, err error) tea.Cmd {
		if err != nil {
			return m.h.Fail(rpcErrorMessage(err))
		}
		m.loading, m.state = false, r.State
		m.list.Refresh()
		return m.h.Done(done(r))
	}))
}

// Provider at row i, the first row being the built-in memory: false for that row and past the rows
func (m *memoryHub) providerAt(i int) (tuigateway.MemoryProviderRow, bool) {
	if i < 1 || i > len(m.state.Providers) {
		return tuigateway.MemoryProviderRow{}, false
	}
	return m.state.Providers[i-1], true
}

// What a provider needs before it works, in a word or two
func memoryNeeds(p tuigateway.MemoryProviderRow) string {
	secrets, plain := false, false
	for _, f := range p.Fields {
		secrets, plain = secrets || f.Secret, plain || !f.Secret
	}
	switch {
	case p.OwnSetup:
		return tr(txt.HubsMemoryNeedsOwnSetup)
	case secrets:
		return tr(txt.HubsMemoryNeedsKey)
	case plain:
		return tr(txt.HubsMemoryNeedsSettings)
	}
	return tr(txt.HubsMemoryNeedsNothing)
}

// Whether the provider in use works, in words, empty for any other provider
func (m *memoryHub) providerState(p tuigateway.MemoryProviderRow) string {
	switch {
	case !p.Active:
		return ""
	case m.state.Ready:
		return tr(txt.HubsMemoryStateReady)
	}
	return tr(txt.HubsMemoryStateNotReady)
}

// Cells of every row: the built-in memory, then each provider, the one new sessions use marked
func (m *memoryHub) cells() [][]string {
	if m.loading {
		return nil
	}
	mark := func(on bool) string {
		if on {
			return "●"
		}
		return ""
	}
	out := [][]string{{mark(m.state.Provider == ""), tr(txt.HubsMemoryBuiltin), tr(txt.HubsMemoryNeedsNothing), ""}}
	for _, p := range m.state.Providers {
		out = append(out, []string{mark(p.Active), p.Name, memoryNeeds(p), m.providerState(p)})
	}
	return out
}

// Lines of a row's detail pane
func (m *memoryHub) detail(i int) []string {
	p, ok := m.providerAt(i)
	if !ok {
		return []string{tr(txt.HubsMemoryBuiltin), tr(txt.HubsMemoryBuiltinAbout)}
	}
	out := []string{p.Name + " · " + memoryNeeds(p)}
	if state := m.providerState(p); state != "" {
		out = append(out, state)
	}
	if p.Active && m.state.Problem != "" {
		out = append(out, m.state.Problem)
	}
	for _, f := range p.Fields {
		switch {
		case f.Secret && f.SecretSet:
			out = append(out, f.Label+": "+tr(txt.HubsMemorySecretSet))
		case f.Value != "":
			out = append(out, f.Label+": "+f.Value)
		}
	}
	return out
}

// Opens a row: the actions of the built-in memory, or the form of a provider's settings
func (m *memoryHub) open(i int) tea.Cmd {
	if p, ok := m.providerAt(i); ok {
		return m.setUp(p)
	}
	erase := func(target, label, does string) hubAction {
		return hubAction{label: label, does: does, confirm: tr(txt.HubsMemoryEraseConfirm, label), run: func() tea.Cmd {
			return m.change(tuigateway.MemoryChangeParams{Verb: "erase", Target: target}, tr(txt.HubsProfilesSaving), func(r *tuigateway.MemoryChangeResult) string {
				if len(r.Erased) == 0 {
					return tr(txt.HubsMemoryErasedNothing)
				}
				return tr(txt.HubsMemoryErased, strings.Join(r.Erased, ", "))
			})
		}}
	}
	return m.a.hubActions(m.h, tr(txt.HubsMemoryBuiltin), func() []string { return m.detail(0) }, func() []hubAction {
		var acts []hubAction
		if m.state.Provider != "" {
			acts = append(acts, hubAction{label: tr(txt.HubsMemoryUseBuiltin), does: tr(txt.HubsMemoryUseBuiltinDoes), run: func() tea.Cmd {
				return m.change(tuigateway.MemoryChangeParams{Verb: "off"}, tr(txt.HubsProfilesSaving), func(*tuigateway.MemoryChangeResult) string {
					return tr(txt.HubsCronSaved, tr(txt.HubsMemoryBuiltin))
				})
			}})
		}
		return append(acts,
			erase("memory", tr(txt.HubsMemoryEraseNotes), tr(txt.HubsMemoryEraseNotesDoes)),
			erase("user", tr(txt.HubsMemoryEraseProfile), tr(txt.HubsMemoryEraseProfileDoes)),
			erase("all", tr(txt.HubsMemoryEraseAll), tr(txt.HubsMemoryEraseAllDoes)))
	})
}

// Form of a provider's settings; sending it installs what the provider needs and makes it the one new sessions use
func (m *memoryHub) setUp(p tuigateway.MemoryProviderRow) tea.Cmd {
	fields := make([]cards.Field, 0, len(p.Fields))
	for _, f := range p.Fields {
		field := cards.Field{ID: f.Key, Label: f.Label, Value: f.Value}
		switch {
		case f.Secret:
			field.Kind, field.Help = cards.FieldSecret, f.URL
			if f.SecretSet {
				field.Placeholder = tr(txt.HubsMemorySecretKeep)
			}
		case len(f.Choices) > 0:
			field.Kind = cards.FieldChoice
			for _, choice := range f.Choices {
				field.Choices = append(field.Choices, cards.Choice{Value: choice, Label: choice})
			}
			if field.Value == "" {
				field.Value = f.Choices[0]
			}
		}
		if len(f.When) > 0 {
			conditions := make([]string, 0, len(f.When))
			for key, value := range f.When {
				conditions = append(conditions, key+" = "+value)
			}
			sort.Strings(conditions)
			field.Help = strings.TrimSpace(tr(txt.HubsMemoryOnlyWhen, strings.Join(conditions, ", ")) + " " + field.Help)
		}
		fields = append(fields, field)
	}
	intro := []string{tr(txt.HubsMemorySetUpAbout)}
	if p.OwnSetup {
		intro = []string{tr(txt.HubsMemoryOwnSetupAbout)}
	}
	spec := cards.FormSpec{Title: p.Name, Intro: intro, Submit: tr(txt.HubsMemoryUse), Fields: fields}
	return m.a.hubForm(m.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		params := tuigateway.MemoryChangeParams{Verb: "use", Provider: p.Name, Values: map[string]string{}}
		for _, f := range p.Fields {
			if value := strings.TrimSpace(values[f.Key]); value != "" {
				params.Values[f.Key] = value
			}
		}
		return m.change(params, tr(txt.HubsMemorySettingUp, p.Name), func(*tuigateway.MemoryChangeResult) string { return tr(txt.HubsMemoryInUse, p.Name) })
	})
}
