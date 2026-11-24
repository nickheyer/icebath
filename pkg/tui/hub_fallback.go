package tui

import (
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the fallback models screen
const fallbackContext = "tui.fallback"

// Longest a check of the chain may take, each model answering one small request
const fallbackCheckWait = 2 * time.Minute

// Keys of the fallback models screen, the shared hub keys and none of its own
func registerFallbackKeys(km *keys.Map) {
	if keys.ContextDocs[fallbackContext] == "" {
		if err := keys.AddContext(fallbackContext, "fallback models screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "fallback", order: 31, title: txt.HubsTitleFallback, short: txt.HubsShortFallback, slash: "fallback",
		open: func(a *app, _ string) tea.Cmd { return a.openFallback() }, keys: registerFallbackKeys})
}

// State of the open fallback models screen: the chain as last read, the model it backs up and what the last check said of each entry
type fallbackHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	primary string
	rows    []tuigateway.FallbackRow
	checks  map[string]tuigateway.FallbackCheckRow
	loading bool
}

// Fallback models screen: the models tried in order when the main model fails
func (a *app) openFallback() tea.Cmd {
	f := &fallbackHub{a: a, loading: true, checks: map[string]tuigateway.FallbackCheckRow{}}
	f.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleFallback), Glyph: a.toolkit.Glyphs.Server, Key: "tui:fallback", Context: fallbackContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: "#", Right: true}, {Title: tr(txt.HubsProfilesColModel), Max: 44}, {Title: tr(txt.HubsColProvider), Max: 28}, {Title: tr(txt.HubsColStatus), Flex: 1}},
		Yield:   []int{3, 2},
		Rows:    f.cells,
		Detail:  f.detail,
		Banner: func() string {
			if f.primary == "" {
				return tr(txt.HubsFallbackBannerNone)
			}
			return tr(txt.HubsFallbackBanner, f.primary)
		},
		Empty: func() string {
			if f.loading {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsFallbackEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if _, ok := f.at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.remove"})
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return f.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return f.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return f.add() },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return f.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return f.load() },
	})
	f.h = a.newHub("tui:fallback", f.list)
	return a.wm.ShowOverlay(f.h)
}

// Shows a chain the service answered
func (f *fallbackHub) show(r *tuigateway.FallbackListResult) {
	f.loading, f.primary, f.rows = false, r.Primary, r.Entries
	f.list.Refresh()
}

// Reads the chain again, a failure staying in the hub
func (f *fallbackHub) load() tea.Cmd {
	return hubCall(f.a, f.h, "", tuigateway.FallbackList, tuigateway.FallbackListParams{}, func(r *tuigateway.FallbackListResult) tea.Cmd {
		f.show(r)
		return nil
	})
}

// Sends one change and shows the chain it answers, back on the list, the note saying what was done
func (f *fallbackHub) change(params tuigateway.FallbackChangeParams, done string) tea.Cmd {
	back := f.h.PopTo(f.list)
	return tea.Batch(back, hubCall(f.a, f.h, tr(txt.HubsProfilesSaving), tuigateway.FallbackChange, params, func(r *tuigateway.FallbackListResult) tea.Cmd {
		f.show(r)
		return f.h.Done(done)
	}))
}

// Row at i, false past the rows
func (f *fallbackHub) at(i int) (tuigateway.FallbackRow, bool) {
	if i < 0 || i >= len(f.rows) {
		return tuigateway.FallbackRow{}, false
	}
	return f.rows[i], true
}

// What the last check said of a row, false when it was not checked
func (f *fallbackHub) checked(row tuigateway.FallbackRow) (tuigateway.FallbackCheckRow, bool) {
	check, ok := f.checks[row.Provider+"\x00"+row.Model]
	return check, ok
}

// Cells of the chain: the place of each model in the order, its provider and what the last check said
func (f *fallbackHub) cells() [][]string {
	out := make([][]string, len(f.rows))
	for i, row := range f.rows {
		status := ""
		if check, ok := f.checked(row); ok {
			status = check.Status
		}
		out[i] = []string{strconv.Itoa(i + 1), row.Model, row.Provider, status}
	}
	return out
}

// Lines of an entry's detail pane: its address when it is an endpoint of the user's own, and what the last check found
func (f *fallbackHub) detail(i int) []string {
	row, ok := f.at(i)
	if !ok {
		return nil
	}
	out := []string{row.Model + " · " + row.Provider}
	if row.BaseURL != "" {
		out = append(out, tr(txt.HubsFallbackDetailAddress, row.BaseURL))
	}
	if check, ok := f.checked(row); ok {
		out = append(out, check.Status)
		if check.Error != "" {
			out = append(out, check.Error)
		}
		if check.LatencyMs > 0 {
			out = append(out, tr(txt.HubsFallbackDetailLatency, strconv.FormatInt(check.LatencyMs, 10)))
		}
	}
	return out
}

