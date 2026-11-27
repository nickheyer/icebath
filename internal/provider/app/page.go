package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Page of one provider: its accounts, then its address, models, usage, rotation and settings
type page struct {
	provider *providerv1.Provider
	list     *cards.List
}

// Row of a page: an account, a credential the provider takes from outside its accounts or leaves out, the row that connects the first
// account, or one of the views of the provider
type entry struct {
	account *providerv1.Account
	// Variable or program the credential comes from, said as the provider names it
	outside string
	// The provider leaves the outside credential out
	ignored bool
	connect bool
	view    string
}

const (
	viewAddress  = "address"
	viewModels   = "models"
	viewUsage    = "usage"
	viewRotation = "rotation"
	viewSettings = "settings"
)

// Most buttons a row shows, what enter does first
const mostHints = 5

// Rows of the page: the accounts, a credential from outside them while there are none, the places the provider leaves out, the row that
// connects the first account while nothing does, then the views
func (p *providers) entries(provider *providerv1.Provider) []entry {
	var rows []entry
	for _, account := range provider.GetAccounts() {
		rows = append(rows, entry{account: account})
	}
	origin := provider.GetCredentialOrigin()
	if len(provider.GetAccounts()) == 0 && (origin == providerv1.Origin_ORIGIN_ENVIRONMENT || origin == providerv1.Origin_ORIGIN_EXTERNAL) {
		rows = append(rows, entry{outside: provider.GetCredentialSource()})
	}
	for _, source := range provider.GetIgnored() {
		rows = append(rows, entry{outside: source, ignored: true})
	}
	if len(provider.GetAccounts()) == 0 && !connected(origin) && provider.GetUsable() && len(p.ways(provider)) > 0 {
		rows = append(rows, entry{connect: true})
	}
	if provider.GetLocal() || provider.GetBaseUrl() == "" {
		rows = append(rows, entry{view: viewAddress})
	}
	rows = append(rows, entry{view: viewModels})
	if provider.GetUsage() {
		rows = append(rows, entry{view: viewUsage})
	}
	if len(provider.GetAccounts()) > 1 {
		rows = append(rows, entry{view: viewRotation})
	}
	return append(rows, entry{view: viewSettings})
}

// Variable a credential from outside the accounts comes from, without the store that supplied it
func variable(source string) string {
	name, _, _ := strings.Cut(source, " (from ")
	return strings.TrimSpace(name)
}

// Whether a credential from outside the accounts is a variable rather than another program
func isVariable(source string) bool {
	return config.EnvName(variable(source))
}

// Name of a row of a credential from outside the accounts
func outsideName(source string) string {
	if isVariable(source) {
		return "Key in " + variable(source)
	}
	return source
}

// A credential from outside the accounts as a sentence names it
func outsideWords(source string) string {
	if isVariable(source) {
		return "the key in " + variable(source)
	}
	return "the sign-in of " + source
}

// Opens the page of the provider over the list
func (p *providers) show(provider *providerv1.Provider) tea.Cmd {
	t := p.kit.Env.Theme
	pg := &page{provider: provider}
	var rows []entry
	pg.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   provider.GetDisplayName(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + provider.GetName(),
		Context: loginsContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Intro:   func() []string { return []string{p.standing(pg.provider)} },
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			rows = p.entries(pg.provider)
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = p.cells(pg.provider, row)
			}
			return cells
		},
		Group: func(i int) string {
			if rows[i].view != "" {
				return "More"
			}
			return "Accounts"
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows) {
				return nil
			}
			return p.entryDetail(pg.provider, rows[i])
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(rows) {
				return p.adding(pg.provider, nil)
			}
			return p.entryHints(pg.provider, rows[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := rows[i]
				switch {
				case row.account != nil:
					return p.accountActions(pg, row.account.GetName())
				case row.outside != "" && row.ignored:
					return p.unignore(pg, row.outside)
				case row.outside != "" && isVariable(row.outside):
					return p.keepOutside(pg, row.outside)
				case row.outside != "":
					return p.ignore(pg, row.outside)
				case row.connect:
					return p.connect(p.first(pg.provider))
				}
				return p.openView(pg, row.view)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if account := rows[i].account; account != nil {
					return p.connect(p.target(pg, account))
				}
				return nil
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd {
				if account := rows[i].account; account != nil {
					return p.rename(pg, account)
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.account != nil:
					return p.remove(pg, row.account)
				case row.outside != "" && !row.ignored:
					return p.ignore(pg, row.outside)
				}
				return nil
			},
			"providers.logins.rotate": func(_ *cards.List, i int) tea.Cmd { return p.toggle(pg, rows[i].account) },
			"providers.logins.reset":  func(_ *cards.List, i int) tea.Cmd { return p.reset(pg, rows[i].account) },
			"hub.add":                 func(*cards.List, int) tea.Cmd { return p.newAccount(pg) },
			"hub.refresh":             func(*cards.List, int) tea.Cmd { return p.load(pg.provider.GetName()) },
		},
	})
	pg.list.ToggleDetail()
	pg.list.Refresh()
	p.page = pg
	p.views = append(p.views, pg.list)
	return p.hub.Push(pg.list)
}

