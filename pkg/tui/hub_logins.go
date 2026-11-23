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

// Keymap contexts of the logins screen and of a sign in in progress
const (
	loginsContext = "tui.logins"
	signInContext = "tui.logins.signin"
)

// Keys of the logins screen beyond the shared hub keys
func registerLoginsKeys(km *keys.Map) {
	for name, doc := range map[string]string{loginsContext: "logins screen", signInContext: "logins screen, a sign in in progress"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.logins.convert", Context: loginsContext, Label: "keep in icebath", Doc: "move a key read from an environment variable into icebath's own store", Default: []string{"c"}},
		{ID: "tui.signin.open", Context: signInContext, Label: "open link", Doc: "open the sign in link in the browser of this computer", Default: []string{"o"}},
		{ID: "tui.signin.copy", Context: signInContext, Label: "copy link", Doc: "copy the sign in link", Default: []string{"c"}},
		{ID: "tui.signin.code", Context: signInContext, Label: "copy code", Doc: "copy the code to type on the sign in page", Default: []string{"y"}},
	} {
		km.Register(a)
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "logins", order: 20, title: txt.HubsTitleLogins, short: txt.HubsShortLogins, slash: "logins", usage: "[provider]", open: func(a *app, provider string) tea.Cmd { return a.openLogins(provider) }, keys: registerLoginsKeys})
}

// One row of the logins screen: a provider heading its logins, or one login of it
type loginEntry struct {
	group tuigateway.LoginProviderGroup
	// Login of the row, nil for the provider's heading
	login *tuigateway.LoginRow
}

// Logins screen: every provider's logins under its name, marking the ones this session uses, the defaults and the rotation sets, narrowed to a
// provider when one is named
func (a *app) openLogins(provider string) tea.Cmd {
	h, list, reload := a.loginsHub(provider)
	return tea.Batch(a.wm.ShowOverlay(h), reload(func() {
		if provider == "" {
			return
		}
		list.Filter(provider)
	}))
}