// Actions screen of one entry of the chain, with the checks of the whole chain
func (f *fallbackHub) open(i int) tea.Cmd {
	row, ok := f.at(i)
	if !ok {
		return nil
	}
	return f.a.hubActions(f.h, row.Model, func() []string { return f.detail(i) }, func() []hubAction {
		var acts []hubAction
		if i > 0 {
			acts = append(acts, hubAction{label: tr(txt.HubsFallbackEarlier), does: tr(txt.HubsFallbackEarlierDoes), run: func() tea.Cmd { return f.move(i, i-1) }})
		}
		if i < len(f.rows)-1 {
			acts = append(acts, hubAction{label: tr(txt.HubsFallbackLater), does: tr(txt.HubsFallbackLaterDoes), run: func() tea.Cmd { return f.move(i, i+1) }})
		}
		return append(acts,
			hubAction{label: tr(txt.HubsFallbackCheck), does: tr(txt.HubsFallbackCheckDoes), run: func() tea.Cmd { return f.check(false) }},
			hubAction{label: tr(txt.HubsFallbackCheckLive), does: tr(txt.HubsFallbackCheckLiveDoes), run: func() tea.Cmd { return f.check(true) }},
			hubAction{key: "hub.remove", label: tr(txt.HubsCronActRemove), does: tr(txt.HubsFallbackRemoveDoes), confirm: tr(txt.HubsFallbackRemoveConfirm, row.Model),
				run: func() tea.Cmd {
					return f.change(tuigateway.FallbackChangeParams{Verb: "remove", Index: int64(i)}, tr(txt.HubsFallbackRemoved, row.Model))
				}},
			hubAction{label: tr(txt.HubsFallbackClear), does: tr(txt.HubsFallbackClearDoes), confirm: tr(txt.HubsFallbackClearConfirm),
				run: func() tea.Cmd {
					return f.change(tuigateway.FallbackChangeParams{Verb: "clear"}, tr(txt.HubsCronSaved, tr(txt.HubsTitleFallback)))
				}})
	})
}

// Adds a model to the end of the chain, with its address when it is an endpoint of the user's own
func (f *fallbackHub) add() tea.Cmd {
	return f.a.hubPickModel(f.h, tr(txt.HubsFallbackPickProvider), func(p tuigateway.ModelOptionProvider, model string) tea.Cmd {
		params := tuigateway.FallbackChangeParams{Verb: "add", Provider: p.Slug, Model: model}
		if p.IsUserDefined {
			params.BaseURL = p.APIURL
		}
		return f.change(params, tr(txt.HubsFallbackAdded, model))
	})
}

// Puts an entry at another place of the chain
func (f *fallbackHub) move(from, to int) tea.Cmd {
	row, ok := f.at(from)
	if !ok {
		return nil
	}
	return f.change(tuigateway.FallbackChangeParams{Verb: "move", Index: int64(from), To: int64(to)}, tr(txt.HubsCronSaved, row.Model))
}

// Takes an entry out of the chain after a confirmation
func (f *fallbackHub) remove(l *cards.List, i int) tea.Cmd {
	row, ok := f.at(i)
	if !ok {
		return nil
	}
	l.Confirm(tr(txt.HubsFallbackRemoveConfirm, row.Model), func() tea.Cmd {
		return f.change(tuigateway.FallbackChangeParams{Verb: "remove", Index: int64(i)}, tr(txt.HubsFallbackRemoved, row.Model))
	})
	return nil
}

// Checks every entry of the chain, sending each one small request when live, and shows what was found beside it
func (f *fallbackHub) check(live bool) tea.Cmd {
	back := f.h.PopTo(f.list)
	f.h.Busy(tr(txt.HubsFallbackChecking))
	return tea.Batch(back, callWithin(f.a.gw, tuigateway.FallbackCheck, tuigateway.FallbackCheckParams{Live: live}, fallbackCheckWait, func(r *tuigateway.FallbackCheckResult, err error) tea.Cmd {
		if err != nil {
			return f.h.Fail(rpcErrorMessage(err))
		}
		ready := 0
		f.checks = map[string]tuigateway.FallbackCheckRow{}
		for _, row := range r.Results {
			f.checks[row.Provider+"\x00"+row.Model] = row
			if row.Ready {
				ready++
			}
		}
		f.list.Refresh()
		return f.h.Done(tr(txt.HubsFallbackChecked, ready, len(r.Results)))
	}))
}
