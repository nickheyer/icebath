package tui

import (
	"strconv"
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

// Keymap context of the hooks screen
const hooksContext = "tui.hooks"

// Longest a test or a check of a hook may take, its script allowed its full time
const hooksRunWait = 6 * time.Minute

// Keys of the hooks screen, the shared hub keys and none of its own
func registerHooksKeys(km *keys.Map) {
	if keys.ContextDocs[hooksContext] == "" {
		if err := keys.AddContext(hooksContext, "hooks screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "hooks", order: 86, title: txt.HubsTitleHooks, short: txt.HubsShortHooks, slash: "hooks",
		open: func(a *app, _ string) tea.Cmd { return a.openHooks() }, keys: registerHooksKeys})
}

// One row of the hooks screen, a shell hook or an address events are posted to
type hooksRow struct {
	hook    *tuigateway.HookRow
	webhook *tuigateway.WebhookRow
}

// State of the open hooks screen: the hooks as last read, the events a new one may be set on and the hub they show in
type hooksHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	rows    []hooksRow
	events  []string
	loading bool
}

// Hooks screen: every script that runs on an agent event with whether it may run, and every address events are posted to
func (a *app) openHooks() tea.Cmd {
	k := &hooksHub{a: a, loading: true}
	k.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleHooks), Glyph: a.toolkit.Glyphs.Terminal, Key: "tui:hooks", Context: hooksContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsHooksColEvent), Max: 28}, {Title: tr(txt.HubsColCommand), Flex: 1}, {Title: tr(txt.HubsColStatus), Max: 26}},
		Yield:   []int{1, 2},
		Rows:    k.cells,
		Detail:  k.detail,
		Empty: func() string {
			if k.loading {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsHooksEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if hook, ok := k.hookAt(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.toggle", Label: strings.ToLower(hookToggleLabel(hook))},
					wm.Hint{Action: "hub.remove"})
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return k.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return k.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return k.add() },
			"hub.toggle":  func(l *cards.List, i int) tea.Cmd { return k.toggle(l, i) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return k.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return k.load() },
	})
	k.h = a.newHub("tui:hooks", k.list)
	return a.wm.ShowOverlay(k.h)
}

// Shows the hooks the service answered, shell hooks first
func (k *hooksHub) show(r *tuigateway.HooksListResult) {
	k.loading, k.events = false, r.Events
	k.rows = k.rows[:0]
	for i := range r.Hooks {
		k.rows = append(k.rows, hooksRow{hook: &r.Hooks[i]})
	}
	for i := range r.Webhooks {
		k.rows = append(k.rows, hooksRow{webhook: &r.Webhooks[i]})
	}
	k.list.Refresh()
}

// Reads the hooks again, a failure staying in the hub
func (k *hooksHub) load() tea.Cmd {
	return hubCall(k.a, k.h, "", tuigateway.HooksList, tuigateway.HooksListParams{}, func(r *tuigateway.HooksListResult) tea.Cmd {
		k.show(r)
		return nil
	})
}

// Sends one change and shows the hooks it answers, back on the list, the note saying what was done
func (k *hooksHub) change(params tuigateway.HooksChangeParams, done string) tea.Cmd {
	back := k.h.PopTo(k.list)
	return tea.Batch(back, hubCall(k.a, k.h, tr(txt.HubsProfilesSaving), tuigateway.HooksChange, params, func(r *tuigateway.HooksListResult) tea.Cmd {
		k.show(r)
		return k.h.Done(done)
	}))
}

// Shell hook at row i, false for a webhook row and past the rows
func (k *hooksHub) hookAt(i int) (tuigateway.HookRow, bool) {
	if i < 0 || i >= len(k.rows) || k.rows[i].hook == nil {
		return tuigateway.HookRow{}, false
	}
	return *k.rows[i].hook, true
}

// Whether a hook may run, in a word
func hookState(hook tuigateway.HookRow) string {
	switch {
	case !hook.Allowed:
		return tr(txt.HubsHooksStateNotAllowed)
	case hook.Changed:
		return tr(txt.HubsHooksStateChanged)
	}
	return tr(txt.HubsHooksStateAllowed)
}

// Word for the action that lets a hook run or stops it
func hookToggleLabel(hook tuigateway.HookRow) string {
	if hook.Allowed {
		return tr(txt.HubsHooksRevoke)
	}
	return tr(txt.HubsHooksAllow)
}

// Cells of every row: the event or events, the script or address, and whether it may run
func (k *hooksHub) cells() [][]string {
	out := make([][]string, len(k.rows))
	for i, row := range k.rows {
		if row.hook != nil {
			out[i] = []string{row.hook.Event, row.hook.Command, hookState(*row.hook)}
			continue
		}
		out[i] = []string{strings.Join(row.webhook.Events, ", "), row.webhook.Label, tr(txt.HubsHooksStateWebhook)}
	}
	return out
}

