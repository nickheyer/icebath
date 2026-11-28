package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of a provider's page: an account, a credential found outside the accounts, the row that adds an account, an option of the provider
// by name, or a setting of its kind as Settings draws it
type pageRow struct {
	account *providerv1.Account
	// Variable or program of a credential found outside the accounts, and whether the provider leaves it out
	outside string
	off     bool
	adds    bool
	option  string
	setting *configv1.Setting
}

// Options of a provider's page, and the rows after them that open a page or act
const (
	optionName      = "name"
	optionDefault   = "default"
	optionModels    = "models"
	optionOffered   = "offered"
	optionAddress   = "address"
	optionUsed      = "used"
	optionOrder     = "order"
	optionExhausted = "exhausted"
	optionUsage     = "usage"
	optionAdvanced  = "advanced"
	optionRemove    = "remove"
)

// Settings of a provider the options stand for, by option
var optionKeys = map[string]string{optionName: providerconfig.Title, optionDefault: providerconfig.DefaultModel, optionModels: providerconfig.Models,
	optionAddress: providerconfig.BaseURL, optionUsed: providerconfig.Account, optionOrder: providerconfig.Strategy,
	optionExhausted: providerconfig.OnExhausted}

// Whether the row is one of the things the page manages
func (e pageRow) thing() bool { return e.account != nil || e.outside != "" || e.adds }

// Whether the row opens a page inside this one
func (e pageRow) opens() bool { return e.option == optionUsage || e.option == optionAdvanced }

// Setting of the provider the option stands for, nil for an option that stands for none
func (p *providers) settingOf(entry *providerv1.Provider, option string) *configv1.Setting {
	if name := optionKeys[option]; name != "" {
		return p.options.Setting(entry.GetSection() + "." + name)
	}
	return nil
}

// Rows of the page: the accounts, the credentials found outside them, the row that adds an account; then the options, those of two
// accounts or more only with two, the kind's own settings; then usage, advanced and remove
func (p *providers) entries(entry *providerv1.Provider) []pageRow {
	var rows []pageRow
	for _, account := range entry.GetAccounts() {
		rows = append(rows, pageRow{account: account})
	}
	if source := entry.GetOutsideSource(); source != "" {
		rows = append(rows, pageRow{outside: source})
	}
	for _, source := range entry.GetIgnored() {
		rows = append(rows, pageRow{outside: source, off: true})
	}
	if len(p.ways(entry)) > 0 {
		rows = append(rows, pageRow{adds: true})
	}
	options := []string{optionName, optionDefault, optionModels, optionOffered}
	if entry.GetOwn() {
		options = append(options, optionAddress)
	}
	// How the accounts are chosen matters with two or more, or one and a credential found outside it; their order only with two or more
	switch n := len(entry.GetAccounts()); {
	case n > 1:
		options = append(options, optionUsed, optionOrder, optionExhausted)
	case n == 1 && entry.GetOutsideSource() != "":
		options = append(options, optionUsed, optionExhausted)
	}
	for _, option := range options {
		rows = append(rows, pageRow{option: option})
	}
	everyday, _ := p.options.Section(entry.GetSection())
	for _, setting := range everyday {
		rows = append(rows, pageRow{setting: setting})
	}
	if entry.GetUsage() {
		rows = append(rows, pageRow{option: optionUsage})
	}
	return append(rows, pageRow{option: optionAdvanced}, pageRow{option: optionRemove})
}

// Whether a credential from outside the accounts is a variable rather than another program
func isVariable(source string) bool {
	return config.EnvName(credentials.SourceName(source))
}

// Name of a row of a credential from outside the accounts
func outsideName(source string) string {
	if isVariable(source) {
		return "Key in " + credentials.SourceName(source)
	}
	return source + " on this computer"
}

// A credential from outside the accounts as a sentence names it
func outsideWords(source string) string {
	if isVariable(source) {
		return "the API key in " + credentials.SourceName(source)
	}
	return "the sign-in of " + source + " on this computer"
}

