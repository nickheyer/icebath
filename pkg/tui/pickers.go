package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the pickers and list windows of the terminal UI
const (
	modelsContext = "tui.models"
)

// Keys of the pickers and list windows, registered with the keymap of the app
func registerListKeys(km *keys.Map) {
	for name, doc := range map[string]string{modelsContext: "model picker"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.models.scope", Context: modelsContext, Label: "scope", Doc: "save the pick for every session or for this session only", Default: []string{"ctrl+g"}},
		{ID: "tui.models.logins", Context: modelsContext, Label: "logins", Doc: "open the logins of the provider of the highlighted row", Default: []string{"ctrl+d"}},
	} {
		km.Register(a)
	}
}

func init() {
	wm.HiddenText = hiddenRowsText
}

// Count of the rows a scrolled picker or table hides above and below, in the language of the UI
func hiddenRowsText(above, below int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, trimJS(tr(txt.PickersCommonMoreAbove, above)))
	}
	if below > 0 {
		parts = append(parts, trimJS(tr(txt.PickersCommonMoreBelow, below)))
	}
	return strings.Join(parts, " · ")
}

// Environment the list windows read, the toolkit palette and keymap of the app
func (a *app) cardEnv() *cards.Env {
	return &cards.Env{Theme: a.toolkit, Keys: a.keys, Ctx: a.ctx, WorkDir: a.store.sessionCwd}
}

// Clock or date of an epoch second, the clock for today
func whenText(epochSeconds float64) string {
	if epochSeconds <= 0 {
		return ""
	}
	t := time.Unix(int64(epochSeconds), 0).In(clockLocation())
	if now := time.Now().In(clockLocation()); now.YearDay() == t.YearDay() && now.Year() == t.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

// Steps of the model picker
const (
	modelStageList = iota
	modelStageEffort
)

// Reasoning levels a model pick may carry, weakest first, as the --reasoning flag spells them
var reasoningLadder = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Model picker in one palette: the models of every signed in provider and a row per provider to sign in to, then the effort of the pick
type modelPicker struct {
	a         *app
	p         *wm.Palette
	sid       string
	providers []tuigateway.ModelOptionProvider
	current   string
	// Whether a pick may be saved for every session, and whether this one is
	allowGlobal, global bool
	stage               int
	// Provider of the picked model
	slug string
	// Model picked on the list, waiting for its effort
	model string
	// Login each provider's requests go out with, by provider id
	via map[string]string
	// Why reading the logins failed, a line under the rows
	failure string
	// Provider of every row of the list by row id
	owner    map[string]string
	onSelect func(arg string) tea.Cmd
}

// Opens the model picker over every provider and model the gateway offers, typed text narrowing it
func (a *app) openModelPicker() tea.Cmd { return a.loadModelPicker(false) }

// Opens the model picker, a pick running /model with its provider, effort and scope
func (a *app) loadModelPicker(refresh bool) tea.Cmd {
	return a.pickModel(refresh, true, func(arg string) tea.Cmd { return a.slash("/model "+arg, nil, false) }, nil)
}

// Reads the providers and models of the gateway and opens the picker, a pick handed on as a /model argument and closed run once the picker left the screen
func (a *app) pickModel(refresh, allowGlobal bool, onSelect func(arg string) tea.Cmd, closed func() tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	params := tuigateway.ModelOptionsParams{IncludeUnconfigured: true, SessionID: sid, Refresh: refresh}
	a.store.ui.Status = tr(txt.PickersModelLoading)
	return ask(a.events.rpc, a.sys, tuigateway.ModelOptions, params, func(r *tuigateway.ModelOptionsResult) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if r != nil && len(r.Providers) == 0 {
			a.sys(tr(txt.PickersModelNoProviders))
		}
		if r == nil || len(r.Providers) == 0 {
			if closed != nil {
				return closed()
			}
			return nil
		}
		mp := &modelPicker{a: a, sid: sid, providers: r.Providers, current: firstText(r.Model, infoText(infoOf(a.store.ui.Info).Model)), allowGlobal: allowGlobal, onSelect: onSelect,
			via: map[string]string{}}
		mp.p = wm.NewPalette(a.toolkit, nil)
		mp.p.Context, mp.p.Notes, mp.p.OnClose = modelsContext, mp.notes, closed
		mp.p.Actions = map[string]func() tea.Cmd{"tui.models.scope": mp.toggleScope, "tui.models.logins": mp.openLogins}
		mp.show()
		for _, p := range mp.providers {
			if p.IsCurrent && mp.p.Select(p.Slug+"/"+mp.current) {
				break
			}
		}
		return tea.Batch(a.wm.ShowOverlay(mp.p), mp.loadLogins())
	})
}

