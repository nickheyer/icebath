package tui

import (
	"errors"
	"maps"
	"slices"
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

// Keymap contexts of the pickers and list windows of the terminal UI
const (
	modelsContext        = "tui.models"
	sessionsContext      = "tui.sessions"
	sessionsDraftContext = "tui.sessions.draft"
	sessionsAllContext   = "tui.sessions.all"
	skillsContext        = "tui.skills"
	pluginsContext       = "tui.plugins"
)

// Keys of the pickers and list windows, registered with the keymap of the app
func registerListKeys(km *keys.Map) {
	for name, doc := range map[string]string{modelsContext: "model picker", sessionsContext: "session switcher", sessionsDraftContext: "session switcher, the row that starts a session from a prompt",
		sessionsAllContext: "session switcher, every row", skillsContext: "skills hub", pluginsContext: "plugins hub",
		mcpContext: "MCP servers", mcpAuthContext: "MCP server browser sign in"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.models.scope", Context: modelsContext, Label: "scope", Doc: "save the pick for every session or for this session only", Default: []string{"ctrl+g"}},
		{ID: "tui.models.disconnect", Context: modelsContext, Label: "disconnect", Doc: "remove the saved credentials of the provider of the highlighted row after a confirmation", Default: []string{"ctrl+d"}},
		{ID: "tui.sessions.open", Context: sessionsContext, Label: "open", Doc: "switch to the session", Default: []string{"enter"}},
		{ID: "tui.sessions.delete", Context: sessionsContext, Label: "delete", Doc: "delete the stored session after a confirmation", Default: []string{"d"}},
		{ID: "tui.sessions.close", Context: sessionsContext, Label: "close", Doc: "close the live session, switching to another one when it was in view", Default: []string{"ctrl+d"}},
		{ID: "tui.sessions.new", Context: sessionsAllContext, Label: "new", Doc: "start a new session beside this one", Default: []string{"ctrl+n"}},
		{ID: "tui.sessions.refresh", Context: sessionsAllContext, Label: "refresh", Doc: "read the live and the stored sessions again", Default: []string{"ctrl+r"}},
		{ID: "tui.sessions.draft.model", Context: sessionsDraftContext, Label: "model", Doc: "choose the model of the session the typed prompt starts", Default: []string{"tab"}},
		{ID: "tui.sessions.draft.up", Context: sessionsDraftContext, Label: "up", Doc: "move to the row above while no prompt is typed", Default: []string{"up"}, Quiet: true},
		{ID: "tui.sessions.draft.down", Context: sessionsDraftContext, Label: "down", Doc: "move to the row below while no prompt is typed", Default: []string{"down"}, Quiet: true},
		{ID: "tui.skills.inspect", Context: skillsContext, Label: "inspect", Doc: "show or hide what the skill does", Default: []string{"enter"}},
		{ID: "tui.skills.reinspect", Context: skillsContext, Label: "reinspect", Doc: "read what the skill does again", Default: []string{"i"}},
		{ID: "tui.skills.install", Context: skillsContext, Label: "install", Doc: "install the skill", Default: []string{"x"}},
		{ID: "tui.plugins.toggle", Context: pluginsContext, Label: "toggle", Doc: "enable or disable the plugin", Default: []string{"enter", "space"}},
		{ID: "tui.plugins.scope", Context: pluginsContext, Label: "user/all", Doc: "list the plugins the user installed or the bundled ones too", Default: []string{"tab"}},
		{ID: "tui.mcp.add", Context: mcpContext, Label: "add", Doc: "add an MCP server from the catalog", Default: []string{"a"}},
		{ID: "tui.mcp.toggle", Context: mcpContext, Label: "enable/disable", Doc: "enable or disable the MCP server", Default: []string{"space", "e"}},
		{ID: "tui.mcp.authorize", Context: mcpContext, Label: "authorize", Doc: "sign in to the MCP server in the browser", Default: []string{"o"}},
		{ID: "tui.mcp.test", Context: mcpContext, Label: "test", Doc: "connect to the MCP server and count its tools", Default: []string{"t"}},
		{ID: "tui.mcp.remove", Context: mcpContext, Label: "remove", Doc: "remove the MCP server from config.yaml after a confirmation", Default: []string{"x"}},
		{ID: "tui.mcp.auth.open", Context: mcpAuthContext, Label: "open", Doc: "open the sign in link in the browser", Default: []string{"enter"}},
		{ID: "tui.mcp.auth.stop", Context: mcpAuthContext, Label: "stop", Doc: "stop waiting and cancel the sign in", Default: []string{"esc"}},
	} {
		km.Register(a)
	}
}

func init() { wm.HiddenText = hiddenRowsText }

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
	modelStageKey
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
	// Provider of the step in hand: the one a key is typed for, the one of the picked model or the one asked about
	slug string
	// Model picked on the list, waiting for its effort
	model string
	// What the picker waits on and why the last save or disconnect failed, each a line under the rows
	waiting, failure string
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
		mp := &modelPicker{a: a, sid: sid, providers: r.Providers, current: firstText(r.Model, infoText(infoOf(a.store.ui.Info).Model)), allowGlobal: allowGlobal, onSelect: onSelect}
		mp.p = wm.NewPalette(a.toolkit, nil)
		mp.p.Context, mp.p.Notes, mp.p.OnClose = modelsContext, mp.notes, closed
		mp.p.Actions = map[string]func() tea.Cmd{"tui.models.scope": mp.toggleScope, "tui.models.disconnect": mp.askDisconnect}
		mp.show()
		for _, p := range mp.providers {
			if p.IsCurrent && mp.p.Select(p.Slug+"/"+mp.current) {
				break
			}
		}
		return a.wm.ShowOverlay(mp.p)
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
	p.Masked, p.Hold, p.Busy, p.OnText, p.OnBack, p.Extra = false, false, false, nil, nil, nil
	if mp.allowGlobal && mp.stage != modelStageKey {
		p.Extra = []wm.Hint{{Action: "tui.models.scope"}}
	}
	switch mp.stage {
	case modelStageKey:
		p.Placeholder = mp.provider(mp.slug).KeyEnv
		p.Masked, p.Hold, p.OnText, p.OnBack = true, true, mp.saveKey, mp.back
		p.SetItems(nil)
	case modelStageEffort:
		p.Placeholder, p.OnBack = tr(txt.PickersModelEffortPrompt), mp.back
		p.SetItems(mp.effortItems())
	default:
		p.Placeholder = tr(txt.PickersModelPrompt)
		p.Extra = append(p.Extra, wm.Hint{Action: "tui.models.disconnect"})
		p.SetItems(mp.listItems())
	}
}

