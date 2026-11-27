package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Keymap context of the accounts of a provider
const loginsContext = "providers.logins"

// Keys of the accounts of a provider, moving an account, renewing its sign-in and keeping its key being rows of the account's own list
var loginKeys = []keys.Action{
	{ID: "providers.logins.rotate", Context: loginsContext, Label: "toggle rotation", Doc: "Put the highlighted account in the rotation, or take it out", Default: []string{"t"}},
	{ID: "providers.logins.reset", Context: loginsContext, Label: "clear limit", Doc: "Forget the limit that the highlighted account ran into", Default: []string{"R"}},
}

// Whether something holds the account back that forgetting its limit clears
func limited(account *providerv1.Account) bool {
	switch account.GetStatus() {
	case "limited", "dead", "needs_sign_in":
		return true
	}
	return account.GetResets() != nil
}

// Word on the button that connects the account again: signing in again for an account that signs in, connecting for any other
func connectVerb(account *providerv1.Account) string {
	if account.GetAuth() == "oauth" && connected(account.GetCredentialOrigin()) {
		return "sign in again"
	}
	return "connect"
}

// State of an account in a few words: whether it can be used, then whether it is used always or now and whether it takes turns
func (p *providers) accountState(provider *providerv1.Provider, account *providerv1.Account) string {
	t := p.kit.Env.Theme
	state := ""
	switch account.GetStatus() {
	case "limited":
		state = render.Warm("Limited", t)
		if resets := account.GetResets(); resets != nil {
			state += render.Warm(" until "+clock(resets.AsTime()), t)
		}
	case "needs_sign_in":
		state = render.Warm("Needs sign-in", t)
	case "dead":
		state = render.Bad("Refused", t)
	default:
		state = p.connection(account.GetCredentialOrigin(), account.GetCredentialSource())
	}
	if state == "" {
		state = render.Dim("Not connected", t)
	}
	var roles []string
	switch {
	case provider.GetPinned() && provider.GetAccount() == account.GetName():
		roles = append(roles, "always used")
	case account.GetActive():
		roles = append(roles, "in use")
	}
	if account.GetRotate() {
		roles = append(roles, "in rotation")
	}
	if len(roles) > 0 {
		state += render.Dim(", "+strings.Join(roles, ", "), t)
	}
	return state
}

// How the provider chooses among its accounts, in the words of its Account setting
func choice(p *providerv1.Provider) string {
	order := p.GetStrategy()
	for _, option := range providerconfig.Strategies() {
		if option.Value == order {
			order = strings.ToLower(option.Title)
		}
	}
	switch {
	case p.GetRotate():
		return "Take turns, " + order
	case p.GetPinned() && p.GetOnExhausted() == providerconfig.Rotate:
		return "Always " + accountTitle(p, p.GetAccount()) + ", then the others"
	case p.GetPinned():
		return "Always " + accountTitle(p, p.GetAccount())
	}
	return "Every account in order"
}

// How an account signs in, in words
func authWords(auth string) string {
	switch auth {
	case "oauth":
		return "An account that signs in through your browser."
	case "aws_sigv4":
		return "An account that uses your AWS credentials."
	case "gcp_oauth":
		return "An account that uses your Google Cloud credentials."
	}
	return "An account with an API key."
}