// Logins hub with its list and the read that fills it, then runs placed once the first answer is in
func (a *app) loginsHub(provider string) (*cards.Hub, *cards.List, func(placed func()) tea.Cmd) {
	sid := a.store.ui.Sid
	var (
		entries []loginEntry
		session *tuigateway.LoginListSession
		h       *cards.Hub
		list    *cards.List
		loaded  bool
	)
	reload := func(placed func()) tea.Cmd {
		return call(a.gw, tuigateway.LoginList, tuigateway.LoginListParams{SessionID: sid}, func(r *tuigateway.LoginListResult, err error) tea.Cmd {
			loaded = true
			if err != nil {
				list.Refresh()
				return h.Fail(rpcErrorMessage(err))
			}
			anchor := ""
			if i := list.Index(); i >= 0 && i < len(entries) && entries[i].login != nil {
				anchor = entries[i].login.ID
			}
			entries, session = nil, r.Session
			for _, g := range r.Providers {
				entries = append(entries, loginEntry{group: g})
				for i := range g.Logins {
					entries = append(entries, loginEntry{group: g, login: &g.Logins[i]})
				}
			}
			list.Refresh()
			for i, e := range entries {
				if e.login != nil && e.login.ID == anchor {
					list.Select(i)
				}
			}
			if placed != nil {
				placed()
			}
			return nil
		})
	}
	again := func() tea.Cmd { return reload(nil) }
	at := func(i int) (loginEntry, bool) {
		if i < 0 || i >= len(entries) {
			return loginEntry{}, false
		}
		return entries[i], true
	}
	loginAt := func(i int) (loginEntry, bool) {
		e, ok := at(i)
		return e, ok && e.login != nil
	}
	use := func(_ *cards.List, i int) tea.Cmd {
		e, ok := at(i)
		switch {
		case !ok:
			return nil
		case e.login == nil:
			return a.addLogin(h, e.group.Slug, true, again)
		case sid == "":
			return h.Fail(tr(txt.HubsLoginsNoSession))
		}
		l := *e.login
		choice := l.ID
		if l.Session {
			choice = ""
		}
		params := tuigateway.SessionSetLoginParams{SessionID: sid, Provider: e.group.Provider, Login: &choice}
		return hubCall(a, h, tr(txt.HubsLoginsPinning), tuigateway.SessionSetLogin, params, func(r *tuigateway.SessionSetLoginResult) tea.Cmd {
			var done string
			switch {
			case r.Cleared:
				done = tr(txt.HubsLoginsFollowDefault, e.group.Name)
			case r.Current:
				done = tr(txt.HubsLoginsUsed, loginLabel(l), e.group.Name)
			default:
				done = tr(txt.HubsLoginsPinnedLater, loginLabel(l), e.group.Name)
			}
			if r.Warning != "" {
				return tea.Batch(h.Fail(r.Warning), again())
			}
			return tea.Batch(h.Done(done), again())
		})
	}
	edit := func(_ *cards.List, i int) tea.Cmd {
		e, ok := at(i)
		if !ok {
			return nil
		}
		if e.login == nil {
			return a.editLoginProvider(h, e.group, again)
		}
		l := *e.login
		if l.AuthType == "oauth" {
			return a.signIn(h, e.group.Provider, e.group.Name, l.Name, again)
		}
		if l.Origin == "linked" {
			return h.Fail(tr(txt.HubsLoginsLinkedEdit, loginOrigin(l)))
		}
		return a.addLoginKey(h, e.group.Provider, e.group.Name, l.Name, again)
	}
	t := a.toolkit
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleLogins), Glyph: t.Glyphs.Ext, Key: "tui:logins", Context: loginsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColLogin), Max: 30}, {Title: tr(txt.HubsColKind)}, {Title: tr(txt.HubsColFrom), Max: 24},
			{Title: tr(txt.HubsColAccount), Flex: 1}, {Title: tr(txt.HubsColStatus)}},
		Yield: []int{3, 2, 4},
		Rows: func() [][]string {
			out := make([][]string, len(entries))
			for i, e := range entries {
				if e.login == nil {
					out[i] = []string{"", render.Heading(e.group.Name, t), "", "", render.Dim(loginGroupSummary(e.group), t), ""}
					continue
				}
				l := *e.login
				out[i] = []string{loginMarks(l), "  " + l.Name, loginKind(l), loginOrigin(l), l.Account, loginStatus(l)}
			}
			return out
		},
		Detail: func(i int) []string {
			e, ok := at(i)
			if !ok {
				return nil
			}
			return loginDetail(e)
		},
		Help: func(i int) []string {
			e, ok := at(i)
			if !ok {
				return []string{tr(txt.HubsShortLogins)}
			}
			return append([]string{tr(txt.HubsLoginsHelp), ""}, loginDetail(e)...)
		},
		Banner: func() string {
			if session != nil && session.Login != nil && session.Login.Missing != "" {
				return tr(txt.HubsLoginsMissing, session.Login.Missing)
			}
			return ""
		},
		Status: func() string { return tr(txt.HubsLoginsLegend) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			e, ok := at(i)
			switch {
			case !ok:
				return []wm.Hint{{Action: "hub.add"}}
			case e.login == nil:
				return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelAdd)}, {Action: "hub.edit", Label: tr(txt.HintsLabelChange)}}
			}
			out := []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelUse)}, {Action: "hub.default"}, {Action: "hub.toggle", Label: tr(txt.HintsLabelRotation)}}
			if e.login.AuthType == "oauth" {
				out = append(out, wm.Hint{Action: "hub.edit", Label: tr(txt.HintsLabelSignInAgain)})
			}
			if e.login.Origin == "linked" && strings.HasPrefix(e.login.Source, "env:") {
				out = append(out, wm.Hint{Action: "tui.logins.convert"})
			}
			return append(out, wm.Hint{Action: "hub.rename"}, wm.Hint{Action: "hub.remove"}, wm.Hint{Action: "hub.add"})
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsLoginsEmpty, a.keys.Display("hub.add"))
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":  use,
			"hub.edit": edit,
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				return again()
			},
			"hub.default": func(_ *cards.List, i int) tea.Cmd {
				e, ok := loginAt(i)
				if !ok {
					return nil
				}
				params := tuigateway.LoginSetDefaultParams{Provider: e.group.Provider, Login: ptr(e.login.ID)}
				return hubCall(a, h, "", tuigateway.LoginSetDefault, params, func(*tuigateway.LoginSetDefaultResult) tea.Cmd {
					return tea.Batch(h.Done(tr(txt.HubsLoginsDefaultSet, loginLabel(*e.login), e.group.Name)), again())
				})
			},
			"hub.toggle": func(_ *cards.List, i int) tea.Cmd {
				e, ok := loginAt(i)
				if !ok {
					return nil
				}
				params := tuigateway.LoginSetRotationParams{Provider: e.group.Provider, Login: e.login.ID, Rotate: !e.login.Rotate}
				return hubCall(a, h, "", tuigateway.LoginSetRotation, params, func(*tuigateway.LoginResult) tea.Cmd {
					done := tr(txt.HubsLoginsRotationOn, loginLabel(*e.login))
					if e.login.Rotate {
						done = tr(txt.HubsLoginsRotationOff, loginLabel(*e.login))
					}
					return tea.Batch(h.Done(done), again())
				})
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd {
				e, ok := loginAt(i)
				if !ok {
					return nil
				}
				l := *e.login
				return a.hubAsk(h, tr(txt.HubsLoginsRenameTitle, loginLabel(l)), tr(txt.HubsLoginsNameLabel), l.Name, false, func(name string) tea.Cmd {
					if name == l.Name {
						return nil
					}
					params := tuigateway.LoginRenameParams{Provider: e.group.Provider, Login: l.ID, Name: name}
					return hubCall(a, h, "", tuigateway.LoginRename, params, func(*tuigateway.LoginResult) tea.Cmd {
						return tea.Batch(h.Done(tr(txt.HubsLoginsRenamed, name)), again())
					})
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				e, ok := loginAt(i)
				if !ok {
					return nil
				}
				login := *e.login
				question := tr(txt.HubsLoginsRemoveConfirm, loginLabel(login), e.group.Name)
				if login.Origin == "linked" {
					question = tr(txt.HubsLoginsRemoveLinkedConfirm, loginLabel(login), loginOrigin(login))
				}
				l.Confirm(question, func() tea.Cmd {
					params := tuigateway.LoginRemoveParams{Provider: e.group.Provider, Login: login.ID}
					return hubCall(a, h, "", tuigateway.LoginRemove, params, func(r *tuigateway.LoginRemoveResult) tea.Cmd {
						done := tr(txt.HubsLoginsRemoved, loginLabel(login))
						if len(r.Hints) > 0 {
							done += " " + strings.Join(r.Hints, " ")
						}
						return tea.Batch(h.Done(done), again())
					})
				})
				return nil
			},
			"tui.logins.convert": func(l *cards.List, i int) tea.Cmd {
				e, ok := loginAt(i)
				if !ok || e.login.Origin != "linked" || !strings.HasPrefix(e.login.Source, "env:") {
					return nil
				}
				login := *e.login
				l.Confirm(tr(txt.HubsLoginsConvertConfirm, strings.TrimPrefix(login.Source, "env:")), func() tea.Cmd {
					params := tuigateway.LoginConvertParams{Provider: e.group.Provider, Login: login.ID}
					return hubCall(a, h, "", tuigateway.LoginConvert, params, func(*tuigateway.LoginResult) tea.Cmd {
						return tea.Batch(h.Done(tr(txt.HubsLoginsConverted, loginLabel(login))), again())
					})
				})
				return nil
			},
			"hub.add": func(_ *cards.List, i int) tea.Cmd {
				e, _ := at(i)
				return a.addLogin(h, e.group.Slug, false, again)
			},
		},
	})
	h = a.newHub("tui:logins", list)
	return h, list, reload
}

