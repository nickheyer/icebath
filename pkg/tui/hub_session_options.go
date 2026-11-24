package tui

import (
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the session options screen
const sessionOptionsContext = "tui.sessionoptions"

// Keys of the session options screen, the shared hub keys and none of its own
func registerSessionOptionsKeys(km *keys.Map) {
	if keys.ContextDocs[sessionOptionsContext] == "" {
		if err := keys.AddContext(sessionOptionsContext, "session options screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "session-options", order: 15, title: txt.HubsTitleSessionOptions, short: txt.HubsShortSessionOptions,
		slash: "options", open: func(a *app, _ string) tea.Cmd { return a.openSessionOptions() }, keys: registerSessionOptionsKeys})
}

// State of the open session options screen: the session it changes, that session's info as last read and the hub it shows in
type optionsHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	sid     string
	info    *tuigateway.SessionInfo
	loading bool
}

// One row of the session options screen: its name, its value in words, what it does, the question asked first and what enter does,
// nil when the option stays as it is until a new session
type sessionOption struct {
	label, value, does, confirm string
	change                      func() tea.Cmd
}

// Session options screen: the options icebath and icebath chat take at launch, as the session open in this window runs them,
// enter changing the highlighted one for that session
func (a *app) openSessionOptions() tea.Cmd {
	o := &optionsHub{a: a, sid: a.store.ui.Sid, loading: a.store.ui.Sid != ""}
	o.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSessionOptions), Glyph: a.toolkit.Glyphs.Settings, Key: "tui:sessionoptions", Context: sessionOptionsContext,
		Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsSessionOptionsColOption), Max: 28}, {Title: tr(txt.HubsColValue), Max: 40},
			{Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:  []int{2},
		Rows:   o.cells,
		Detail: o.detail,
		Banner: func() string { return tr(txt.HubsSessionOptionsHelp) },
		Empty:  o.empty,
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{}
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelChange)}}
		},
		Free: map[string]bool{"hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return o.load() },
			"hub.use":     func(l *cards.List, i int) tea.Cmd { return o.use(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return o.load() },
	})
	o.h = a.newHub("tui:sessionoptions", o.list)
	return a.wm.ShowOverlay(o.h)
}

// Reads the session's info again, a failure staying in the hub
func (o *optionsHub) load() tea.Cmd {
	return o.reload(func(*tuigateway.SessionInfo) tea.Cmd { return nil })
}

// Reads the session's info again and hands it on, nothing read without a session
func (o *optionsHub) reload(then func(info *tuigateway.SessionInfo) tea.Cmd) tea.Cmd {
	if o.sid == "" {
		return nil
	}
	return hubCall(o.a, o.h, "", tuigateway.CanonicalSessionInfo, tuigateway.CanonicalSessionInfoParams{SessionID: o.sid}, func(info *tuigateway.SessionInfo) tea.Cmd {
		o.loading, o.info = false, info
		o.list.Refresh()
		return then(info)
	})
}

// Words of an empty screen: no session, the info still loading, or a session that has no launch options
func (o *optionsHub) empty() string {
	switch {
	case o.sid == "":
		return tr(txt.HubsSessionOptionsNoSession)
	case o.loading:
		return tr(txt.HubsLoading)
	}
	return tr(txt.HubsSessionOptionsNotLocal)
}