// Lines of the detail of an account: how it signs in and where its credential comes from, who it signs in as, what stands against it,
// its place, and the facts a person tells two accounts apart by
func (p *providers) detail(provider *providerv1.Provider, account *providerv1.Account) []string {
	t := p.kit.Env.Theme
	lines := []string{authWords(account.GetAuth())}
	source := account.GetCredentialSource()
	switch account.GetCredentialOrigin() {
	case providerv1.Origin_ORIGIN_SAVED:
		lines = append(lines, "icebath keeps what it signs in with.")
	case providerv1.Origin_ORIGIN_ENVIRONMENT:
		lines = append(lines, "Its key comes from "+source+".")
	case providerv1.Origin_ORIGIN_EXTERNAL:
		lines = append(lines, "It uses the sign-in that "+source+" keeps on this computer.")
	case providerv1.Origin_ORIGIN_UNNEEDED:
		lines = append(lines, "It needs nothing to sign in with.")
	default:
		lines = append(lines, render.Warm("Not connected yet. Connect it to use it.", t))
	}
	if identity := account.GetIdentity(); identity != "" {
		lines = append(lines, "Signed in as "+identity+".")
	}
	resets := ""
	if at := account.GetResets(); at != nil {
		resets = " until " + clock(at.AsTime()) + ", " + left(at.AsTime())
	}
	switch account.GetStatus() {
	case "limited":
		lines = append(lines, render.Warm("It reached a limit"+resets+". Another account is used meanwhile, when there is one.", t))
	case "needs_sign_in":
		lines = append(lines, render.Warm("Its sign-in ran out"+resets+". Sign in again to use it.", t))
	case "dead":
		lines = append(lines, render.Bad(provider.GetDisplayName()+" refused it for good. Connect it again with a new key or sign-in.", t))
	}
	if reason := account.GetReason(); reason != "" {
		lines = append(lines, render.Dim(provider.GetDisplayName()+" said: "+reason, t))
	}
	var facts []string
	if n := len(provider.GetAccounts()); n > 1 {
		place := slices.IndexFunc(provider.GetAccounts(), func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
		facts = append(facts, fmt.Sprintf("Tried %s of %d", ordinal(place+1), n))
	}
	if expires := account.GetExpires(); expires != nil {
		facts = append(facts, "Sign-in lasts until "+clock(expires.AsTime()))
	}
	if variable := account.GetKeyEnv(); variable != "" && account.GetCredentialOrigin() != providerv1.Origin_ORIGIN_ENVIRONMENT {
		facts = append(facts, "Reads its key from "+variable)
	}
	if fingerprint := account.GetFingerprint(); fingerprint != "" {
		facts = append(facts, "Fingerprint "+fingerprint)
	}
	if n := account.GetRequests(); n > 0 {
		facts = append(facts, strs.Thousands(n)+" "+plural(n, "request"))
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	return lines
}

// Noun for a count, in the plural past one
func plural(n int64, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// Place in an order as a number with its ending: 1st, 2nd, 3rd, 4th
func ordinal(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return strconv.Itoa(n) + "th"
	case n%10 == 1:
		return strconv.Itoa(n) + "st"
	case n%10 == 2:
		return strconv.Itoa(n) + "nd"
	case n%10 == 3:
		return strconv.Itoa(n) + "rd"
	}
	return strconv.Itoa(n) + "th"
}

// Moment as a person reads it: the time alone today, else the day and the time
func clock(at time.Time) string {
	local, now := at.Local(), time.Now()
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2") + " at " + local.Format("15:04")
}

// Time left before a moment in words: in about 2 hours, in 5 minutes, soon
func left(at time.Time) string {
	span := time.Until(at)
	switch {
	case span <= time.Minute:
		return "soon"
	case span >= 48*time.Hour:
		return "in about " + strconv.Itoa(int(span.Round(24*time.Hour)/(24*time.Hour))) + " days"
	case span >= 2*time.Hour:
		return "in about " + strconv.Itoa(int(span.Round(time.Hour)/time.Hour)) + " hours"
	case span >= time.Hour:
		return "in about an hour"
	}
	minutes := int((span + time.Minute - 1) / time.Minute)
	if minutes == 1 {
		return "in a minute"
	}
	return "in " + strconv.Itoa(minutes) + " minutes"
}

// List of text as one line of YAML
func encodeList(items []string) (string, error) {
	var node yaml.Node
	if err := node.Encode(append([]string{}, items...)); err != nil {
		return "", err
	}
	node.Style = yaml.FlowStyle
	out, err := yaml.Marshal(&node)
	return strings.TrimSpace(string(out)), err
}

// Puts the account in or out of the provider's rotation
func (p *providers) toggle(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil {
		return nil
	}
	on := !account.GetRotate()
	key := account.GetSection() + "." + providerconfig.InRotation
	return tui.Call(p.hub, func() (struct{}, error) { return struct{}{}, p.set(key, strconv.FormatBool(on)) }, func(struct{}) tea.Cmd {
		done := account.GetTitle() + " now takes turns with the other accounts in the rotation."
		if !on {
			done = account.GetTitle() + " is out of the rotation now."
		}
		if on && !pg.provider.GetRotate() {
			done += " Rotation takes effect once " + pg.provider.GetDisplayName() + " takes turns, under Rotation."
		}
		return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
	})
}

// Moves the account one place earlier or later in the order the provider's accounts are tried
func (p *providers) move(pg *page, account *providerv1.Account, step int) tea.Cmd {
	if account == nil {
		return nil
	}
	all := pg.provider.GetAccounts()
	place := slices.IndexFunc(all, func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
	to := place + step
	switch {
	case to < 0:
		return p.hub.Done(account.GetTitle() + " is tried first already.")
	case to >= len(all):
		return p.hub.Done(account.GetTitle() + " is tried last already.")
	}
	return tui.Call(p.hub, func() (*providerv1.MoveAccountResponse, error) {
		return p.service.MoveAccount(p.kit.Env.Ctx, &providerv1.MoveAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName(), Priority: int32(to)})
	}, func(moved *providerv1.MoveAccountResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(fmt.Sprintf("%s is now tried %s of %d.", account.GetTitle(), ordinal(int(moved.GetPriority())+1), len(all))),
			p.load(pg.provider.GetName()))
	})
}

// Forgets the limit the account ran into
func (p *providers) reset(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil {
		return nil
	}
	return tui.Call(p.hub, func() (*providerv1.ResetResponse, error) {
		return p.service.Reset(p.kit.Env.Ctx, &providerv1.ResetRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
	}, func(reset *providerv1.ResetResponse) tea.Cmd {
		done := "No limit held " + account.GetTitle() + " back."
		if reset.GetCleared() > 0 {
			done = "Forgot the limit of " + account.GetTitle() + ". It is tried again with the next request."
		}
		return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
	})
}

// Renews the account's sign-in now
func (p *providers) renew(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil || !account.GetRenewable() {
		return nil
	}
	p.hub.Busy("Renewing the sign-in of " + account.GetTitle() + "…")
	return tui.Call(p.hub, func() (*providerv1.RenewResponse, error) {
		return p.service.Renew(p.kit.Env.Ctx, &providerv1.RenewRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
	}, func(renewed *providerv1.RenewResponse) tea.Cmd {
		done := "Renewed the sign-in of " + account.GetTitle() + "."
		if expires := renewed.GetAccount().GetExpires(); expires != nil {
			done = "Renewed the sign-in of " + account.GetTitle() + ". It lasts until " + clock(expires.AsTime()) + "."
		}
		return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
	})
}

// Uses the account always from now on, or every account in order again for none
func (p *providers) use(pg *page, account *providerv1.Account) tea.Cmd {
	name, done := "", pg.provider.GetDisplayName()+" now tries every account in order."
	if account != nil {
		name, done = account.GetName(), pg.provider.GetDisplayName()+" now always uses "+account.GetTitle()+"."
	}
	return tui.Call(p.hub, func() (*providerv1.UseAccountResponse, error) {
		return p.service.UseAccount(p.kit.Env.Ctx, &providerv1.UseAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: name})
	}, func(*providerv1.UseAccountResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
	})
}

// Asks before the key an account reads from a variable is kept in icebath, then keeps it
func (p *providers) keep(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil || account.GetKeyEnv() == "" {
		return nil
	}
	pg.list.Confirm("Keep the key from "+account.GetKeyEnv()+" in icebath? "+account.GetTitle()+" stops reading the variable.", func() tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.KeepKeyResponse, error) {
			return p.service.KeepKey(p.kit.Env.Ctx, &providerv1.KeepKeyRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
		}, func(*providerv1.KeepKeyResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(account.GetTitle()+" now keeps its key in icebath."), p.load(pg.provider.GetName()))
		})
	})
	return nil
}