// Adds an account: the first one without asking a name, any other once its name is given
func (p *providers) newAccount(pg *page) tea.Cmd {
	if len(p.ways(pg.provider)) == 0 {
		return nil
	}
	if len(pg.provider.GetAccounts()) == 0 {
		return p.connect(p.first(pg.provider))
	}
	return p.account(pg.provider)
}

// Target of an account of the page's provider
func (p *providers) target(pg *page, account *providerv1.Account) Target {
	return Target{Profile: p.profile, Provider: pg.provider, Account: account}
}

// Cells of a row of a page: its name, the mark of a row that opens a view, and its state
func (p *providers) cells(provider *providerv1.Provider, row entry) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.account != nil:
		return []string{row.account.GetTitle(), "", p.accountState(provider, row.account)}
	case row.outside != "" && row.ignored:
		return []string{outsideName(row.outside), "", render.Dim("Not used", t)}
	case row.outside != "":
		return []string{outsideName(row.outside), "", render.Good("Connected", t) + render.Dim(", in use", t)}
	case row.connect:
		return []string{"Connect your account", opens, render.Dim(p.waysWords(provider), t)}
	}
	switch row.view {
	case viewAddress:
		if base := provider.GetBaseUrl(); base != "" {
			return []string{"Address", opens, render.Dim(base, t)}
		}
		return []string{"Address", opens, render.Warm("Not set", t)}
	case viewModels:
		about := "Every model listed"
		if n := len(provider.GetModels()); n > 0 {
			about = render.Count(n, "model") + " turned on"
		}
		return []string{"Models", opens, render.Dim(about, t)}
	case viewUsage:
		return []string{"Usage", opens, render.Dim(cmp.Or(glance(provider.GetAccountUsage()), "What the account may still spend"), t)}
	case viewRotation:
		return []string{"Rotation", opens, render.Dim(choice(provider), t)}
	}
	return []string{"Settings", opens, render.Dim("Name, default model, address and more", t)}
}

// The ways to connect an account of the provider in a few words
func (p *providers) waysWords(provider *providerv1.Provider) string {
	ways := p.ways(provider)
	titles := make([]string, 0, len(ways))
	for _, w := range ways {
		titles = append(titles, w.Title)
	}
	switch len(titles) {
	case 0:
		return ""
	case 1:
		return titles[0]
	case 2:
		return titles[0] + " or " + titles[1]
	}
	return titles[0] + ", " + titles[1] + " and more"
}