// Rows of the list: every model of a signed in provider, and one row for a provider to sign in to or one that lists no model
func (mp *modelPicker) listItems() []wm.Item {
	mp.owner = map[string]string{}
	var items []wm.Item
	for _, p := range mp.providers {
		slug, name := p.Slug, providerName(p)
		row := "provider:" + slug
		switch {
		case p.Unauthenticated():
			suffix := tr(txt.PickersModelProviderNeedsSetup)
			if p.AuthType == "api_key" {
				suffix = tr(txt.PickersModelProviderNoKey)
			}
			mp.owner[row] = slug
			items = append(items, wm.Item{ID: row, Title: "○ " + name, Detail: suffix, Stay: true, Run: func() tea.Cmd { return mp.signIn(slug) }})
			continue
		case len(p.Models) == 0:
			mp.owner[row] = slug
			items = append(items, wm.Item{ID: row, Title: "● " + name, Detail: tr(txt.PickersModelModelStageNoneListed), Stay: true, Run: func() tea.Cmd { return nil }})
		}
		for _, model := range p.Models {
			id := slug + "/" + model
			mp.owner[id] = slug
			title := model
			if model == mp.current && p.IsCurrent {
				title = "● " + model
			}
			items = append(items, wm.Item{ID: id, Title: title, Detail: name, Stay: true, Run: func() tea.Cmd { return mp.pick(slug, model) }})
		}
	}
	return items
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

// Lines under the rows: what a question removes, the step in hand, a failure, what is awaited, the warning of the provider of the lit row and where the pick is saved
func (mp *modelPicker) notes() []wm.Note {
	dim := func(text string) wm.Note { return wm.Note{Text: text, Style: wm.Dim} }
	name := mp.slug
	if p := mp.provider(mp.slug); p != nil {
		name = providerName(*p)
	}
	if mp.p.Asking() {
		return []wm.Note{dim(tr(txt.PickersModelDisconnectRemovesCredentials, name)), dim(tr(txt.PickersModelDisconnectReauthLater))}
	}
	var out []wm.Note
	switch mp.stage {
	case modelStageKey:
		out = append(out, wm.Note{Text: tr(txt.PickersModelKeyTitle, name)}, dim(tr(txt.PickersModelKeyNote)))
	case modelStageEffort:
		out = append(out, dim(tr(txt.PickersModelEffortSubtitle, mp.model)))
	}
	if mp.failure != "" {
		out = append(out, wm.Note{Text: tr(txt.PickersCommonError, mp.failure), Style: wm.Bad})
	}
	if mp.waiting != "" {
		out = append(out, dim(mp.waiting))
	}
	if mp.stage == modelStageKey {
		return out
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

// Switches between saving the pick for every session and for this session only, on the steps that show the scope
func (mp *modelPicker) toggleScope() tea.Cmd {
	if mp.allowGlobal && mp.stage != modelStageKey {
		mp.global = !mp.global
	}
	return nil
}

// Close key on the key and the effort step: back to the list with the cursor on the row it left
func (mp *modelPicker) back() bool {
	row := "provider:" + mp.slug
	if mp.stage == modelStageEffort {
		row = mp.slug + "/" + mp.model
	}
	mp.stage, mp.failure, mp.waiting = modelStageList, "", ""
	mp.show()
	mp.p.Select(row)
	return true
}

// Enter on a provider without credentials: the key step for one that takes an API key, nothing for one the model command sets up
func (mp *modelPicker) signIn(slug string) tea.Cmd {
	if p := mp.provider(slug); p == nil || p.AuthType != "api_key" || p.KeyEnv == "" {
		return nil
	}
	mp.stage, mp.slug, mp.failure, mp.waiting = modelStageKey, slug, "", ""
	mp.show()
	return nil
}

// Saves the typed key, the step staying open with the failure when the gateway refused it and the list returning with the provider's models when it took it
func (mp *modelPicker) saveKey(text string) tea.Cmd {
	key, slug := trimJS(text), mp.slug
	if key == "" {
		return nil
	}
	mp.failure, mp.waiting, mp.p.Busy = "", tr(txt.PickersModelKeySaving), true
	params := tuigateway.ModelSaveKeyParams{Slug: slug, APIKey: key, SessionID: mp.sid}
	return call(mp.a.gw, tuigateway.ModelSaveKey, params, func(res *tuigateway.ModelSaveKeyResult, err error) tea.Cmd {
		waited := mp.stage == modelStageKey && mp.slug == slug
		if waited {
			mp.waiting, mp.p.Busy = "", false
		}
		failure := ""
		switch {
		case err != nil:
			failure = rpcErrorMessage(err)
		case res.Provider.Slug == "":
			failure = tr(txt.PickersModelFailedToSaveKey)
		}
		if failure != "" {
			if waited {
				mp.failure = failure
			}
			return nil
		}
		if p := mp.provider(res.Provider.Slug); p != nil {
			*p = res.Provider
		}
		if !waited {
			if mp.stage == modelStageList {
				mp.p.SetItems(mp.listItems())
			}
			return nil
		}
		mp.stage = modelStageList
		mp.show()
		row := "provider:" + slug
		if models := res.Provider.Models; len(models) > 0 {
			row = slug + "/" + models[0]
		}
		mp.p.Select(row)
		return nil
	})
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

// Asks before the credentials of the provider of the lit row are removed, a provider without any leaving the key alone
func (mp *modelPicker) askDisconnect() tea.Cmd {
	it, lit := mp.p.Lit()
	if mp.stage != modelStageList || !lit {
		return nil
	}
	slug := mp.owner[it.ID]
	p := mp.provider(slug)
	if p == nil || p.Unauthenticated() {
		return nil
	}
	mp.slug, mp.failure = slug, ""
	mp.p.SetQuery("")
	mp.p.Select(it.ID)
	mp.p.Ask(tr(txt.PickersModelDisconnectTitle, providerName(*p)), func() tea.Cmd { return mp.disconnect(slug) })
	return nil
}

// Removes the credentials of a provider and marks it signed out in place, its models gone and its row saying how to sign in again
func (mp *modelPicker) disconnect(slug string) tea.Cmd {
	mp.waiting, mp.p.Busy = tr(txt.PickersModelDisconnectDisconnecting), true
	params := tuigateway.ModelDisconnectParams{Slug: slug, SessionID: mp.sid}
	return call(mp.a.gw, tuigateway.ModelDisconnect, params, func(res *tuigateway.ModelDisconnectResult, err error) tea.Cmd {
		mp.waiting, mp.p.Busy = "", false
		if err != nil {
			mp.failure = rpcErrorMessage(err)
			return nil
		}
		p := mp.provider(slug)
		if !res.Disconnected || p == nil {
			return nil
		}
		signedIn := false
		p.Authenticated, p.Models, p.TotalModels = &signedIn, nil, 0
		p.Warning = tr(txt.PickersModelRunModelToConfigure)
		if p.KeyEnv != "" {
			p.Warning = tr(txt.PickersModelPasteKeyToActivate, p.KeyEnv)
		}
		if mp.stage == modelStageList {
			mp.p.SetItems(mp.listItems())
			mp.p.Select("provider:" + slug)
		}
		return nil
	})
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

// One row of the session switcher
type sessionRow struct {
	id      string
	title   string
	preview string
	status  string
	when    float64
	count   float64
	live    bool
	current bool
	// Row that starts a session from the prompt typed into it
	fresh bool
}

// Word for the state of a live session, the state as sent when the packs have no word for it
func sessionStatusLabel(status string) string {
	return trOr("pickers.session.status."+status, status)
}

// Model a draft model argument names, without its provider path, the word for the model in effect when none was picked
func draftModelLabel(arg string) string {
	parts := strings.FieldsFunc(arg, isJSSpace)
	var name []string
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "--provider" || parts[i] == "--reasoning":
			i++
		case !strings.HasPrefix(parts[i], "--"):
			name = append(name, parts[i])
		}
	}
	model := strings.Join(name, " ")
	if model == "" {
		return tr(txt.PickersSessionCurrentOrDefault)
	}
	if cut := strings.LastIndex(model, "/"); cut >= 0 && cut < len(model)-1 {
		return model[cut+1:]
	}
	return model
}

// Drops this view's hold on a session with the close stamp once any detach of it finished, handing on whether the owner let it go, false with no failure when the view held none
func (l *sessionLifecycle) closeHeld(sid string, then func(detached bool, err error) tea.Cmd) tea.Cmd {
	return l.afterDetach(sid, func() tea.Cmd {
		subscriptionID := l.subscriptions[sid]
		if subscriptionID == "" {
			return then(false, nil)
		}
		flight := &detachFlight{}
		l.detachFlights[sid] = flight
		params := tuigateway.SessionDetachParams{SessionID: sid, SubscriptionID: subscriptionID, EndReason: ptr("tui_close")}
		return call(l.gw, tuigateway.CanonicalSessionDetach, params, func(result *tuigateway.SessionDetachResult, err error) tea.Cmd {
			detached := err == nil && result.Detached
			if detached && l.subscriptions[sid] == subscriptionID {
				delete(l.subscriptions, sid)
			}
			if l.detachFlights[sid] == flight {
				delete(l.detachFlights, sid)
			}
			cmds := []tea.Cmd{then(detached, err)}
			for _, waiter := range flight.waiters {
				cmds = append(cmds, waiter())
			}
			return tea.Batch(cmds...)
		})
	})
}

// Closes a live session from the switcher: the gateway's session.close on the method table, and on the shared gateway this view's own hold dropped with
// the close stamp, the owner ending an idle session once its last viewer left
func (a *app) closeLiveView(id string, then func(closed bool, err error) tea.Cmd) tea.Cmd {
	a.store.ui.Status = tr(txt.SessionStatusClosingSession)
	settle := func(closed bool, err error) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
		}
		return then(closed, err)
	}
	if a.gw.canonical() {
		return a.life.closeHeld(id, settle)
	}
	return a.life.closeSession(id, false, func(closed *tuigateway.SessionCloseResult) tea.Cmd {
		return settle(closed != nil && closed.Closed, nil)
	})
}

// Starts a live session beside the current one and sends the prompt there, the model of the draft applied first
func (a *app) startPromptSession(prompt, modelArg string) tea.Cmd {
	trimmed := trimJS(prompt)
	if trimmed == "" {
		return nil
	}
	return a.life.newLiveSession(tr(txt.SessionLifecycleNewLiveSessionStarted), "", func(sid string) tea.Cmd {
		if sid == "" {
			a.sys(errorLine(tr(txt.SessionMainFailedToStartLiveSession)))
			return nil
		}
		destination, err := a.store.captureDestination()
		if err != nil {
			a.uncaught(err)
			return nil
		}
		destination.Sid = sid
		dispatch := func() tea.Cmd { return a.sendExpanded(trimmed, true, trimmed, submitOpts{Destination: &destination}) }
		if requested := trimJS(modelArg); requested != "" {
			return a.applyDraftModel(sid, requested, destination, "", dispatch)
		}
		return dispatch()
	})
}

// Switches a freshly started session to the model of its draft and then sends its prompt, a guarded model asking first and sending once confirmed
func (a *app) applyDraftModel(sid, requested string, destination submissionDestination, confirm string, dispatch func() tea.Cmd) tea.Cmd {
	again := func(token string) func() tea.Cmd {
		return func() tea.Cmd { return a.applyDraftModel(sid, requested, destination, token, dispatch) }
	}
	misfit := func() tea.Cmd {
		a.sys(errorLine(tr(txt.SessionMainInvalidModelSwitchResponse)))
		return nil
	}
	if !a.gw.canonical() {
		params := tuigateway.ConfigSetParams{ConfirmExpensiveModel: confirm != "", Key: "model", SessionID: sid, Value: tuigateway.TextValue(requested)}
		return ask(a.events.rpc, a.sys, tuigateway.ConfigSet, params, func(res *tuigateway.ConfigSetResult) tea.Cmd {
			if res == nil {
				return nil
			}
			if res.ConfirmRequired {
				detail := firstText(res.ConfirmMessage, res.Warning, tr(txt.SlashCmdSessionModelExpensiveDetail))
				a.store.overlay.Confirm = &confirmReq{CancelLabel: tr(txt.SlashCmdSessionModelCancel), ConfirmLabel: tr(txt.SlashCmdSessionModelSwitchAnyway),
					Danger: true, Detail: detail, OnConfirm: again("confirmed"), Title: tr(txt.SlashCmdSessionModelExpensiveTitle)}
				return nil
			}
			model := setWord(res)
			if model == "" {
				return misfit()
			}
			if a.store.isCurrentDestination(destination) {
				a.sys(tr(txt.SessionMainModelSwitched, model))
				if res.Warning != "" {
					a.sys(warningLine(res.Warning))
				}
				a.store.ui.Info = patchedInfo(a.store.ui.Info, tuigateway.SessionInfo{Model: &model})
			}
			return dispatch()
		})
	}
	never := func() bool { return false }
	return a.store.mutateCanonicalSession(a.gw, sid, "model", requested, never, confirm, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
			return nil
		}
		if m == nil || m.Result == nil {
			return misfit()
		}
		result := m.Result
		if token := result.Confirm; confirm == "" && val(result.Status) == "confirmation_required" && token != nil {
			a.store.openModelConfirmation(val(result.ConfirmMessage), firstText(val(result.TargetModel), splitArgs(requested)[0]), again(*token))
			return nil
		}
		if val(result.Model) == "" {
			return misfit()
		}
		if a.store.isCurrentDestination(destination) {
			if err := a.store.applyModelResult(result, a.sys); err != nil {
				a.sys(errorLine(rpcErrorMessage(err)))
				return nil
			}
		}
		return dispatch()
	})
}