// One line summing up a provider's choices: its default, how its rotation picks and what happens when a pinned login runs out
func loginGroupSummary(g tuigateway.LoginProviderGroup) string {
	var parts []string
	switch {
	case g.Default == "@rotate":
		parts = append(parts, tr(txt.HubsLoginsSummaryRotation, g.Strategy))
	case g.Default != "":
		for _, l := range g.Logins {
			if l.ID == g.Default {
				parts = append(parts, tr(txt.HubsLoginsSummaryDefault, l.Name))
			}
		}
	}
	if g.OnExhausted == "rotate" {
		parts = append(parts, tr(txt.HubsLoginsSummaryRotateOnExhausted))
	}
	return strings.Join(parts, " · ")
}

// Marks of a login: in use for this session, the provider's default, in rotation
func loginMarks(l tuigateway.LoginRow) string {
	mark := ""
	if l.Session || l.Active {
		mark += "●"
	}
	if l.Default {
		mark += "★"
	}
	if l.Rotate {
		mark += "↻"
	}
	return mark
}

// Name of a login with its label when the label says something more
func loginLabel(l tuigateway.LoginRow) string {
	if l.Label != "" && l.Label != l.Name {
		return l.Name + " (" + l.Label + ")"
	}
	return l.Name
}

// Kind of a login in plain words
func loginKind(l tuigateway.LoginRow) string {
	switch l.AuthType {
	case "api_key":
		return tr(txt.HubsLoginsKindKey)
	case "oauth":
		return tr(txt.HubsLoginsKindSubscription)
	}
	return l.AuthType
}

// Where a login's secret lives, in plain words
func loginOrigin(l tuigateway.LoginRow) string {
	switch src := l.Source; {
	case strings.HasPrefix(src, "manual"):
		return "icebath"
	case strings.HasPrefix(src, "env:"):
		return ".env " + strings.TrimPrefix(src, "env:")
	case src == "claude_code":
		return "Claude Code"
	case src == "qwen-cli":
		return "Qwen CLI"
	}
	return l.Source
}