// Lines under the list about a row of a page: what it is, then the facts a person acts on
func (p *providers) entryDetail(provider *providerv1.Provider, row entry) []string {
	t := p.kit.Env.Theme
	name := provider.GetDisplayName()
	switch {
	case row.account != nil:
		return p.detail(provider, row.account)
	case row.outside != "" && row.ignored && isVariable(row.outside):
		return []string{name + " takes no key from " + variable(row.outside) + ".", render.Dim("Use again lets it read the key there once more.", t)}
	case row.outside != "" && row.ignored:
		return []string{name + " takes nothing from " + row.outside + ".", render.Dim("Use again lets it take the sign-in there once more.", t)}
	case row.outside != "" && isVariable(row.outside):
		return []string{"The agent uses the API key in " + row.outside + ", which you set outside icebath.",
			render.Dim("Keep key copies it into an account of "+name+". Stop using leaves the variable out from now on.", t)}
	case row.outside != "":
		return []string{"The agent uses the sign-in that " + row.outside + " keeps on this computer.",
			render.Dim("Stop using leaves it out from now on. Its own files stay as they are.", t)}
	case row.connect:
		return []string{"Sign in or give an API key, and the agent can use " + name + "."}
	}
	switch row.view {
	case viewAddress:
		lines := []string{"Where requests to " + name + " go. The server's own documentation calls it the Base URL."}
		if base := provider.GetBaseUrl(); base != "" {
			lines = append(lines, render.Dim("Now: "+base, t))
		}
		return lines
	case viewModels:
		return []string{"The models that /model offers for " + name + "."}
	case viewUsage:
		return []string{"What " + name + " says the account in use may still spend."}
	case viewRotation:
		return []string{"How " + name + " picks among its accounts.", render.Dim(choice(provider), t)}
	}
	return []string{"Every setting of " + name + ", on the settings screen."}
}

// Buttons for a row of a page: what enter does, then the other keys that apply to it, most used first
func (p *providers) entryHints(provider *providerv1.Provider, row entry) []wm.Hint {
	var hints []wm.Hint
	switch {
	case row.account != nil:
		account := row.account
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "open"}, wm.Hint{Action: "hub.edit", Label: connectVerb(account)})
		if limited(account) {
			hints = append(hints, wm.Hint{Action: "providers.logins.reset"})
		}
		if len(provider.GetAccounts()) > 1 {
			label := "rotate"
			if account.GetRotate() {
				label = "stop rotating"
			}
			hints = append(hints, wm.Hint{Action: "providers.logins.rotate", Label: label})
		}
		hints = append(hints, wm.Hint{Action: "hub.remove"})
	case row.outside != "" && row.ignored:
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "use again"})
	case row.outside != "" && isVariable(row.outside):
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "keep key"}, wm.Hint{Action: "hub.remove", Label: "stop using"})
	case row.outside != "":
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "stop using"})
	case row.connect:
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "connect"})
	case row.view == viewAddress:
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "change"})
	case row.view == viewRotation:
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "change"})
	default:
		hints = append(hints, wm.Hint{Action: "hub.use", Label: "open"})
	}
	return p.adding(provider, hints)
}

// Buttons with the one that adds an account after them, when the provider takes accounts and there is room
func (p *providers) adding(provider *providerv1.Provider, hints []wm.Hint) []wm.Hint {
	if len(hints) < mostHints && len(p.ways(provider)) > 0 {
		hints = append(hints, wm.Hint{Action: "hub.add", Label: "add account"})
	}
	return hints[:min(len(hints), mostHints)]
}

// Opens a view of the provider: its address, models, usage, rotation or settings
func (p *providers) openView(pg *page, view string) tea.Cmd {
	switch view {
	case viewAddress:
		return p.address(pg.provider, false)
	case viewModels:
		return p.models(pg)
	case viewUsage:
		return p.usage(pg)
	case viewRotation:
		return p.rotation(pg)
	}
	return p.configure(pg.provider)
}

// Something a list of actions offers: its name, what it does, the word on its button and the run of it
type action struct {
	title, about, verb string
	run                func() tea.Cmd
	// Closes the list before it runs, for an action that leads somewhere else or ends what the list is about
	closes bool
}

// Opens a list of actions over the hub, each with what it does beside it or under the list, and the lines of detail after it. The actions
// are read again whenever the providers are
func (p *providers) actions(title string, intro []string, detail func() []string, acts func() []action) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []action
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Models,
		Key:     "providers:actions:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return intro },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = acts()
			cells := make([][]string, len(shown))
			for i, act := range shown {
				cells[i] = []string{act.title, render.Dim(act.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			var lines []string
			if i >= 0 && i < len(shown) && !list.Shows(1) && shown[i].about != "" {
				lines = append(lines, shown[i].about)
			}
			if detail != nil {
				if more := detail(); len(more) > 0 {
					if len(lines) > 0 {
						lines = append(lines, "")
					}
					lines = append(lines, more...)
				}
			}
			return lines
		},
		Empty: func() string { return "There is nothing to do here now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: shown[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				act := shown[i]
				if act.closes {
					return tea.Batch(p.hub.Close(list), act.run())
				}
				return act.run()
			},
		},
	})
	list.ToggleDetail()
	p.views = append(p.views, list)
	return p.hub.Push(list)
}