// Opens the page of the provider over the list: who it is connected as or what it needs, its accounts, its options, then what opens and
// removing it
func (p *providers) show(name string) tea.Cmd {
	t := p.kit.Env.Theme
	current := func() *providerv1.Provider { return p.find(name) }
	if current() == nil {
		return nil
	}
	var rows []pageRow
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   current().GetDisplayName(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + name,
		Context: loginsContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Intro: func() []string {
			entry := current()
			switch {
			case entry == nil:
				return nil
			case Ready(entry):
				return []string{entry.GetStanding()}
			}
			return []string{render.Warm(entry.GetStanding(), t)}
		},
		Columns: pageColumns,
		Rows: func() [][]string {
			entry := current()
			if entry == nil {
				rows = nil
				return nil
			}
			rows = p.entries(entry)
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = p.cells(entry, row)
			}
			return cells
		},
		Group: func(i int) string {
			switch row := rows[i]; {
			case row.thing():
				return "Accounts"
			case row.setting != nil, row.option != "" && !row.opens() && row.option != optionRemove:
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			if entry := current(); entry != nil && i >= 0 && i < len(rows) {
				return p.entryDetail(entry, rows[i])
			}
			return nil
		},
		Empty: func() string { return "This provider is no longer in this profile." },
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			entry := current()
			if entry == nil || i < 0 || i >= len(rows) {
				return nil
			}
			return p.entryHints(entry, rows[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if entry := current(); entry != nil {
					return p.use(l, entry, rows[i])
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := current()
				if entry == nil {
					return nil
				}
				switch row := rows[i]; {
				case row.account != nil:
					return p.removeAccount(l, entry, row.account)
				case row.outside != "" && !row.off:
					return p.turnOutside(entry, row.outside, false)
				case row.setting != nil:
					return p.options.Reset(l, row.setting)
				case row.option == optionName:
					return p.unname(l, entry)
				default:
					if setting := p.settingOf(entry, row.option); setting != nil {
						return p.options.Reset(l, setting)
					}
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				if entry := current(); entry != nil {
					return p.addAccount(entry)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return tea.Batch(p.load(name), p.options.Load(nil)) },
			"providers.logins.rotate": func(l *cards.List, i int) tea.Cmd {
				if entry := current(); entry != nil && rows[i].account != nil && len(entry.GetAccounts()) > 1 {
					return p.doAct(l, entry, rows[i].account, act{name: actTurns})
				}
				return nil
			},
			"providers.logins.reset": func(l *cards.List, i int) tea.Cmd {
				if entry := current(); entry != nil && rows[i].account != nil && limited(rows[i].account) {
					return p.doAct(l, entry, rows[i].account, act{name: actForget})
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	list.Refresh()
	p.views.Add(list)
	p.options.Watch(list)
	return p.hub.Push(list)
}

// Keymap context of the accounts on a provider's page
const loginsContext = "providers.logins"

// Keys of an account on a provider's page, beside opening its page
var loginKeys = []keys.Action{
	{ID: "providers.logins.rotate", Context: loginsContext, Label: "take turns", Doc: "Make the highlighted account take turns, or stop", Default: []string{"t"}},
	{ID: "providers.logins.reset", Context: loginsContext, Label: "forget limit", Doc: "Forget the limit that the highlighted account ran into",
		Default: []string{"R"}},
}

// Columns of a page's rows, those Settings draws its options in: the name, the mark, the value or state
var pageColumns = []render.Column{{}, {}, {Flex: 1}}

// Cells of a row of a provider's page: its name, the mark of a changed value or of a row that opens, and its state or value
func (p *providers) cells(entry *providerv1.Provider, row pageRow) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case row.account != nil:
		return []string{row.account.GetTitle(), "", p.accountWords(row.account)}
	case row.outside != "" && row.off:
		return []string{outsideName(row.outside), "", render.Dim("Off", t)}
	case row.outside != "" && entry.GetOutsideUsed():
		return []string{outsideName(row.outside), "", render.Good("In use", t)}
	case row.outside != "":
		return []string{outsideName(row.outside), "", "On"}
	case row.adds:
		return []string{"Add an account", opens, ""}
	case row.setting != nil:
		return p.options.Cells(row.setting)
	}
	switch row.option {
	case optionOffered:
		if entry.GetHidden() {
			return []string{"Offered in the model list", "", "Off"}
		}
		return []string{"Offered in the model list", "", "On"}
	case optionModels:
		return []string{"Models offered", opens, render.Dim(offeredWords(entry), t)}
	case optionUsage:
		return []string{"Usage", opens, render.Dim(glance(entry.GetAccountUsage()), t)}
	case optionAdvanced:
		return []string{"Advanced", opens, render.Dim(render.Count(p.rare(entry), "setting"), t)}
	case optionRemove:
		return []string{"Remove", "", ""}
	}
	if setting := p.settingOf(entry, row.option); setting != nil {
		return p.options.Cells(setting)
	}
	return []string{row.option, "", ""}
}

// The models a provider offers in the model list, in words
func offeredWords(entry *providerv1.Provider) string {
	switch n := len(entry.GetModels()); n {
	case 0:
		return "All"
	case 1:
		return entry.GetModels()[0]
	default:
		return render.Count(n, "model")
	}
}

// State of an account as its row says it, by the one function of the server, warm while it can't be used
func (p *providers) accountWords(account *providerv1.Account) string {
	t := p.kit.Env.Theme
	if account.GetStatus() == "ready" {
		return render.Good(account.GetState(), t)
	}
	return render.Warm(account.GetState(), t)
}

// Lines under the list about a row of a provider's page, never what the row or the first line says
func (p *providers) entryDetail(entry *providerv1.Provider, row pageRow) []string {
	name := entry.GetDisplayName()
	switch {
	case row.account != nil:
		return p.accountFacts(entry, row.account)
	case row.outside != "" && row.off:
		return []string{name + " leaves out " + outsideWords(row.outside) + "."}
	case row.outside != "" && !entry.GetOutsideUsed():
		return []string{unusedOutside(entry, row.outside)}
	case row.outside != "":
		return []string{"The agent uses " + outsideWords(row.outside) + ", which you keep outside icebath."}
	case row.adds:
		return []string{"Sign in to another account, or add another API key."}
	case row.setting != nil:
		return p.options.Detail(row.setting, false)
	}
	switch row.option {
	case optionOffered:
		return []string{"Whether the model list, and so the model picker, offers the models of " + name + "."}
	case optionUsage:
		return []string{"What " + name + " says the account in use may still spend."}
	case optionAdvanced:
		return []string{"Settings of " + name + " that few people change."}
	case optionRemove:
		return []string{"Take " + name + " out of icebath, with its accounts."}
	}
	setting := p.settingOf(entry, row.option)
	if setting == nil {
		return nil
	}
	lines := p.options.Detail(setting, false)
	// The other way out than the one the first line of the page names, under what the option is
	if instead := entry.GetInstead(); instead != "" && optionKeys[row.option] == entry.GetInsteadOf() && len(lines) > 0 {
		lines = slices.Insert(lines, 1, render.Warm(instead, p.kit.Env.Theme))
	}
	return lines
}

// Why the provider leaves the credential found outside its accounts unused now
func unusedOutside(entry *providerv1.Provider, source string) string {
	name, found := entry.GetDisplayName(), outsideWords(source)
	switch {
	case entry.GetPinned():
		return name + " finds " + found + ", but uses the account " + accountTitle(entry, entry.GetAccount()) + " alone."
	case entry.GetRotate():
		return name + " finds " + found + ", but its accounts take turns."
	case !Ready(entry):
		return name + " finds " + found + ", but can't send requests yet."
	}
	return name + " finds " + found + ", and takes it once none of its accounts can sign in."
}

// Buttons for a row of a provider's page: what enter does, then the other keys that apply to it
func (p *providers) entryHints(entry *providerv1.Provider, row pageRow) []wm.Hint {
	switch {
	case row.account != nil:
		hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
		if len(entry.GetAccounts()) > 1 {
			hints = append(hints, wm.Hint{Action: "providers.logins.rotate", Label: map[bool]string{true: "stop taking turns", false: "take turns"}[row.account.GetRotate()]})
		}
		if limited(row.account) {
			hints = append(hints, wm.Hint{Action: "providers.logins.reset"})
		}
		return append(hints, wm.Hint{Action: "hub.remove", Label: "remove"}, wm.Hint{Action: "hub.add", Label: "add account"})
	case row.outside != "" && row.off:
		return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
	case row.outside != "":
		return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
	case row.adds:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	case row.setting != nil:
		return p.options.Hints(row.setting)
	case row.opens():
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	case row.option == optionRemove:
		return []wm.Hint{{Action: "hub.use", Label: "remove"}}
	case row.option == optionOffered && entry.GetHidden():
		return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
	case row.option == optionOffered:
		return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
	}
	hints := []wm.Hint{{Action: "hub.use", Label: "change"}}
	if setting := p.settingOf(entry, row.option); setting != nil && setting.GetSource() == configv1.Source_SOURCE_FILE {
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
	}
	return hints
}

// Does what enter does on a row of a provider's page
func (p *providers) use(l *cards.List, entry *providerv1.Provider, row pageRow) tea.Cmd {
	switch {
	case row.account != nil:
		return p.accountPage(entry.GetName(), row.account.GetName())
	case row.outside != "":
		return p.turnOutside(entry, row.outside, row.off)
	case row.adds:
		return p.addAccount(entry)
	case row.setting != nil:
		return p.options.Change(row.setting)
	}
	switch row.option {
	case optionName:
		return p.renameProvider(entry)
	case optionDefault:
		return p.pickDefault(entry)
	case optionModels:
		return p.models(entry.GetName())
	case optionOffered:
		return tui.Call(p.hub, func() (*providerv1.SetOfferedResponse, error) {
			return p.service.SetOffered(p.kit.Env.Ctx, &providerv1.SetOfferedRequest{Profile: p.profile, Provider: entry.GetName(), On: entry.GetHidden()})
		}, func(offered *providerv1.SetOfferedResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(offered.GetSaid()), p.load(entry.GetName()))
		})
	case optionAddress:
		return p.address(entry)
	case optionUsed:
		return p.pickUsed(entry)
	case optionOrder, optionExhausted:
		return p.pickChoice(entry, p.settingOf(entry, row.option))
	case optionUsage:
		return p.usage(entry.GetName())
	case optionAdvanced:
		return p.advanced(entry.GetName())
	case optionRemove:
		return p.remove(l, entry)
	}
	return nil
}

// Turns the use of a credential found outside the accounts on or off, the one control of that choice
func (p *providers) turnOutside(entry *providerv1.Provider, source string, on bool) tea.Cmd {
	return tui.Call(p.hub, func() (*providerv1.SetOutsideResponse, error) {
		return p.service.SetOutside(p.kit.Env.Ctx, &providerv1.SetOutsideRequest{Profile: p.profile, Provider: entry.GetName(), Source: credentials.SourceName(source), On: on})
	}, func(turned *providerv1.SetOutsideResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(turned.GetSaid()), p.load(entry.GetName()))
	})
}

// Asks the name the provider is shown by, which the key it is kept under follows; the page opens again under its new key
func (p *providers) renameProvider(entry *providerv1.Provider) tea.Cmd {
	field := cards.Field{Label: "Name", Value: entry.GetDisplayName(), Select: true, Help: "The name this provider is shown by, such as Work Claude."}
	return p.kit.Ask(p.hub, "Rename", field, "Rename", func(title string) tea.Cmd { return p.rename(entry, title) })
}

// Gives the provider the name it is shown by, the name of its kind for none, and opens its page again under the key that followed it
func (p *providers) rename(entry *providerv1.Provider, title string) tea.Cmd {
	return tui.Call(p.hub, func() (*providerv1.RenameResponse, error) {
		return p.service.Rename(p.kit.Env.Ctx, &providerv1.RenameRequest{Profile: p.profile, Provider: entry.GetName(), Title: title})
	}, func(renamed *providerv1.RenameResponse) tea.Cmd {
		moved := renamed.GetProvider()
		p.focus = moved.GetName()
		return tea.Batch(p.hub.PopTo(p.list), p.hub.Done("Renamed "+entry.GetDisplayName()+" to "+moved.GetDisplayName()+"."), p.options.Load(nil),
			p.load(moved.GetName()))
	})
}

// Asks before a provider that was given a name takes the name of its kind again, as resetting any other option puts its default back
func (p *providers) unname(l *cards.List, entry *providerv1.Provider) tea.Cmd {
	setting := p.settingOf(entry, optionName)
	if setting == nil || setting.GetSource() != configv1.Source_SOURCE_FILE {
		return nil
	}
	l.Confirm(cards.Question{Text: "Reset " + setting.GetTitle() + " to " + setting.GetDefaultShown() + "?", Verb: "Reset"},
		func() tea.Cmd { return p.rename(entry, "") })
	return nil
}

// Picks the default model of the provider from its own models
func (p *providers) pickDefault(entry *providerv1.Provider) tea.Cmd {
	key := entry.GetSection() + "." + providerconfig.DefaultModel
	ask := tui.Ask{Title: "Default model", Intro: []string{"The model " + entry.GetDisplayName() + " uses when no other is picked."}, Key: key,
		Values: map[string]string{key: p.options.Setting(key).GetValue()}, Profile: p.profile}
	ask.Done = func(values map[string]string) tea.Cmd { return p.save(entry, key, values[key]) }
	cmd, _ := p.kit.Choose(p.hub, providerconfig.PickModel, ask)
	return cmd
}

// Saves one setting of the provider, as YAML, through the one function the server has for it, and says what it is now
func (p *providers) save(entry *providerv1.Provider, key, value string) tea.Cmd {
	return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
		return p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: value})
	}, func(*configv1.SetResponse) tea.Cmd {
		return p.options.Load(func() tea.Cmd {
			said := ""
			if setting := p.options.Setting(key); setting != nil {
				said = setting.GetTitle() + " is now " + ansi.Strip(p.options.Cells(setting)[2]) + "."
			}
			return tea.Batch(p.hub.Done(said), p.load(entry.GetName()))
		})
	})
}