// Status of a login in plain words with when a limit lifts
func loginStatus(l tuigateway.LoginRow) string {
	s := string(l.Status)
	switch l.Status {
	case "ready":
		s = tr(txt.HubsLoginsStatusReady)
	case "limited":
		s = tr(txt.HubsLoginsStatusLimited)
	case "needs_sign_in":
		s = tr(txt.HubsLoginsStatusNeedsSignIn)
	case "dead":
		s = tr(txt.HubsLoginsStatusDead)
	case "unavailable":
		s = tr(txt.HubsLoginsStatusUnavailable)
	}
	if l.ResetsAt != nil && *l.ResetsAt != "" {
		if at, err := time.Parse(time.RFC3339, *l.ResetsAt); err == nil {
			s += " " + tr(txt.HubsLoginsUntil, at.Local().Format("15:04"))
		}
	}
	return s
}

// Lines of a row's detail pane: a provider's choices, or a login's source, account, usage and status
func loginDetail(e loginEntry) []string {
	g := e.group
	if e.login == nil {
		out := []string{g.Name + " · " + g.Provider}
		if s := loginGroupSummary(g); s != "" {
			out = append(out, s)
		}
		out = append(out, tr(txt.HubsLoginsDetailCount, len(g.Logins)))
		if name := g.Selection.Name; name != "" {
			out = append(out, tr(txt.HubsLoginsDetailSelection, name))
		}
		return out
	}
	l := *e.login
	out := []string{loginLabel(l) + " · " + g.Name, tr(txt.HubsLoginsDetailFrom, loginOrigin(l)), loginStatus(l)}
	if l.Account != "" {
		out = append(out, tr(txt.HubsLoginsDetailAccount, l.Account))
	}
	if l.Fingerprint != "" {
		out = append(out, tr(txt.HubsLoginsDetailFingerprint, l.Fingerprint))
	}
	if l.Reason != "" {
		out = append(out, l.Reason)
	}
	if l.SharedWith != "" {
		out = append(out, tr(txt.HubsLoginsDetailShared, l.SharedWith))
	}
	if len(l.Usage) > 0 {
		out = append(out, "", tr(txt.HubsLoginsDetailUsage))
		for _, u := range l.Usage {
			out = append(out, "  "+usageWindowText(u))
		}
	}
	return out
}

// One usage window of a login as a line
func usageWindowText(u tuigateway.ProviderUsageWindow) string {
	b := strings.Builder{}
	b.WriteString(u.Label)
	b.WriteString(" " + strconv.Itoa(int(u.UsedPercent+0.5)) + "%")
	if u.ResetsAt != nil && *u.ResetsAt != "" {
		if at, err := time.Parse(time.RFC3339, *u.ResetsAt); err == nil {
			b.WriteString(" · " + tr(txt.HubsLoginsUntil, at.Local().Format("Jan 2 15:04")))
		}
	}
	return b.String()
}

// Changes a provider's choices: its default login or the rotation, how the rotation picks, and what a pinned login does when it runs out
func (a *app) editLoginProvider(h *cards.Hub, g tuigateway.LoginProviderGroup, done func() tea.Cmd) tea.Cmd {
	defaults := []cards.Choice{{Value: "", Label: tr(txt.HubsLoginsDefaultNone), Meaning: tr(txt.HubsLoginsDefaultNoneMeaning)},
		{Value: "@rotate", Label: tr(txt.HubsLoginsDefaultRotate), Meaning: tr(txt.HubsLoginsDefaultRotateMeaning)}}
	for _, l := range g.Logins {
		defaults = append(defaults, cards.Choice{Value: l.ID, Label: loginLabel(l), Meaning: loginKind(l) + " · " + loginOrigin(l)})
	}
	spec := cards.FormSpec{Title: g.Name, Submit: tr(txt.HubsLoginsSave), Fields: []cards.Field{
		{ID: "default", Label: tr(txt.HubsLoginsFieldDefault), Kind: cards.FieldChoice, Value: g.Default, Choices: defaults},
		{ID: "strategy", Label: tr(txt.HubsLoginsFieldStrategy), Kind: cards.FieldChoice, Value: g.Strategy, Choices: []cards.Choice{
			{Value: "fill_first", Label: tr(txt.HubsLoginsStrategyFillFirst), Meaning: tr(txt.HubsLoginsStrategyFillFirstMeaning)},
			{Value: "round_robin", Label: tr(txt.HubsLoginsStrategyRoundRobin), Meaning: tr(txt.HubsLoginsStrategyRoundRobinMeaning)},
			{Value: "least_used", Label: tr(txt.HubsLoginsStrategyLeastUsed), Meaning: tr(txt.HubsLoginsStrategyLeastUsedMeaning)},
			{Value: "random", Label: tr(txt.HubsLoginsStrategyRandom), Meaning: tr(txt.HubsLoginsStrategyRandomMeaning)}}},
		{ID: "exhausted", Label: tr(txt.HubsLoginsFieldExhausted), Kind: cards.FieldChoice, Value: firstText(g.OnExhausted, "fail"), Choices: []cards.Choice{
			{Value: "fail", Label: tr(txt.HubsLoginsExhaustedAsk), Meaning: tr(txt.HubsLoginsExhaustedAskMeaning)},
			{Value: "rotate", Label: tr(txt.HubsLoginsExhaustedRotate), Meaning: tr(txt.HubsLoginsExhaustedRotateMeaning)}}},
	}}
	return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		params := tuigateway.LoginSetDefaultParams{Provider: g.Provider, OnExhausted: v["exhausted"], Strategy: v["strategy"]}
		if v["default"] != g.Default {
			params.Login = ptr(v["default"])
		}
		return hubCall(a, h, "", tuigateway.LoginSetDefault, params, func(*tuigateway.LoginSetDefaultResult) tea.Cmd {
			return tea.Batch(h.Done(tr(txt.HubsLoginsSaved, g.Name)), done())
		})
	})
}