// Everything an account can do, on a list of its own: whether it is always used, connecting it again, its place in the rotation and the
// order, forgetting its limit, renewing its sign-in, keeping its key, renaming and removing it
func (p *providers) accountActions(pg *page, name string) tea.Cmd {
	current := func() *providerv1.Account {
		for _, account := range pg.provider.GetAccounts() {
			if account.GetName() == name {
				return account
			}
		}
		return nil
	}
	account := current()
	if account == nil {
		return nil
	}
	detail := func() []string {
		if now := current(); now != nil {
			return p.detail(pg.provider, now)
		}
		return nil
	}
	return p.actions(account.GetTitle(), nil, detail, func() []action {
		account := current()
		if account == nil {
			return nil
		}
		provider := pg.provider
		title := account.GetTitle()
		var acts []action
		if provider.GetPinned() && provider.GetAccount() == name {
			acts = append(acts, action{title: "Stop always using it", about: "Go back to trying every account in order", verb: "stop",
				run: func() tea.Cmd { return p.use(pg, nil) }})
		} else {
			acts = append(acts, action{title: "Always use it", about: "Make every request with this account, even when others take turns", verb: "always use",
				run: func() tea.Cmd { return p.use(pg, account) }})
		}
		connect := action{title: "Connect", about: "Sign in or give an API key", verb: "connect", closes: true,
			run: func() tea.Cmd { return p.connect(p.target(pg, account)) }}
		if connected(account.GetCredentialOrigin()) {
			connect.title, connect.about, connect.verb = "Connect again", "Sign in once more or give a new API key", connectVerb(account)
		}
		acts = append(acts, connect)
		if limited(account) {
			acts = append(acts, action{title: "Forget the limit", about: "Try this account again now, before the limit lifts", verb: "forget",
				run: func() tea.Cmd { return p.reset(pg, account) }})
		}
		if account.GetRenewable() {
			acts = append(acts, action{title: "Renew the sign-in", about: "Get a fresh sign-in from " + provider.GetDisplayName() + " now", verb: "renew",
				run: func() tea.Cmd { return p.renew(pg, account) }})
		}
		if all := provider.GetAccounts(); len(all) > 1 {
			if account.GetRotate() {
				acts = append(acts, action{title: "Take out of the rotation", about: "Stop taking turns with the other accounts", verb: "stop rotating",
					run: func() tea.Cmd { return p.toggle(pg, account) }})
			} else {
				acts = append(acts, action{title: "Put in the rotation", about: "Take turns with the other accounts in the rotation", verb: "rotate",
					run: func() tea.Cmd { return p.toggle(pg, account) }})
			}
			place := slices.IndexFunc(all, func(other *providerv1.Account) bool { return other.GetName() == name })
			if place > 0 {
				acts = append(acts, action{title: "Try it earlier", about: "Try this account before " + all[place-1].GetTitle(), verb: "move up",
					run: func() tea.Cmd { return p.move(pg, account, -1) }})
			}
			if place >= 0 && place < len(all)-1 {
				acts = append(acts, action{title: "Try it later", about: "Try this account after " + all[place+1].GetTitle(), verb: "move down",
					run: func() tea.Cmd { return p.move(pg, account, 1) }})
			}
		}
		if account.GetKeyEnv() != "" {
			acts = append(acts, action{title: "Keep the key in icebath", about: "Copy the key from " + account.GetKeyEnv() + " and stop reading the variable",
				verb: "keep key", run: func() tea.Cmd { return p.keep(pg, account) }})
		}
		return append(acts,
			action{title: "Rename", about: "Give " + title + " another name", verb: "rename", closes: true, run: func() tea.Cmd { return p.rename(pg, account) }},
			action{title: "Remove", about: "Delete " + title + " and what icebath keeps for it", verb: "remove", closes: true,
				run: func() tea.Cmd { return p.remove(pg, account) }})
	})
}