// Lines of a row's detail pane
func (k *hooksHub) detail(i int) []string {
	if i < 0 || i >= len(k.rows) {
		return nil
	}
	if target := k.rows[i].webhook; target != nil {
		signed := tr(txt.HubsHooksDetailUnsigned)
		if target.Signed {
			signed = tr(txt.HubsHooksDetailSigned)
		}
		out := []string{target.Label + " · " + signed, target.URL, tr(txt.HubsHooksDetailEvents, strings.Join(target.Events, ", "))}
		if target.Matcher != "" {
			out = append(out, tr(txt.HubsHooksDetailTools, target.Matcher))
		}
		return append(out, tr(txt.HubsHooksDetailSeconds, strconv.FormatInt(target.Timeout, 10)))
	}
	hook := *k.rows[i].hook
	out := []string{hook.Event + " · " + hook.Command, hookState(hook)}
	if hook.ApprovedAt != "" {
		out = append(out, tr(txt.HubsHooksDetailAllowedAt, hook.ApprovedAt))
	}
	if hook.Changed {
		out = append(out, tr(txt.HubsHooksDetailChanged, hook.ChangedWas, hook.ChangedNow))
	}
	if hook.Matcher != "" {
		out = append(out, tr(txt.HubsHooksDetailTools, hook.Matcher))
	}
	out = append(out, tr(txt.HubsHooksDetailSeconds, strconv.FormatInt(hook.Timeout, 10)))
	if hook.FailClosed {
		out = append(out, tr(txt.HubsHooksDetailFailClosed))
	}
	return out
}

// Actions screen of a shell hook
func (k *hooksHub) open(i int) tea.Cmd {
	hook, ok := k.hookAt(i)
	if !ok {
		return nil
	}
	return k.a.hubActions(k.h, hook.Command, func() []string { return k.detail(i) }, func() []hubAction {
		toggle := hubAction{key: "hub.toggle", label: tr(txt.HubsHooksAllow), does: tr(txt.HubsHooksAllowDoes), confirm: tr(txt.HubsHooksAllowConfirm, hook.Command, hook.Event),
			run: func() tea.Cmd { return k.allow(hook) }}
		if hook.Allowed {
			toggle = hubAction{key: "hub.toggle", label: tr(txt.HubsHooksRevoke), does: tr(txt.HubsHooksRevokeDoes), run: func() tea.Cmd { return k.revoke(hook) }}
		}
		return []hubAction{toggle,
			{label: tr(txt.HubsHooksTest), does: tr(txt.HubsHooksTestDoes), run: func() tea.Cmd { return k.test(hook) }},
			{label: tr(txt.HubsHooksCheck), does: tr(txt.HubsHooksCheckDoes), run: func() tea.Cmd { return k.check(hook) }},
			{key: "hub.remove", label: tr(txt.HubsCronActRemove), does: tr(txt.HubsHooksRemoveDoes), confirm: tr(txt.HubsHooksRemoveConfirm, hook.Command),
				run: func() tea.Cmd { return k.drop(hook) }},
		}
	})
}

// Form of a new hook: the event it runs on, its script, the tools it is for and the seconds it may take
func (k *hooksHub) add() tea.Cmd {
	event := cards.Field{ID: "event", Label: tr(txt.HubsHooksColEvent), Kind: cards.FieldChoice, Required: true}
	for _, name := range k.events {
		event.Choices = append(event.Choices, cards.Choice{Value: name, Label: name})
	}
	if len(event.Choices) > 0 {
		event.Value = event.Choices[0].Value
	}
	spec := cards.FormSpec{Title: tr(txt.HubsHooksNew), Submit: tr(txt.HubsProfilesCreateSubmit), Fields: []cards.Field{event,
		{ID: "command", Label: tr(txt.HubsColCommand), Help: tr(txt.HubsHooksCommandHelp), Required: true},
		{ID: "tools", Label: tr(txt.HubsHooksFieldTools), Help: tr(txt.HubsHooksToolsHelp), More: true},
		{ID: "seconds", Label: tr(txt.HubsHooksFieldSeconds), Kind: cards.FieldNumber, More: true},
	}}
	return k.a.hubForm(k.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		seconds, _ := strconv.ParseInt(strings.TrimSpace(values["seconds"]), 10, 64)
		command := strings.TrimSpace(values["command"])
		params := tuigateway.HooksChangeParams{Verb: "add", Event: values["event"], Command: command, Matcher: strings.TrimSpace(values["tools"]), Timeout: seconds}
		return k.change(params, tr(txt.HubsFallbackAdded, command))
	})
}

// Lets a hook run
func (k *hooksHub) allow(hook tuigateway.HookRow) tea.Cmd {
	return k.change(tuigateway.HooksChangeParams{Verb: "allow", Event: hook.Event, Command: hook.Command}, tr(txt.HubsHooksAllowed, hook.Command))
}

// Stops a hook from running, its settings staying
func (k *hooksHub) revoke(hook tuigateway.HookRow) tea.Cmd {
	return k.change(tuigateway.HooksChangeParams{Verb: "revoke", Event: hook.Event, Command: hook.Command}, tr(txt.HubsHooksRevoked, hook.Command))
}