// Every option of the session in a fixed order, none before its info is read or when it has no launch options
func (o *optionsHub) options() []sessionOption {
	if o.info == nil || o.info.LaunchOptions == nil {
		return nil
	}
	opts := *o.info.LaunchOptions
	bypassed := opts.SafeMode || opts.IgnoreUserConfig
	toggle := func(label, does, key string, on bool) sessionOption {
		return sessionOption{label: label, value: switchWord(on), does: does, change: func() tea.Cmd {
			return o.launch(label, map[string]any{key: !on})
		}}
	}
	rules := toggle(tr(txt.HubsSessionOptionsIgnoreRules), tr(txt.HubsSessionOptionsIgnoreRulesDoes), "ignore_rules", opts.IgnoreRules)
	if opts.SafeMode {
		rules.value, rules.change = tr(txt.HubsSessionOptionsOnUntilNew), nil
	}
	skills := sessionOption{label: tr(txt.HubsSessionOptionsSkills), value: skillsText(opts.Skills), does: tr(txt.HubsSessionOptionsSkillsDoes),
		change: func() tea.Cmd { return o.skills(opts.Skills) }}
	if bypassed {
		skills.change = nil
	}
	bypass := func(label, does, confirm, key string, on bool) sessionOption {
		row := sessionOption{label: label, value: switchWord(false), does: does}
		switch {
		case on:
			row.value = tr(txt.HubsSessionOptionsOnUntilNew)
		case !bypassed:
			row.confirm, row.change = confirm, func() tea.Cmd { return o.launch(label, map[string]any{key: true}) }
		}
		return row
	}
	return []sessionOption{
		toggle(tr(txt.HubsSessionOptionsCheckpoints), tr(txt.HubsSessionOptionsCheckpointsDoes), "checkpoints", opts.Checkpoints),
		toggle(tr(txt.HubsSessionOptionsPassSessionID), tr(txt.HubsSessionOptionsPassSessionIDDoes), "pass_session_id", opts.PassSessionID),
		rules,
		{label: tr(txt.HubsSessionOptionsMaxTurns), value: turnLimitText(opts.MaxTurns), does: tr(txt.HubsSessionOptionsMaxTurnsDoes),
			change: func() tea.Cmd { return o.turnLimit(opts.MaxTurns) }},
		{label: tr(txt.HubsSessionOptionsRunBudget), value: runBudgetText(opts.RunBudget), does: tr(txt.HubsSessionOptionsRunBudgetDoes),
			change: func() tea.Cmd { return o.runBudget(opts.RunBudget) }},
		skills,
		{label: tr(txt.HubsSessionOptionsCwd), value: infoText(o.info.Cwd), does: tr(txt.HubsSessionOptionsCwdDoes), change: o.cwd},
		{label: tr(txt.HubsSessionOptionsEndpoint), value: endpointText(opts.BaseURL), does: tr(txt.HubsSessionOptionsEndpointDoes),
			change: func() tea.Cmd { return o.endpoint(opts.BaseURL) }},
		bypass(tr(txt.HubsSessionOptionsSafeMode), tr(txt.HubsSessionOptionsSafeModeDoes), tr(txt.HubsSessionOptionsSafeModeConfirm), "safe_mode", opts.SafeMode),
		bypass(tr(txt.HubsSessionOptionsNoUserConfig), tr(txt.HubsSessionOptionsNoUserConfigDoes), tr(txt.HubsSessionOptionsNoUserConfigConfirm),
			"ignore_user_config", opts.IgnoreUserConfig),
	}
}

// Cells of every option row
func (o *optionsHub) cells() [][]string {
	opts := o.options()
	out := make([][]string, len(opts))
	for i, opt := range opts {
		out[i] = []string{opt.label, opt.value, opt.does}
	}
	return out
}

// Lines of an option's detail pane: its name and value, then what it does
func (o *optionsHub) detail(i int) []string {
	opts := o.options()
	if i < 0 || i >= len(opts) {
		return nil
	}
	return []string{opts[i].label + " · " + opts[i].value, opts[i].does}
}

// Enter on an option: its question first when it has one, the hub saying so when it stays as it is
func (o *optionsHub) use(l *cards.List, i int) tea.Cmd {
	opts := o.options()
	if i < 0 || i >= len(opts) {
		return nil
	}
	opt := opts[i]
	switch {
	case opt.change == nil:
		return o.h.Fail(tr(txt.HubsSessionOptionsFixed, opt.label))
	case opt.confirm != "":
		l.Confirm(opt.confirm, opt.change)
		return nil
	}
	return opt.change()
}