// Asks before an account is removed, then removes it and says which variables were cleared with it
func (p *providers) remove(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil {
		return nil
	}
	question := "Remove the account " + account.GetTitle() + "? What icebath keeps for it is deleted."
	if len(pg.provider.GetAccounts()) == 1 {
		question = "Remove the account " + account.GetTitle() + "? " + pg.provider.GetDisplayName() + " has no other account to use."
	}
	pg.list.Confirm(question, func() tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.RemoveAccountResponse, error) {
			return p.service.RemoveAccount(p.kit.Env.Ctx, &providerv1.RemoveAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
		}, func(removed *providerv1.RemoveAccountResponse) tea.Cmd {
			done := "Removed " + account.GetTitle() + "."
			if cleared := removed.GetCleared(); len(cleared) > 0 {
				done = "Removed " + account.GetTitle() + " and cleared " + strings.Join(cleared, " and ") + " from your keys file."
			}
			return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
		})
	})
	return nil
}

// Asks before the provider leaves a credential from outside its accounts out, then leaves it out from now on
func (p *providers) ignore(pg *page, source string) tea.Cmd {
	name := pg.provider.GetDisplayName()
	question := "Stop using the sign-in of " + source + " for " + name + "? Its own files stay as they are."
	if isVariable(source) {
		question = "Stop using the key in " + variable(source) + " for " + name + "? If your keys file holds it, it is cleared there too."
	}
	pg.list.Confirm(question, func() tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.RemoveAccountResponse, error) {
			return p.service.RemoveAccount(p.kit.Env.Ctx, &providerv1.RemoveAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Source: variable(source)})
		}, func(left *providerv1.RemoveAccountResponse) tea.Cmd {
			done := name + " no longer uses " + outsideWords(source) + "."
			if isVariable(source) && !slices.Contains(left.GetCleared(), variable(source)) {
				done += " The variable is still set in your shell, but " + name + " leaves it out."
			}
			return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
		})
	})
	return nil
}

// Lets the provider take a credential from a place it left out once more
func (p *providers) unignore(pg *page, source string) tea.Cmd {
	kept := slices.DeleteFunc(slices.Clone(pg.provider.GetIgnored()), func(have string) bool { return have == source })
	value, err := encodeList(kept)
	if err != nil {
		return p.hub.Fail(err.Error())
	}
	return tui.Call(p.hub, func() (struct{}, error) {
		return struct{}{}, p.set(pg.provider.GetSection()+"."+providerconfig.Ignored, value)
	}, func(struct{}) tea.Cmd {
		return tea.Batch(p.hub.Done(pg.provider.GetDisplayName()+" uses "+outsideWords(source)+" again when it is there."), p.load(pg.provider.GetName()))
	})
}

// Copies the key a provider reads from a variable outside its accounts into an account of its own, which stops reading the variable
func (p *providers) keepOutside(pg *page, source string) tea.Cmd {
	name := variable(source)
	pg.list.Confirm("Keep the key in "+name+" in an account of "+pg.provider.GetDisplayName()+"? The account stops reading the variable.", func() tea.Cmd {
		target := p.first(pg.provider)
		return tui.Call(p.hub, func() (*providerv1.KeepKeyResponse, error) {
			ready, err := target.Ready(p.kit.Env.Ctx)
			if err != nil {
				return nil, err
			}
			if err := Store(p.kit.Env.Ctx, p.settings, ready, map[string]any{providerconfig.KeyEnv: name}); err != nil {
				return nil, err
			}
			return p.service.KeepKey(p.kit.Env.Ctx, &providerv1.KeepKeyRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: ready.Account.GetName()})
		}, func(kept *providerv1.KeepKeyResponse) tea.Cmd {
			return tea.Batch(p.hub.Done("The account "+kept.GetAccount().GetTitle()+" now keeps the key from "+name+"."), p.load(pg.provider.GetName()))
		})
	})
	return nil
}