// Adds a login on the hub: the provider picked, the given one first, or taken as given when chosen, then an API key, a sign in, or a key
// another place keeps
func (a *app) addLogin(h *cards.Hub, provider string, chosen bool, done func() tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	methods := func(slug, name string) tea.Cmd {
		choices := []hubChoice{{id: "key", title: tr(txt.HubsLoginsAddKey)}, {id: "oauth", title: tr(txt.HubsLoginsAddSignIn)},
			{id: "env", title: tr(txt.HubsLoginsAddEnv)}, {id: "tool", title: tr(txt.HubsLoginsAddTool), detail: tr(txt.HubsLoginsAddToolDetail)}}
		return a.hubPick(h, tr(txt.HubsLoginsAddHow, name), choices, "", func(c hubChoice) tea.Cmd {
			switch c.id {
			case "key":
				return a.addLoginKey(h, slug, name, "", done)
			case "oauth":
				return a.signIn(h, slug, name, "", done)
			case "tool":
				return a.hubPick(h, tr(txt.HubsLoginsAddTool), []hubChoice{{id: "claude_code", title: "Claude Code"}, {id: "qwen-cli", title: "Qwen CLI"}}, "", func(c hubChoice) tea.Cmd {
					params := tuigateway.LoginLinkParams{SessionID: sid, Provider: slug, Source: c.id, UseForSession: sid != ""}
					return hubCall(a, h, tr(txt.HubsLoginsSaving), tuigateway.LoginLink, params, func(r *tuigateway.LoginResult) tea.Cmd { return a.loginAdded(h, r, name, done) })
				})
			}
			spec := cards.FormSpec{Title: tr(txt.HubsLoginsAddEnvTitle), Submit: tr(txt.HubsLoginsAddSubmit), Fields: []cards.Field{
				{ID: "env", Label: tr(txt.HubsLoginsEnvLabel), Kind: cards.FieldText, Required: true, Placeholder: "ANTHROPIC_API_KEY", Help: tr(txt.HubsLoginsEnvHelp)},
				{ID: "name", Label: tr(txt.HubsLoginsNameLabel), Kind: cards.FieldText, Placeholder: tr(txt.HubsLoginsNameHint)},
			}}
			return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
				params := tuigateway.LoginLinkParams{SessionID: sid, Provider: slug, EnvVar: strings.TrimSpace(v["env"]), Name: strings.TrimSpace(v["name"]), UseForSession: sid != ""}
				return hubCall(a, h, tr(txt.HubsLoginsSaving), tuigateway.LoginLink, params, func(r *tuigateway.LoginResult) tea.Cmd { return a.loginAdded(h, r, name, done) })
			})
		})
	}
	return hubCall(a, h, tr(txt.HubsLoading), tuigateway.ModelOptions, tuigateway.ModelOptionsParams{SessionID: sid, IncludeUnconfigured: true}, func(r *tuigateway.ModelOptionsResult) tea.Cmd {
		var choices []hubChoice
		names := map[string]string{}
		for _, p := range r.Providers {
			names[p.Slug] = providerName(p)
			choices = append(choices, hubChoice{id: p.Slug, title: providerName(p), detail: p.Slug})
		}
		if name, known := names[provider]; known && chosen {
			return methods(provider, name)
		}
		return a.hubPick(h, tr(txt.HubsLoginsAddProvider), choices, provider, func(c hubChoice) tea.Cmd { return methods(c.id, c.title) })
	})
}