// Reads which login each provider's requests use, for the via line of its rows, this session's own login first
func (mp *modelPicker) loadLogins() tea.Cmd {
	return call(mp.a.gw, tuigateway.LoginList, tuigateway.LoginListParams{SessionID: mp.sid}, func(r *tuigateway.LoginListResult, err error) tea.Cmd {
		if err != nil {
			mp.failure = rpcErrorMessage(err)
			return nil
		}
		for _, g := range r.Providers {
			name := g.Selection.Name
			if g.Selection.Rotate {
				name = tr(txt.PickersModelViaRotation)
			}
			for _, l := range g.Logins {
				if name == "" && (l.Session || l.Active) {
					name = l.Name
				}
			}
			if name != "" {
				mp.via[g.Slug] = name
			}
		}
		if l := infoOf(mp.a.store.ui.Info).Login; l != nil && l.Slug != "" {
			if l.Rotate {
				mp.via[l.Slug] = tr(txt.PickersModelViaRotation)
			} else if l.Name != "" {
				mp.via[l.Slug] = l.Name
			}
		}
		if mp.stage == modelStageList {
			lit, _ := mp.p.Lit()
			mp.p.SetItems(mp.listItems())
			mp.p.Select(lit.ID)
		}
		return nil
	})
}

// Name a provider shows under, its slug when it has none
func providerName(p tuigateway.ModelOptionProvider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Slug
}

// Provider with the slug, nil when the gateway listed none
func (mp *modelPicker) provider(slug string) *tuigateway.ModelOptionProvider {
	for i := range mp.providers {
		if mp.providers[i].Slug == slug {
			return &mp.providers[i]
		}
	}
	return nil
}

// Puts the rows, the field and the keys of the step in hand into the palette
func (mp *modelPicker) show() {
	p := mp.p
	p.SetQuery("")
	p.Busy, p.OnBack, p.Extra = false, nil, nil
	if mp.allowGlobal {
		p.Extra = []wm.Hint{{Action: "tui.models.scope"}}
	}
	if mp.stage == modelStageEffort {
		p.Placeholder, p.OnBack = tr(txt.PickersModelEffortPrompt), mp.back
		p.SetItems(mp.effortItems())
		return
	}
	p.Placeholder = tr(txt.PickersModelPrompt)
	p.Extra = append(p.Extra, wm.Hint{Action: "tui.models.logins"})
	p.SetItems(mp.listItems())
}

// Rows of the list: every model of a signed in provider with the login it goes out with, a row for each provider to add a login to, and
// a last row adding a login to any provider
func (mp *modelPicker) listItems() []wm.Item {
	mp.owner = map[string]string{}
	var items, signedOut []wm.Item
	for _, p := range mp.providers {
		slug, name := p.Slug, providerName(p)
		row := "provider:" + slug
		mp.owner[row] = slug
		switch {
		case p.Unauthenticated():
			signedOut = append(signedOut, wm.Item{ID: row, Title: "○ " + name, Detail: tr(txt.PickersModelAddLoginFor), Run: func() tea.Cmd { return mp.addLogin(slug) }})
			continue
		case len(p.Models) == 0:
			items = append(items, wm.Item{ID: row, Title: "● " + name, Detail: tr(txt.PickersModelModelStageNoneListed), Stay: true, Run: func() tea.Cmd { return nil }})
		}
		detail := name
		if login := mp.via[slug]; login != "" {
			detail = tr(txt.PickersModelVia, name, login)
		}
		for _, model := range p.Models {
			id := slug + "/" + model
			mp.owner[id] = slug
			title := model
			if model == mp.current && p.IsCurrent {
				title = "● " + model
			}
			items = append(items, wm.Item{ID: id, Title: title, Detail: detail, Stay: true, Run: func() tea.Cmd { return mp.pick(slug, model) }})
		}
	}
	items = append(items, signedOut...)
	return append(items, wm.Item{ID: "login:add", Title: "+ " + tr(txt.PickersModelAddLogin), Detail: tr(txt.PickersModelAddLoginDetail), Run: func() tea.Cmd { return mp.addLogin("") }})
}

// Adds a login on the logins screen, for the provider when one is named, the model picker opening again once it is added
func (mp *modelPicker) addLogin(slug string) tea.Cmd {
	a := mp.a
	h, _, reload := a.loginsHub("")
	again := func() tea.Cmd { return tea.Batch(wm.RequestDismissOf(h), a.openModelPicker()) }
	return tea.Batch(a.wm.ShowOverlay(h), reload(nil), a.addLogin(h, slug, slug != "", again))
}

// Opens the logins screen at the provider of the highlighted row
func (mp *modelPicker) openLogins() tea.Cmd {
	provider := ""
	if it, lit := mp.p.Lit(); lit {
		if p := mp.provider(mp.owner[it.ID]); p != nil {
			provider = providerName(*p)
		}
	}
	return mp.a.openLogins(provider)
}

// Rows of the effort step: the ladder, reasoning off, and the effort left as it is
func (mp *modelPicker) effortItems() []wm.Item {
	slug, model := mp.slug, mp.model
	row := func(title, value string) wm.Item {
		return wm.Item{ID: "effort:" + value, Title: title, Stay: true, Run: func() tea.Cmd { return mp.emit(slug, model, value) }}
	}
	var items []wm.Item
	for _, level := range reasoningLadder {
		items = append(items, row(level, level))
	}
	return append(items, row(tr(txt.PickersModelReasoningNone), "none"), row(tr(txt.PickersModelReasoningKeepCurrent), ""))
}