// Runs the session switcher's reading of the live sessions, nothing while it is closed
func (a *app) pollSwitcher() tea.Cmd {
	if a.switcherPoll == nil {
		return nil
	}
	return a.switcherPoll()
}

// Session switcher: the row that starts a session from a typed prompt, the live sessions and the stored ones, with switch, close, delete, new and refresh
func (a *app) openSessionSwitcher() tea.Cmd {
	rows := []sessionRow{{fresh: true}}
	var (
		list *cards.List
		// Stored sessions as they last loaded, kept through a failed reading
		stored                                  []listedSession
		liveErr, histErr, actionErr, draftModel string
		// Session being closed
		closing string
		// Whether the cursor was put on its first row, and whether the model of the draft is being picked
		placed, picking bool
	)
	// Rebuilds the rows from the live sessions and the stored ones that are not live, the cursor staying on the session it was on
	place := func(live []listedSession) {
		anchor := ""
		if i := list.Index(); i > 0 && i < len(rows) {
			anchor = rows[i].id
		}
		current, storedSid := a.store.ui.Sid, a.store.ui.StoredSid
		next := []sessionRow{{fresh: true}}
		seen := map[string]bool{}
		add := func(sessions []listedSession, live bool) {
			for _, s := range sessions {
				if s.ID == "" || seen[s.ID] {
					continue
				}
				seen[s.ID] = true
				row := sessionRow{id: s.ID, live: live, current: s.ID == current || s.ID == storedSid || s.Current, title: s.Title, preview: s.Preview,
					status: s.Status, when: s.When, count: s.Count}
				if row.status == "" && live {
					row.status = "idle"
					if s.Running {
						row.status = "working"
					}
				}
				next = append(next, row)
			}
		}
		add(live, true)
		add(stored, false)
		sessions := next[1:]
		sort.SliceStable(sessions, func(i, j int) bool {
			if sessions[i].live != sessions[j].live {
				return sessions[i].live
			}
			return sessions[i].when > sessions[j].when
		})
		rows = next
		list.Refresh()
		if !placed {
			placed = true
			first := 0
			for i, r := range rows {
				if r.live && (first == 0 || r.current) {
					first = i
				}
				if r.live && r.current {
					break
				}
			}
			list.Select(first)
			return
		}
		for i, r := range rows {
			if anchor != "" && r.id == anchor {
				list.Select(i)
			}
		}
	}
	// Reads the live sessions and, with history, the stored ones, a failed reading of either saying so above the rows and keeping the rows it had
	load := func(history bool, done func() tea.Cmd) tea.Cmd {
		current := a.store.ui.Sid
		return requestSessions(a.gw, true, current, func(live []listedSession, err error) tea.Cmd {
			settle := func(storedRows []listedSession, storedErr error) tea.Cmd {
				liveErr = ""
				if history {
					histErr = ""
					if storedErr != nil {
						histErr = tr(txt.PickersSessionErrorsCouldNotLoadResumable)
					} else {
						stored = storedRows
					}
				}
				if err != nil {
					liveErr = rpcErrorMessage(err)
					return done()
				}
				place(live)
				return done()
			}
			if !history {
				return settle(nil, nil)
			}
			return requestSessions(a.gw, false, current, settle)
		})
	}
	repaint := func() tea.Cmd { list.Refresh(); return nil }
	shown := func() bool { return picking || a.wm.Overlay() == wm.Card(list) }
	start := func(l *cards.List, text string) tea.Cmd {
		if text == "" {
			return tea.Batch(wm.RequestDismissOf(l), a.life.newLiveSession("", "", nil))
		}
		l.SetTyped("")
		return tea.Batch(wm.RequestDismissOf(l), a.startPromptSession(text, draftModel))
	}
	list = cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSessions), Glyph: a.toolkit.Glyphs.Sessions, Key: "tui:sessions", Context: sessionsContext, Shared: []string{sessionsAllContext}, Primary: "tui.sessions.open",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColSession), Flex: 2, Max: 48}, {Title: tr(txt.HubsColLastMessage), Flex: 3}, {Title: tr(txt.HubsColMsgs), Right: true}, {Title: tr(txt.HubsColWhen), Right: true}},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				if r.fresh {
					out[i] = []string{"+", tr(txt.PickersSessionRowNew) + " · " + tr(txt.PickersSessionDraftModel, draftModelLabel(draftModel)), tr(txt.PickersSessionRowStartNew), "", ""}
					continue
				}
				mark := " "
				switch {
				case r.current:
					mark = "●"
				case r.live:
					mark = "◌"
				}
				title := r.title
				if title == "" {
					title = sessionLabel(r.id)
				}
				if r.live {
					title += " · " + sessionStatusLabel(r.status)
				}
				if r.id == closing {
					title = tr(txt.PickersSessionRowClosing)
				}
				count := ""
				if r.count > 0 {
					count = countText(r.count)
				}
				out[i] = []string{mark, title, strings.ReplaceAll(r.preview, "\n", " "), count, whenText(r.when)}
			}
			return out
		},
		Banner: func() string {
			if text := firstText(actionErr, liveErr, histErr); text != "" {
				return tr(txt.PickersCommonError, text)
			}
			return ""
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			every := []wm.Hint{{Action: "tui.sessions.new"}, {Action: "tui.sessions.refresh"}}
			switch {
			case i < 0 || i >= len(rows):
				return every
			case rows[i].fresh:
				return append([]wm.Hint{{Action: "edit.apply", Label: tr(txt.HintsLabelStart)}, {Action: "tui.sessions.draft.model"},
					{Action: "tui.sessions.draft.down", Pair: "tui.sessions.draft.up", Label: tr(txt.HintsLabelMove)}}, every...)
			case rows[i].live:
				return append([]wm.Hint{{Action: "tui.sessions.open", Label: tr(txt.HintsLabelSwitch)}, {Action: "tui.sessions.close"}}, every...)
			}
			return append([]wm.Hint{{Action: "tui.sessions.open", Label: tr(txt.HintsLabelResume)}, {Action: "tui.sessions.delete"}}, every...)
		},
		Input: &cards.ListInput{
			Row: func(i int) bool { return i >= 0 && i < len(rows) && rows[i].fresh }, Col: 2, Contexts: []string{sessionsDraftContext},
			Done:   start,
			Cancel: func(l *cards.List) tea.Cmd { return wm.RequestDismissOf(l) },
		},
		Free: map[string]bool{"tui.sessions.new": true, "tui.sessions.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.sessions.open": func(l *cards.List, i int) tea.Cmd {
				row := rows[i]
				switch {
				case row.fresh:
					return start(l, trimJS(l.Typed()))
				case row.current:
					return wm.RequestDismissOf(l)
				case row.live:
					if a.store.ui.Busy {
						a.leftRunning(a.store.ui.Sid)
					}
					return tea.Batch(wm.RequestDismissOf(l), a.life.activateLiveSession(row.id))
				case a.life.guardBusySessionSwitch(tr(txt.SessionLifecycleSwitchSessions)):
					return wm.RequestDismissOf(l)
				}
				return tea.Batch(wm.RequestDismissOf(l), a.resumeByID(row.id, nil))
			},
			"tui.sessions.new": func(l *cards.List, _ int) tea.Cmd {
				return tea.Batch(wm.RequestDismissOf(l), a.life.newLiveSession("", "", nil))
			},
			"tui.sessions.refresh": func(*cards.List, int) tea.Cmd { return load(true, repaint) },
			"tui.sessions.close": func(l *cards.List, i int) tea.Cmd {
				row := rows[i]
				if !row.live || closing != "" {
					return nil
				}
				actionErr, closing = "", row.id
				return a.closeLiveView(row.id, func(closed bool, err error) tea.Cmd {
					closing = ""
					switch {
					case err != nil:
						actionErr = rpcErrorMessage(err)
						return repaint()
					case !closed && a.gw.canonical():
						actionErr = tr(txt.PickersSessionErrorsNotAttached)
						return repaint()
					case !closed:
						actionErr = tr(txt.PickersSessionErrorsAlreadyClosed)
						return repaint()
					}
					return load(true, func() tea.Cmd {
						if !row.current {
							return repaint()
						}
						for _, r := range rows {
							if r.live && r.id != row.id {
								return tea.Batch(wm.RequestDismissOf(l), a.life.activateLiveSession(r.id))
							}
						}
						return tea.Batch(wm.RequestDismissOf(l), a.life.newLiveSession("", "", nil))
					})
				})
			},
			"tui.sessions.delete": func(l *cards.List, i int) tea.Cmd {
				row := rows[i]
				if row.fresh {
					return nil
				}
				if row.current {
					return wm.Notify(tr(txt.PickersSessionStaysInView), wm.Warn)
				}
				name := row.title
				if name == "" {
					name = sessionLabel(row.id)
				}
				l.Confirm(tr(txt.PickersSessionDeleteConfirm, name), func() tea.Cmd {
					return a.deleteStoredSession(row.id, func() tea.Cmd { return load(true, repaint) })
				})
				return nil
			},
			"tui.sessions.draft.model": func(*cards.List, int) tea.Cmd {
				picking = true
				back := func() tea.Cmd {
					return resolved(func() tea.Cmd {
						picking = false
						if a.wm.Overlay() != nil {
							return nil
						}
						return a.wm.ShowOverlay(list)
					})
				}
				return a.pickModel(false, false, func(arg string) tea.Cmd {
					draftModel = sessionScopedModelArg(arg)
					return nil
				}, back)
			},
			"tui.sessions.draft.up": func(l *cards.List, _ int) tea.Cmd {
				if trimJS(l.Typed()) == "" {
					l.Move(-1)
				}
				return nil
			},
			"tui.sessions.draft.down": func(l *cards.List, _ int) tea.Cmd {
				if trimJS(l.Typed()) == "" {
					l.Move(1)
				}
				return nil
			},
		},
	})
	a.switcherReload = func() tea.Cmd {
		if !shown() {
			a.switcherReload, a.switcherPoll = nil, nil
			return nil
		}
		return load(true, repaint)
	}
	a.switcherPoll = func() tea.Cmd {
		if !shown() {
			a.switcherReload, a.switcherPoll = nil, nil
			return nil
		}
		return load(false, repaint)
	}
	return load(true, func() tea.Cmd { return a.wm.ShowOverlay(list) })
}