// Asks for an API key, masked, and saves it as a login of the provider, replacing the key of the named login when one is named
func (a *app) addLoginKey(h *cards.Hub, provider, providerName, login string, done func() tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	title := tr(txt.HubsLoginsAddKeyTitle, providerName)
	fields := []cards.Field{{ID: "key", Label: tr(txt.HubsLoginsKeyLabel), Kind: cards.FieldSecret, Required: true, Help: tr(txt.HubsLoginsKeyHelp)}}
	if login == "" {
		fields = append(fields,
			cards.Field{ID: "name", Label: tr(txt.HubsLoginsNameLabel), Kind: cards.FieldText, Placeholder: tr(txt.HubsLoginsNameHint)},
			cards.Field{ID: "default", Label: tr(txt.HubsLoginsMakeDefault), Kind: cards.FieldBool, Value: "false", Help: tr(txt.HubsLoginsMakeDefaultHelp)},
			cards.Field{ID: "rotate", Label: tr(txt.HubsLoginsInRotation), Kind: cards.FieldBool, Value: "false", More: true, Help: tr(txt.HubsLoginsInRotationHelp)})
	} else {
		title = tr(txt.HubsLoginsReplaceKeyTitle, login)
	}
	return a.hubForm(h, cards.FormSpec{Title: title, Submit: tr(txt.HubsLoginsAddSubmit), Fields: fields}, func(_ *cards.Form, v map[string]string) tea.Cmd {
		name := login
		if name == "" {
			name = strings.TrimSpace(v["name"])
		}
		params := tuigateway.LoginAddKeyParams{SessionID: sid, Provider: provider, APIKey: strings.TrimSpace(v["key"]), Name: name,
			MakeDefault: v["default"] == "true", Rotate: v["rotate"] == "true", UseForSession: sid != "" && login == ""}
		return hubCall(a, h, tr(txt.HubsLoginsSaving), tuigateway.LoginAddKey, params, func(r *tuigateway.LoginResult) tea.Cmd { return a.loginAdded(h, r, providerName, done) })
	})
}

// Says on the hub that a login was added, and why the session could not use it when it could not
func (a *app) loginAdded(h *cards.Hub, r *tuigateway.LoginResult, providerName string, done func() tea.Cmd) tea.Cmd {
	if r.Warning != "" {
		return tea.Batch(h.Fail(tr(txt.HubsLoginsAdded, loginLabel(r.Login), providerName)+" "+r.Warning), done())
	}
	return tea.Batch(h.Done(tr(txt.HubsLoginsAdded, loginLabel(r.Login), providerName)), done())
}