// Lays launch options over the session the way a resume with those flags does
func (o *optionsHub) launch(label string, payload map[string]any) tea.Cmd {
	return o.mutate("launch", label, payload, func(*tuigateway.SessionInfo) tea.Cmd { return nil })
}

// Sends one change of the session, back on the list, then reads its info again and hands it on
func (o *optionsHub) mutate(operation, label string, payload map[string]any, then func(info *tuigateway.SessionInfo) tea.Cmd) tea.Cmd {
	sid := o.sid
	stale := func() bool { return o.a.store.ui.Sid != sid }
	back := o.h.PopTo(o.list)
	o.h.Busy(tr(txt.HubsProfilesSaving))
	return tea.Batch(back, o.a.store.mutateCanonical(o.a.gw, sid, operation, payload, stale, func(m *mutationOutcome, err error) tea.Cmd {
		switch {
		case err != nil:
			return o.h.Fail(rpcErrorMessage(err))
		case m == nil:
			o.h.Quiet()
			return nil
		}
		done := o.h.Done(tr(txt.HubsSessionOptionsApplied, label))
		return tea.Batch(done, o.reload(then))
	}))
}

// Form of the turn limit, empty for no limit
func (o *optionsHub) turnLimit(current *int64) tea.Cmd {
	label := tr(txt.HubsSessionOptionsMaxTurns)
	value := ""
	if current != nil {
		value = strconv.FormatInt(*current, 10)
	}
	parse := func(text string) (int64, bool) {
		if text = strings.TrimSpace(text); text == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(text, 10, 64)
		return n, err == nil && n > 0
	}
	check := func(text string) string {
		if _, fine := parse(text); !fine {
			return tr(txt.HubsSessionOptionsTurnsCheck)
		}
		return ""
	}
	spec := cards.FormSpec{Title: label, Intro: []string{tr(txt.HubsSessionOptionsMaxTurnsDoes)}, Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
		{ID: "turns", Label: label, Help: tr(txt.HubsSessionOptionsMaxTurnsHelp), Value: value, Check: check}}}
	return o.a.hubForm(o.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		turns, fine := parse(values["turns"])
		if !fine {
			return o.h.Fail(tr(txt.HubsSessionOptionsTurnsCheck))
		}
		return o.launch(label, map[string]any{"max_turns": turns})
	})
}

// Form of the run time limit in seconds, 0 for no limit
func (o *optionsHub) runBudget(current *float64) tea.Cmd {
	label := tr(txt.HubsSessionOptionsRunBudget)
	value := "0"
	if current != nil {
		value = strconv.FormatFloat(*current, 'f', -1, 64)
	}
	parse := func(text string) (float64, bool) {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		return seconds, err == nil && seconds >= 0 && !math.IsInf(seconds, 0)
	}
	check := func(text string) string {
		if _, fine := parse(text); !fine {
			return tr(txt.HubsSessionOptionsBudgetCheck)
		}
		return ""
	}
	spec := cards.FormSpec{Title: label, Intro: []string{tr(txt.HubsSessionOptionsRunBudgetDoes)}, Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
		{ID: "seconds", Label: label, Help: tr(txt.HubsSessionOptionsRunBudgetHelp), Kind: cards.FieldNumber, Value: value, Required: true, Check: check}}}
	return o.a.hubForm(o.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		seconds, fine := parse(values["seconds"])
		if !fine {
			return o.h.Fail(tr(txt.HubsSessionOptionsBudgetCheck))
		}
		return o.launch(label, map[string]any{"run_budget": seconds})
	})
}

// Form of the skills written into the system prompt, empty taking them all out
func (o *optionsHub) skills(current []string) tea.Cmd {
	label := tr(txt.HubsSessionOptionsSkills)
	spec := cards.FormSpec{Title: label, Intro: []string{tr(txt.HubsSessionOptionsSkillsDoes)}, Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
		{ID: "skills", Label: label, Help: tr(txt.HubsSessionOptionsSkillsHelp), Value: strings.Join(current, ", ")}}}
	return o.a.hubForm(o.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		names := []string{}
		for _, name := range skillSplitRe.Split(values["skills"], -1) {
			if name = strings.TrimFunc(name, isJSSpace); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		return o.launch(label, map[string]any{"skills": names})
	})
}