// Saves one value under a key of the provider, as YAML, through the server, and says what came of it in the words given
func (p *providers) store(entry *providerv1.Provider, key, value, said string) tea.Cmd {
	return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
		return p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: value})
	}, func(*configv1.SetResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(said), p.options.Load(nil), p.load(entry.GetName()))
	})
}

// Asks the address of the provider's server
func (p *providers) address(entry *providerv1.Provider) tea.Cmd {
	field := cards.Field{Label: "Address", Value: entry.GetBaseUrl(), Select: true, Check: checkAddress,
		Help: "Where requests to " + entry.GetDisplayName() + " go, such as http://localhost:8080/v1. It starts with http:// or https://."}
	return p.kit.Ask(p.hub, "Address", field, "Save", func(address string) tea.Cmd {
		return p.save(entry, entry.GetSection()+"."+providerconfig.BaseURL, strconv.Quote(strings.TrimRight(address, "/")))
	})
}

// Picks how the provider chooses among its accounts: every account in order, taking turns, or one account by name
func (p *providers) pickUsed(entry *providerv1.Provider) tea.Cmd {
	current := ""
	switch {
	case entry.GetRotate():
		current = providerconfig.Rotation
	case entry.GetPinned():
		current = entry.GetAccount()
	}
	choices := []tui.Choice{{ID: "", Title: "Every account in order", Detail: "The first that works, then the next when one runs out"},
		{ID: providerconfig.Rotation, Title: "Take turns", Detail: "The accounts that take turns, in the rotation order"}}
	for _, account := range entry.GetAccounts() {
		choices = append(choices, tui.Choice{ID: account.GetName(), Title: account.GetTitle(), Detail: "Every request goes to this account"})
	}
	return p.kit.PickFrom(p.hub, "Account used", []string{"Which account requests to " + entry.GetDisplayName() + " are made with."}, "choose", choices,
		current, func(choice tui.Choice) tea.Cmd {
			return tui.Call(p.hub, func() (*providerv1.UseAccountResponse, error) {
				return p.service.UseAccount(p.kit.Env.Ctx, &providerv1.UseAccountRequest{Profile: p.profile, Provider: entry.GetName(), Account: choice.ID})
			}, func(*providerv1.UseAccountResponse) tea.Cmd {
				return tea.Batch(p.hub.Done("Account used is now "+choice.Title+"."), p.load(entry.GetName()), p.options.Load(nil))
			})
		})
}