// Signs in to a provider on a page of the hub: the link opened in this computer's browser and copied on request, the code to type there,
// any code to paste back asked in the hub, the page closing once the sign in ends; a named login is signed in again
func (a *app) signIn(h *cards.Hub, provider, providerName, login string, done func() tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	params := tuigateway.LoginOAuthStartParams{SessionID: sid, Provider: provider, Name: login, UseForSession: sid != "" && login == "", Browser: true}
	return hubCall(a, h, tr(txt.HubsLoginsSignInStarting, providerName), tuigateway.LoginOAuthStart, params, func(r *tuigateway.LoginOAuthStartResult) tea.Cmd {
		var (
			state    tuigateway.LoginOAuthPollResult
			opened   bool
			finished bool
			asked    string
			page     *cards.Page
		)
		t := a.toolkit
		cancel := func() tea.Cmd {
			if finished {
				return nil
			}
			finished = true
			return call(a.gw, tuigateway.LoginOAuthPoll, tuigateway.LoginOAuthPollParams{JobID: r.JobID, Cancel: true}, func(_ *tuigateway.LoginOAuthPollResult, err error) tea.Cmd {
				if err != nil {
					return h.Fail(rpcErrorMessage(err))
				}
				return h.Done(tr(txt.HubsLoginsSignInStopped))
			})
		}
		var poll func(input *string) tea.Cmd
		poll = func(input *string) tea.Cmd {
			if finished {
				return nil
			}
			// Closing the page or the hub stops the sign in
			if !h.Holds(page) || !h.Shown() {
				return cancel()
			}
			return call(a.gw, tuigateway.LoginOAuthPoll, tuigateway.LoginOAuthPollParams{JobID: r.JobID, Input: input}, func(p *tuigateway.LoginOAuthPollResult, err error) tea.Cmd {
				if finished {
					return nil
				}
				if err != nil {
					finished = true
					return tea.Batch(h.Close(page), h.Fail(rpcErrorMessage(err)), done())
				}
				state = *p
				var cmds []tea.Cmd
				if p.URL != "" && p.Open && !opened {
					opened = true
					if !openExternalURL(p.URL) {
						cmds = append(cmds, h.Fail(tr(txt.HubsLoginsBrowserDidNotOpen)))
					}
				}
				switch p.State {
				case "done":
					finished = true
					cmds = append(cmds, h.Close(page))
					if p.Login != nil {
						return tea.Batch(append(cmds, a.loginAdded(h, &tuigateway.LoginResult{Login: *p.Login, Warning: p.Warning}, providerName, done))...)
					}
					return tea.Batch(append(cmds, done())...)
				case "failed", "cancelled":
					finished = true
					return tea.Batch(append(cmds, h.Close(page), h.Fail(firstText(p.Error, tr(txt.HubsLoginsSignInStopped))), done())...)
				case "waiting_input":
					if p.Prompt != asked {
						asked = p.Prompt
						cmds = append(cmds, a.hubAsk(h, providerName, p.Prompt, "", false, func(v string) tea.Cmd { return poll(&v) }))
					}
				}
				cmds = append(cmds, tea.Tick(time.Second, func(time.Time) tea.Msg { return asyncDoneMsg{run: func() tea.Cmd { return poll(nil) }} }))
				return tea.Batch(cmds...)
			})
		}
		page = cards.NewPage(a.hubEnv(), cards.PageSpec{Title: tr(txt.HubsLoginsSignInTitle, providerName), Key: "tui:logins:signin", Context: signInContext,
			Lines: func(int) []string {
				out := []string{render.Heading(tr(txt.HubsLoginsSignInTitle, providerName), t), ""}
				switch {
				case state.URL == "":
					out = append(out, render.Dim(tr(txt.HubsLoginsSignInWaitingLink), t))
				case state.UserCode != "":
					out = append(out, tr(txt.HubsLoginsSignInOpenThis), render.Warm(state.URL, t), "", tr(txt.HubsLoginsSignInCodeLabel), render.Heading(state.UserCode, t))
				case opened:
					out = append(out, tr(txt.HubsLoginsSignInBrowser), render.Warm(state.URL, t))
				default:
					out = append(out, tr(txt.HubsLoginsSignInOpenThis), render.Warm(state.URL, t))
				}
				if state.Loopback {
					out = append(out, "", render.Dim(tr(txt.HubsLoginsSignInLoopback), t))
				}
				if n := len(state.Lines); n > 0 {
					out = append(out, "")
					for _, line := range state.Lines[max(n-6, 0):] {
						out = append(out, render.Dim(line, t))
					}
				}
				return append(out, "", render.Dim(tr(txt.HubsLoginsSignInWaiting), t))
			},
			Hints: func(*cards.Page) []wm.Hint {
				out := []wm.Hint{}
				if state.URL != "" {
					out = append(out, wm.Hint{Action: "tui.signin.open"}, wm.Hint{Action: "tui.signin.copy"})
				}
				if state.UserCode != "" {
					out = append(out, wm.Hint{Action: "tui.signin.code"})
				}
				return out
			},
			Actions: map[string]func(p *cards.Page) tea.Cmd{
				"tui.signin.open": func(*cards.Page) tea.Cmd {
					if state.URL != "" && !openExternalURL(state.URL) {
						return h.Fail(tr(txt.HubsLoginsBrowserDidNotOpen))
					}
					return nil
				},
				"tui.signin.copy": func(*cards.Page) tea.Cmd {
					if state.URL == "" {
						return nil
					}
					return a.hubCopy(h, state.URL, tr(txt.HubsLoginsWhatLink))
				},
				"tui.signin.code": func(*cards.Page) tea.Cmd {
					if state.UserCode == "" {
						return nil
					}
					return a.hubCopy(h, state.UserCode, tr(txt.HubsLoginsWhatCode))
				},
			},
		})
		h.Quiet()
		return tea.Batch(h.Push(page), poll(nil))
	})
}