// Gives the account another name, asked for over the page: the name it is shown by, and the name it is kept under when that is free
func (p *providers) rename(pg *page, account *providerv1.Account) tea.Cmd {
	field := cards.Field{Label: "Name", Value: account.GetTitle(), Help: "The name you know this account by, such as Work or Personal.",
		Check: func(value string) string {
			if slug(value) == "" {
				return "Use at least one letter or digit."
			}
			return ""
		}}
	return p.kit.Ask(p.hub, "Rename", field, "Rename", func(name string) tea.Cmd {
		if name == account.GetTitle() {
			return nil
		}
		return tui.Call(p.hub, func() (struct{}, error) {
			if err := p.set(account.GetSection()+"."+providerconfig.Title, strconv.Quote(name)); err != nil {
				return struct{}{}, err
			}
			kept := slug(name)
			if kept == account.GetName() || slices.ContainsFunc(pg.provider.GetAccounts(), func(other *providerv1.Account) bool { return other.GetName() == kept }) {
				return struct{}{}, nil
			}
			_, err := p.service.RenameAccount(p.kit.Env.Ctx, &providerv1.RenameAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(),
				Account: account.GetName(), Name: kept})
			return struct{}{}, err
		}, func(struct{}) tea.Cmd {
			return tea.Batch(p.hub.Done("Renamed "+account.GetTitle()+" to "+name+"."), p.load(pg.provider.GetName()))
		})
	})
}

// Options of a setting of every provider by its key, the words the rotation form shares with the settings screen
func options(key string) []config.Option {
	for _, setting := range providerconfig.Rotations() {
		if setting.Key == key && setting.Options != nil {
			return setting.Options()
		}
	}
	return nil
}