// Picks one of the values of a choice of the provider, saved through the server
func (p *providers) pickChoice(entry *providerv1.Provider, setting *configv1.Setting) tea.Cmd {
	if setting == nil {
		return nil
	}
	var choices []tui.Choice
	for _, option := range setting.GetOptions() {
		choices = append(choices, tui.Choice{ID: option.GetValue(), Title: option.GetTitle(), Detail: option.GetDescription()})
	}
	current := strings.Trim(setting.GetValue(), `"'`)
	return p.kit.PickFrom(p.hub, setting.GetTitle(), []string{setting.GetDescription()}, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
		return p.save(entry, setting.GetKey(), strconv.Quote(choice.ID))
	})
}

// Asks before an account is removed, then removes it and says what was cleared with it
func (p *providers) removeAccount(l *cards.List, entry *providerv1.Provider, account *providerv1.Account) tea.Cmd {
	text := "Remove the account " + account.GetTitle() + "? Every setting and session that names it is cleared."
	if len(entry.GetAccounts()) == 1 {
		text = "Remove the account " + account.GetTitle() + "? " + entry.GetDisplayName() + " then has no account of its own."
	}
	l.Confirm(cards.Question{Text: text, Verb: "Remove", Deletes: true}, func() tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.RemoveAccountResponse, error) {
			return p.service.RemoveAccount(p.kit.Env.Ctx, &providerv1.RemoveAccountRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName()})
		}, func(removed *providerv1.RemoveAccountResponse) tea.Cmd {
			return tea.Batch(p.closeView("providers:account:"+account.GetName()), p.hub.Done(removed.GetSaid()), p.load(entry.GetName()),
				p.options.Load(nil))
		})
	})
	return nil
}

// Closes the view with the key when the hub holds it
func (p *providers) closeView(key string) tea.Cmd {
	for _, view := range slices.Backward(p.views.Lists()) {
		if view.Key() == key && p.hub.Holds(view) {
			return p.hub.Close(view)
		}
	}
	return nil
}

// Name an account of a listed provider is shown by
func accountTitle(entry *providerv1.Provider, name string) string {
	for _, account := range entry.GetAccounts() {
		if account.GetName() == name {
			return account.GetTitle()
		}
	}
	return name
}