// Takes a hook out of the settings
func (k *hooksHub) drop(hook tuigateway.HookRow) tea.Cmd {
	return k.change(tuigateway.HooksChangeParams{Verb: "remove", Event: hook.Event, Command: hook.Command}, tr(txt.HubsFallbackRemoved, hook.Command))
}

// Lets the hook of a row run after a confirmation, or stops it at once
func (k *hooksHub) toggle(l *cards.List, i int) tea.Cmd {
	hook, ok := k.hookAt(i)
	switch {
	case !ok:
		return nil
	case hook.Allowed:
		return k.revoke(hook)
	}
	l.Confirm(tr(txt.HubsHooksAllowConfirm, hook.Command, hook.Event), func() tea.Cmd { return k.allow(hook) })
	return nil
}

// Takes the hook of a row out of the settings after a confirmation
func (k *hooksHub) remove(l *cards.List, i int) tea.Cmd {
	hook, ok := k.hookAt(i)
	if !ok {
		return nil
	}
	l.Confirm(tr(txt.HubsHooksRemoveConfirm, hook.Command), func() tea.Cmd { return k.drop(hook) })
	return nil
}

// Seconds as text with at most two decimals
func hookSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 2, 64)
}

// Lines telling how one run of a hook's script ended
func hookRunLines(run tuigateway.HookRun) []string {
	var out []string
	switch {
	case run.Error != "":
		out = append(out, errorLine(run.Error))
	case run.TimedOut:
		out = append(out, tr(txt.HubsHooksRunTimedOut, hookSeconds(run.Seconds)))
	case run.Exited:
		out = append(out, tr(txt.HubsHooksRunExit, strconv.FormatInt(run.ExitCode, 10), hookSeconds(run.Seconds)))
	}
	if text := strings.TrimSpace(run.Stdout); text != "" {
		out = append(out, tr(txt.HubsHooksRunOutput, text))
	}
	if text := strings.TrimSpace(run.Stderr); text != "" {
		out = append(out, tr(txt.HubsHooksRunErrors, text))
	}
	if run.Directive == "" {
		return append(out, tr(txt.HubsHooksNoAnswer))
	}
	return append(out, tr(txt.HubsHooksRunAnswer, run.Directive))
}

// Runs a hook once on a sample event and shows how it ended on a page of the hub
func (k *hooksHub) test(hook tuigateway.HookRow) tea.Cmd {
	k.h.Busy(tr(txt.HubsFallbackChecking))
	params := tuigateway.HooksTestParams{Event: hook.Event, Command: hook.Command}
	return callWithin(k.a.gw, tuigateway.HooksTest, params, hooksRunWait, func(r *tuigateway.HookRun, err error) tea.Cmd {
		if err != nil {
			return k.h.Fail(rpcErrorMessage(err))
		}
		k.h.Quiet()
		return k.a.hubLinesPage(k.h, hook.Command, hookRunLines(*r))
	})
}

// Checks a hook and shows what was found on a page of the hub
func (k *hooksHub) check(hook tuigateway.HookRow) tea.Cmd {
	k.h.Busy(tr(txt.HubsFallbackChecking))
	params := tuigateway.HooksCheckParams{Event: hook.Event, Command: hook.Command}
	return callWithin(k.a.gw, tuigateway.HooksCheck, params, hooksRunWait, func(r *tuigateway.HooksCheckResult, err error) tea.Cmd {
		if err != nil {
			return k.h.Fail(rpcErrorMessage(err))
		}
		k.h.Quiet()
		t := k.a.toolkit
		lines := []string{render.Good(tr(txt.HubsHooksCheckCanRun), t)}
		if !r.Executable {
			lines[0] = render.Bad(tr(txt.HubsHooksCheckCannotRun), t)
		}
		if r.Allowed {
			lines = append(lines, render.Good(tr(txt.HubsHooksDetailAllowedAt, r.ApprovedAt), t))
		} else {
			lines = append(lines, render.Bad(tr(txt.HubsHooksCheckNotAllowed), t))
		}
		switch {
		case r.Changed:
			lines = append(lines, render.Bad(tr(txt.HubsHooksDetailChanged, r.ChangedWas, r.ChangedNow), t))
		case r.Compared:
			lines = append(lines, render.Good(tr(txt.HubsHooksCheckUnchanged), t))
		}
		if r.Run != nil {
			switch r.Output {
			case "empty":
				lines = append(lines, render.Good(tr(txt.HubsHooksCheckWatches), t))
			case "json":
				lines = append(lines, render.Good(tr(txt.HubsHooksCheckAnswersWell), t))
			case "invalid":
				lines = append(lines, render.Bad(tr(txt.HubsHooksCheckBadAnswer, strings.TrimSpace(r.Run.Stdout)), t))
			default:
				lines = append(lines, hookRunLines(*r.Run)[0])
			}
		}
		if r.Problems > 0 {
			lines = append(lines, "", render.Bad(tr(txt.HubsHooksCheckProblems, strconv.FormatInt(r.Problems, 10)), t))
		} else {
			lines = append(lines, "", render.Good(tr(txt.HubsHooksCheckHealthy), t))
		}
		return k.a.hubLinesPage(k.h, hook.Command, lines)
	})
}