// Folder a typed path names, a relative one read from the session's folder and a leading ~ as the home folder
func optionsFolder(typed, from string) (string, error) {
	path := strings.TrimSpace(typed)
	if rest, home := strings.CutPrefix(path, "~"); home && (rest == "" || strings.HasPrefix(rest, string(filepath.Separator))) {
		dir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, rest)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(from, path)
	}
	return filepath.Clean(path), nil
}

// Form of the working folder; once the session moved, this window and the sessions it starts next follow it
func (o *optionsHub) cwd() tea.Cmd {
	label := tr(txt.HubsSessionOptionsCwd)
	current := infoText(o.info.Cwd)
	check := func(text string) string {
		path, err := optionsFolder(text, current)
		if err != nil {
			return tr(txt.HubsSessionOptionsCwdCheck)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return tr(txt.HubsSessionOptionsCwdCheck)
		}
		return ""
	}
	spec := cards.FormSpec{Title: label, Intro: []string{tr(txt.HubsSessionOptionsCwdDoes)}, Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
		{ID: "path", Label: label, Help: tr(txt.HubsSessionOptionsCwdHelp), Value: current, Required: true, Check: check}}}
	return o.a.hubForm(o.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		path, err := optionsFolder(values["path"], current)
		if err != nil {
			return o.h.Fail(err.Error())
		}
		return o.mutate("workspace", label, map[string]any{"cwd": path}, func(info *tuigateway.SessionInfo) tea.Cmd {
			moved := infoText(info.Cwd)
			if err := o.a.followCwd(moved); err != nil {
				return o.h.Fail(i18n.T(o.a.ctx, "cli.session.cwd_enter_failed", "", map[string]any{"path": moved, "error": err.Error()}))
			}
			return nil
		})
	})
}

// Form of the endpoint the session calls, an OpenAI compatible address and the key it takes
func (o *optionsHub) endpoint(current *string) tea.Cmd {
	label := tr(txt.HubsSessionOptionsEndpoint)
	check := func(text string) string {
		u, err := url.Parse(strings.TrimSpace(text))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return tr(txt.HubsSessionOptionsUrlCheck)
		}
		return ""
	}
	spec := cards.FormSpec{Title: label, Intro: []string{tr(txt.HubsSessionOptionsEndpointDoes)}, Submit: tr(txt.HubsSettingsSave), Fields: []cards.Field{
		{ID: "url", Label: tr(txt.HubsSessionOptionsEndpointURL), Value: val(current), Required: true, Check: check},
		{ID: "key", Label: tr(txt.HubsSessionOptionsEndpointKey), Help: tr(txt.HubsSessionOptionsEndpointKeyHelp), Kind: cards.FieldSecret}}}
	return o.a.hubForm(o.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		payload := map[string]any{"base_url": strings.TrimSpace(values["url"])}
		if key := strings.TrimSpace(values["key"]); key != "" {
			payload["api_key"] = key
		}
		return o.launch(label, payload)
	})
}

// Turn limit in words
func turnLimitText(turns *int64) string {
	if turns == nil {
		return tr(txt.HubsSessionOptionsNoLimit)
	}
	return strconv.FormatInt(*turns, 10)
}

// Run time limit in words
func runBudgetText(seconds *float64) string {
	if seconds == nil {
		return tr(txt.HubsSessionOptionsNoLimit)
	}
	return render.Elapsed(time.Duration(*seconds * float64(time.Second)))
}

// Launch skills in words
func skillsText(names []string) string {
	if len(names) == 0 {
		return tr(txt.HubsSessionOptionsNone)
	}
	return strings.Join(names, ", ")
}

// Endpoint in words, the profile's route when the session names none
func endpointText(baseURL *string) string {
	if text := infoText(baseURL); text != "" {
		return text
	}
	return tr(txt.HubsSessionOptionsEndpointProfile)
}