// Lists what a hub shows, an owner without the hub method answering through its command worker in the pager
func hubRequest[P, R any](a *app, m tuigateway.Method[P, R], params P, command string, show func(r *R) tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	return call(a.gw, m, params, func(r *R, err error) tea.Cmd {
		var refused *rpcError
		if errors.As(err, &refused) && refused.HasCode && refused.Code == -32601 {
			return call(a.gw, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: command, SessionID: sid}, func(res *tuigateway.SlashExecResult, err error) tea.Cmd {
				if err != nil {
					a.sys(errorLine(describeSlashExecError(command, err)))
					return nil
				}
				out := res.Output
				if out == "" {
					out = tr(txt.SlashCmdCoreNoOutputFor, command)
				}
				return a.showOutput(out, command)
			})
		}
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
			return nil
		}
		return show(r)
	})
}

// Skills hub, every installed skill by category with what it does, read again on demand, a digit opening one of the first ten rows in view and an install action
func (a *app) openSkillsHub() tea.Cmd {
	return hubRequest(a, tuigateway.SkillsManage, tuigateway.SkillsManageParams{Action: "list"}, "skills", func(r *tuigateway.SkillsManageResult) tea.Cmd {
		type skillRow struct{ category, name string }
		var rows []skillRow
		for _, category := range slices.Sorted(maps.Keys(r.Skills)) {
			for _, name := range r.Skills[category] {
				rows = append(rows, skillRow{category, name})
			}
		}
		details := map[string][]string{}
		// Reads what a skill does, the detail saying it is loading until the answer or the failure stands in it
		inspect := func(l *cards.List, name string) tea.Cmd {
			delete(details, name)
			return call(a.gw, tuigateway.SkillsManage, tuigateway.SkillsManageParams{Action: "inspect", Query: name}, func(res *tuigateway.SkillsManageResult, err error) tea.Cmd {
				switch {
				case err != nil:
					details[name] = []string{tr(txt.HubsListErrorLine, rpcErrorMessage(err))}
				case res.Info == nil:
					details[name] = []string{name}
				default:
					lines := strings.Split(res.Info.Description, "\n")
					if res.Info.Path != "" {
						lines = append(lines, "", tr(txt.HubsSkillsPathLine, res.Info.Path))
					}
					details[name] = lines
				}
				l.Refresh()
				return nil
			})
		}
		// Opens the detail of a row, reading what its skill does unless it is already held
		open := func(l *cards.List, i int) tea.Cmd {
			var cmds []tea.Cmd
			if !l.DetailShown() {
				cmds = append(cmds, l.ToggleDetail())
			}
			if _, held := details[rows[i].name]; !held {
				cmds = append(cmds, inspect(l, rows[i].name))
			}
			return tea.Batch(cmds...)
		}
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: tr(txt.HubsTitleSkills), Glyph: a.toolkit.Glyphs.Tools, Key: "tui:skills", Context: skillsContext, Primary: "tui.skills.inspect",
			Columns: []render.Column{{Title: tr(txt.HubsColSkill), Flex: 3}, {Title: tr(txt.HubsColCategory), Flex: 2}},
			Rows: func() [][]string {
				out := make([][]string, len(rows))
				for i, s := range rows {
					out[i] = []string{s.name, s.category}
				}
				return out
			},
			Detail: func(i int) []string {
				if i < 0 || i >= len(rows) {
					return nil
				}
				if lines, loaded := details[rows[i].name]; loaded {
					return lines
				}
				return []string{tr(txt.HubsSkillsLoadingOne)}
			},
			Empty: func() string { return tr(txt.UserMessagesSkillsNoneInstalled) },
			Quick: open,
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.skills.inspect": func(l *cards.List, i int) tea.Cmd {
					if l.DetailShown() {
						return l.ToggleDetail()
					}
					return open(l, i)
				},
				"tui.skills.reinspect": func(l *cards.List, i int) tea.Cmd {
					delete(details, rows[i].name)
					return open(l, i)
				},
				"tui.skills.install": func(l *cards.List, i int) tea.Cmd {
					name := rows[i].name
					return ask(a.events.rpc, a.sys, tuigateway.SkillsManage, tuigateway.SkillsManageParams{Action: "install", Query: name}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
						if res == nil {
							return nil
						}
						return tea.Batch(wm.Notify(tr(txt.SlashCmdOpsSkillsInstalled, name), wm.Success), a.events.loadSlashCatalog(a.store.ui.Sid))
					})
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}

// Plugins hub, the plugins the user installed or every plugin with its state, toggled in place by its key or by the digit of one of the first ten rows in view
func (a *app) openPluginsHub() tea.Cmd {
	return hubRequest(a, tuigateway.PluginsManage, tuigateway.PluginsManageParams{Action: "list"}, "plugins", func(r *tuigateway.PluginsManageResult) tea.Cmd {
		plugins := r.Plugins
		userCount, bundledCount := val(r.UserCount), val(r.BundledCount)
		all := false
		// Plugins the scope lists as indexes into every plugin, the bundled ones left out of the user scope unless the user installed none
		listed := func() ([]int, bool) {
			var own, every []int
			for i, p := range plugins {
				every = append(every, i)
				if p.Source != "bundled" {
					own = append(own, i)
				}
			}
			if all || len(own) == 0 {
				return every, true
			}
			return own, false
		}
		toggle := func(l *cards.List, i int) tea.Cmd {
			shown, _ := listed()
			at := shown[i]
			p := plugins[at]
			params := tuigateway.PluginsManageParams{Action: "toggle", Enable: ptr(!p.Enabled()), Key: p.Key, Name: p.Name}
			return ask(a.events.rpc, a.sys, tuigateway.PluginsManage, params, func(res *tuigateway.PluginsManageResult) tea.Cmd {
				switch {
				case res != nil && res.Plugin != nil:
					plugins[at] = *res.Plugin
				case res != nil && p.Enabled():
					plugins[at].Status = "disabled"
				case res != nil:
					plugins[at].Status = "enabled"
				}
				l.Refresh()
				if res == nil {
					return nil
				}
				// The widgets of a plugin come and go with it, so the scan that announces them runs again
				return a.widgets.rescan(nil)
			})
		}
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: tr(txt.HubsTitlePlugins), Glyph: a.toolkit.Glyphs.Ext, Key: "tui:plugins", Context: pluginsContext, Primary: "tui.plugins.toggle",
			Columns: []render.Column{{Title: tr(txt.HubsColOn)}, {Title: tr(txt.HubsColPlugin), Max: 32}, {Title: tr(txt.HubsColSource)}, {Title: tr(txt.HubsColWhatItAdds), Flex: 1}},
			Rows: func() [][]string {
				shown, _ := listed()
				out := make([][]string, len(shown))
				for i, at := range shown {
					p := plugins[at]
					mark := "○"
					if p.Enabled() {
						mark = "●"
					}
					out[i] = []string{mark, p.Name, p.Source, strings.ReplaceAll(p.Description, "\n", " ")}
				}
				return out
			},
			Status: func() string {
				switch _, every := listed(); {
				case len(plugins) == 0:
					return ""
				case every:
					return tr(txt.HubsPluginsAllScope, len(plugins))
				case bundledCount > 0:
					return tr(txt.HubsPluginsUserScope, userCount) + tr(txt.HubsPluginsBundledSuffix, bundledCount)
				}
				return tr(txt.HubsPluginsUserScope, userCount)
			},
			Empty: func() string { return tr(txt.HubsPluginsEmpty) + "\n" + tr(txt.HubsPluginsInstallHint) },
			Quick: toggle,
			Free:  map[string]bool{"tui.plugins.scope": true},
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.plugins.toggle": toggle,
				"tui.plugins.scope": func(l *cards.List, _ int) tea.Cmd {
					all = !all
					l.Refresh()
					l.Move(-len(plugins))
					return nil
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}