// Changes how the provider chooses among its accounts: every account in order, taking turns or always one; the order turns are taken in;
// and what happens when the account in use runs out
func (p *providers) rotation(pg *page) tea.Cmd {
	provider := pg.provider
	current := ""
	switch {
	case provider.GetRotate():
		current = providerconfig.Rotation
	case provider.GetPinned():
		current = provider.GetAccount()
	}
	uses := []cards.Choice{{Value: "", Label: "Every account in order", Meaning: "The first that works, then the next when one runs out"},
		{Value: providerconfig.Rotation, Label: "Take turns", Meaning: "The accounts in rotation, in the rotation order"}}
	for _, account := range provider.GetAccounts() {
		uses = append(uses, cards.Choice{Value: account.GetName(), Label: "Always " + account.GetTitle(), Meaning: "Every request goes to this account"})
	}
	var orders, exhausted []cards.Choice
	for _, option := range options(providerconfig.Strategy) {
		orders = append(orders, cards.Choice{Value: option.Value, Label: option.Title, Meaning: option.About})
	}
	for _, option := range options(providerconfig.OnExhausted) {
		exhausted = append(exhausted, cards.Choice{Value: option.Value, Label: option.Title, Meaning: option.About})
	}
	name := provider.GetDisplayName()
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Rotation",
		Intro:  []string{"How " + name + " picks among its accounts."},
		Submit: "Save",
		Fields: []cards.Field{
			{ID: "use", Label: "Account", Kind: cards.FieldChoice, Value: current, Choices: uses},
			{ID: "strategy", Label: "Rotation order", Kind: cards.FieldChoice, Value: provider.GetStrategy(), Choices: orders,
				Help: "Used while the accounts take turns."},
			{ID: "exhausted", Label: "When accounts run out", Kind: cards.FieldChoice, Value: provider.GetOnExhausted(), Choices: exhausted,
				Help: "Used while one account is always used."},
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			form.SetBusy(true)
			return tui.Async(func() func() tea.Cmd {
				err := func() error {
					if use := values["use"]; use != current {
						if _, err := p.service.UseAccount(p.kit.Env.Ctx, &providerv1.UseAccountRequest{Profile: p.profile, Provider: provider.GetName(), Account: use}); err != nil {
							return err
						}
					}
					if err := p.set(provider.GetSection()+"."+providerconfig.Strategy, strconv.Quote(values["strategy"])); err != nil {
						return err
					}
					return p.set(provider.GetSection()+"."+providerconfig.OnExhausted, strconv.Quote(values["exhausted"]))
				}()
				return func() tea.Cmd {
					form.SetBusy(false)
					if err != nil {
						return p.hub.Fail(tui.Reason(err))
					}
					done := name + " now tries every account in order."
					switch use := values["use"]; use {
					case "":
					case providerconfig.Rotation:
						done = name + " now takes turns between the accounts in rotation."
					default:
						done = name + " now always uses " + accountTitle(provider, use) + "."
					}
					return tea.Batch(p.hub.Close(form), p.hub.Done(done), p.load(provider.GetName()))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Links the account to an environment variable it reads its key from, the key kept outside icebath
func (p *providers) linked(target Target) tea.Cmd {
	field := cards.Field{ID: "variable", Label: "Variable", Placeholder: firstVariable(target.Provider.GetKeyEnv()), Required: true,
		Help: "The environment variable that holds the key, set in your shell or your keys file.",
		Check: func(value string) string {
			if !config.EnvName(strings.TrimSpace(value)) {
				return "A variable name has only letters, digits and underscores, and starts with a letter."
			}
			return ""
		}}
	was := connected(target.Provider.GetCredentialOrigin())
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Key from an environment variable",
		Intro:  []string{"Name the variable that holds the API key of your " + target.Provider.GetDisplayName() + " account" + named(target) + "."},
		Fields: []cards.Field{field},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			variable := strings.TrimSpace(values["variable"])
			form.SetBusy(true)
			return tui.Async(func() func() tea.Cmd {
				err := Store(p.kit.Env.Ctx, p.settings, target, map[string]any{providerconfig.KeyEnv: variable, providerconfig.APIKeyField: nil})
				now := p.find(target.Provider.GetName())
				return func() tea.Cmd {
					form.SetBusy(false)
					if err != nil {
						form.SetError("variable", tui.Reason(err))
						return nil
					}
					done := target.Label() + " now reads its key from " + variable + "."
					if now != nil && !connected(now.GetCredentialOrigin()) {
						done += " " + variable + " holds no key yet, so set it and open icebath again."
					} else if !was {
						done = connectedNote(target, was, false)
					}
					return tea.Batch(p.hub.Close(form), p.hub.Done(done), p.load(target.Provider.GetName()))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// First of the names, the placeholder of a variable field
func firstVariable(names []string) string {
	if len(names) > 0 {
		return names[0]
	}
	return "PROVIDER_API_KEY"
}