// Lines under the rows: the step in hand, a failure, the warning of the provider of the lit row and where the pick is saved
func (mp *modelPicker) notes() []wm.Note {
	dim := func(text string) wm.Note { return wm.Note{Text: text, Style: wm.Dim} }
	var out []wm.Note
	if mp.stage == modelStageEffort {
		out = append(out, dim(tr(txt.PickersModelEffortSubtitle, mp.model)))
	}
	if mp.failure != "" {
		out = append(out, wm.Note{Text: tr(txt.PickersCommonError, mp.failure), Style: wm.Bad})
	}
	if mp.stage == modelStageList {
		warning := ""
		if it, lit := mp.p.Lit(); lit {
			if p := mp.provider(mp.owner[it.ID]); p != nil && p.Warning != "" {
				warning = tr(txt.PickersModelProviderWarning, p.Warning)
			}
		}
		out = append(out, wm.Note{Text: warning, Style: wm.Warm})
	}
	scope := tr(txt.PickersModelPersistScopeSession)
	if mp.allowGlobal && mp.global {
		scope = tr(txt.PickersModelPersistScopeGlobal)
	}
	line := tr(txt.PickersModelPersistLabel, scope)
	if !mp.allowGlobal {
		line += tr(txt.PickersModelPersistOnlySuffix)
	}
	return append(out, dim(line))
}

// Switches between saving the pick for every session and for this session only
func (mp *modelPicker) toggleScope() tea.Cmd {
	if mp.allowGlobal {
		mp.global = !mp.global
	}
	return nil
}

// Close key on the effort step: back to the list with the cursor on the model it left
func (mp *modelPicker) back() bool {
	row := mp.slug + "/" + mp.model
	mp.stage, mp.failure = modelStageList, ""
	mp.show()
	mp.p.Select(row)
	return true
}

// Enter on a model: its effort step, or the pick at once when the catalog says the model has no reasoning control
func (mp *modelPicker) pick(slug, model string) tea.Cmd {
	if p := mp.provider(slug); p != nil {
		if caps, known := p.Capabilities[model]; known && !caps.Reasoning {
			return mp.emit(slug, model, "")
		}
	}
	mp.stage, mp.slug, mp.model = modelStageEffort, slug, model
	mp.show()
	return nil
}

// Closes the picker and hands the pick on as a /model argument: the model, its provider, the effort when one was picked and the scope
func (mp *modelPicker) emit(slug, model, effort string) tea.Cmd {
	arg := model + " --provider " + slug
	if effort != "" {
		arg += " --reasoning " + effort
	}
	if mp.allowGlobal && mp.global {
		arg += " --global"
	} else {
		arg += " " + tuiSessionModelFlag
	}
	return tea.Batch(wm.RequestDismissOf(mp.p), mp.onSelect(arg))
}

// Saves the effort that rode a model pick for every session, the shared gateway pinning it only to the session it switched
func (a *app) persistPickedEffort(sid, arg string) tea.Cmd {
	payload, err := modelPayload(arg)
	effort, _ := payload["reasoning"].(string)
	if err != nil || payload["persist"] != true || effort == "" {
		return nil
	}
	params := tuigateway.ConfigSetParams{Key: "reasoning", SessionID: sid, Value: tuigateway.TextValue(effort), Scope: "global"}
	return ask(a.events.rpc, a.sys, tuigateway.ConfigSet, params, done[tuigateway.ConfigSetResult])
}

// Drops this view's hold on a session with the close stamp once any detach of it finished, handing on whether the owner let it go, false with no failure when the view held none
func (l *sessionLifecycle) closeHeld(sid string, then func(detached bool, err error) tea.Cmd) tea.Cmd {
	return l.subs.afterDetach(sid, func() tea.Cmd {
		subscriptionID := l.subs.of(sid)
		if subscriptionID == "" {
			return then(false, nil)
		}
		flight := &detachFlight{}
		l.subs.flights[sid] = flight
		params := tuigateway.SessionDetachParams{SessionID: sid, SubscriptionID: subscriptionID, EndReason: ptr("tui_close")}
		return call(l.gw, tuigateway.CanonicalSessionDetach, params, func(result *tuigateway.SessionDetachResult, err error) tea.Cmd {
			detached := err == nil && result.Detached
			if detached {
				l.subs.release(sid, subscriptionID)
			}
			if l.subs.flights[sid] == flight {
				delete(l.subs.flights, sid)
			}
			cmds := []tea.Cmd{then(detached, err)}
			for _, waiter := range flight.waiters {
				cmds = append(cmds, waiter())
			}
			return tea.Batch(cmds...)
		})
	})
}

// Runs the sessions screen's reading of the live sessions, nothing while it is closed
func (a *app) pollSwitcher() tea.Cmd {
	if a.switcherPoll == nil {
		return nil
	}
	return a.switcherPoll()
}