// Asks what to do once the login a session pinned ran out: switch to another ready login, wait for it to reset and retry, retry now, use the
// rotation for this session or always, or add a login; the question stays until answered or put away
func (a *app) loginExhausted(ev tuigateway.LoginExhaustedPayload) tea.Cmd {
	sid := firstText(ev.SessionID, a.store.ui.Sid)
	var h *cards.Hub
	when := ""
	if ev.ResetsAt != nil {
		if at, err := time.Parse(time.RFC3339, *ev.ResetsAt); err == nil {
			when = at.Local().Format("15:04")
		}
	}
	provider := firstText(ev.Slug, ev.Provider)
	detail := func() []string {
		out := []string{tr(txt.HubsLoginsExhaustedLead, ev.Login.Name, provider)}
		if ev.Reason != "" {
			out = append(out, ev.Reason)
		}
		if when != "" {
			out = append(out, tr(txt.HubsLoginsExhaustedResets, when))
		}
		return out
	}
	settle := func(done string) func() tea.Cmd {
		return func() tea.Cmd { return tea.Batch(wm.RequestDismissOf(h), wm.Notify(done, wm.Success)) }
	}
	setLogin := func(params tuigateway.SessionSetLoginParams, done string) tea.Cmd {
		return hubCall(a, h, tr(txt.HubsLoginsPinning), tuigateway.SessionSetLogin, params, func(r *tuigateway.SessionSetLoginResult) tea.Cmd {
			if r.Warning != "" {
				return h.Fail(r.Warning)
			}
			return settle(done)()
		})
	}
	wait := func(now bool, done string) tea.Cmd {
		params := tuigateway.SessionLoginWaitParams{SessionID: sid, Provider: ev.Provider, Now: now}
		return hubCall(a, h, "", tuigateway.SessionLoginWait, params, func(r *tuigateway.SessionLoginWaitResult) tea.Cmd {
			if r.State == "idle" {
				return h.Fail(tr(txt.HubsLoginsExhaustedNoReset))
			}
			return settle(done)()
		})
	}
	var acts []hubAction
	for _, ready := range ev.Ready {
		id, name := ready.ID, ready.Name
		acts = append(acts, hubAction{label: tr(txt.HubsLoginsExhaustedSwitch, name), does: tr(txt.HubsLoginsExhaustedSwitchDoes), run: func() tea.Cmd {
			return setLogin(tuigateway.SessionSetLoginParams{SessionID: sid, Provider: ev.Provider, Login: &id, Retry: true}, tr(txt.HubsLoginsExhaustedSwitched, name))
		}})
	}
	if when != "" {
		acts = append(acts, hubAction{label: tr(txt.HubsLoginsExhaustedWait, when), does: tr(txt.HubsLoginsExhaustedWaitDoes), run: func() tea.Cmd {
			return wait(false, tr(txt.HubsLoginsExhaustedWaiting, when))
		}})
	}
	acts = append(acts, hubAction{label: tr(txt.HubsLoginsExhaustedRetry), does: tr(txt.HubsLoginsExhaustedRetryDoes), run: func() tea.Cmd {
		return wait(true, tr(txt.HubsLoginsExhaustedRetried))
	}})
	if ev.Rotation {
		acts = append(acts,
			hubAction{label: tr(txt.HubsLoginsExhaustedRotateSession), does: tr(txt.HubsLoginsExhaustedRotateSessionDoes), run: func() tea.Cmd {
				return setLogin(tuigateway.SessionSetLoginParams{SessionID: sid, Provider: ev.Provider, OnExhausted: ptr("rotate"), Retry: true}, tr(txt.HubsLoginsExhaustedRotating))
			}},
			hubAction{label: tr(txt.HubsLoginsExhaustedAlways, provider), does: tr(txt.HubsLoginsExhaustedAlwaysDoes), run: func() tea.Cmd {
				return hubCall(a, h, "", tuigateway.LoginSetDefault, tuigateway.LoginSetDefaultParams{Provider: ev.Provider, OnExhausted: "rotate"}, func(*tuigateway.LoginSetDefaultResult) tea.Cmd {
					return wait(true, tr(txt.HubsLoginsExhaustedRotating))
				})
			}})
	}
	acts = append(acts, hubAction{label: tr(txt.PickersModelAddLogin), does: tr(txt.PickersModelAddLoginDetail), run: func() tea.Cmd {
		return a.addLogin(h, provider, true, func() tea.Cmd { return nil })
	}})
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsLoginsExhaustedTitle, ev.Login.Name), Glyph: a.toolkit.Glyphs.Ext, Key: "tui:login:exhausted", Context: hubActionsContext, Primary: "hub.run",
		Columns: []render.Column{{Title: tr(txt.HubsColAction)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(acts))
			for i, act := range acts {
				out[i] = []string{act.label, act.does}
			}
			return out
		},
		Banner:  func() string { return strings.Join(detail(), " · ") },
		Hints:   func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.run"}} },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{"hub.run": func(_ *cards.List, i int) tea.Cmd { return acts[i].run() }},
	})
	h = a.newHub("tui:login:exhausted", list)
	h.Stick()
	return a.wm.ShowOverlay(h)
}
