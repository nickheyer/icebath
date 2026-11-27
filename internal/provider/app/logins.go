package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Keymap context of the accounts of a provider
const loginsContext = "providers.logins"

var loginKeys = []keys.Action{
	{ID: "providers.logins.rotate", Context: loginsContext, Label: "rotation", Doc: "put the highlighted account in or out of the provider's rotation", Default: []string{"t"}},
	{ID: "providers.logins.up", Context: loginsContext, Label: "earlier", Doc: "try the highlighted account before the one above it", Default: []string{"K"}},
	{ID: "providers.logins.down", Context: loginsContext, Label: "later", Doc: "try the highlighted account after the one below it", Default: []string{"J"}},
	{ID: "providers.logins.reset", Context: loginsContext, Label: "clear limit", Doc: "forget the limit the highlighted account ran into", Default: []string{"R"}},
	{ID: "providers.logins.renew", Context: loginsContext, Label: "renew", Doc: "renew the sign-in of the highlighted account now", Default: []string{"N"}},
	{ID: "providers.logins.keep", Context: loginsContext, Label: "keep in icebath", Doc: "move a key read from an environment variable into icebath's own store", Default: []string{"c"}},
}

// Marks of an account: in use, the provider's default, in rotation
func marks(p *providerv1.Provider, account *providerv1.Account, t theme.Theme) string {
	mark := ""
	if account.GetActive() {
		mark += "●"
	}
	if p.GetPinned() && p.GetAccount() == account.GetName() {
		mark += "★"
	}
	if account.GetRotate() {
		mark += "↻"
	}
	return render.Dim(mark, t)
}

// What stands against an account in a few words, empty while it is ready
func (p *providers) state(account *providerv1.Account) string {
	t := p.kit.Env.Theme
	until := ""
	if resets := account.GetResets(); resets != nil {
		until = " until " + resets.AsTime().Local().Format("15:04") + " (" + left(resets.AsTime()) + ")"
	}
	switch account.GetStatus() {
	case "limited":
		return render.Warm("Limited"+until, t)
	case "needs_sign_in":
		return render.Warm("Needs sign in"+until, t)
	case "dead":
		return render.Warm("Refused for good", t)
	}
	return ""
}

// How the provider chooses among its accounts, in a few words
func choice(p *providerv1.Provider) string {
	order := strings.ReplaceAll(p.GetStrategy(), "_", " ")
	switch {
	case p.GetRotate():
		return "Rotation, " + order
	case p.GetPinned() && p.GetOnExhausted() == providerconfig.Rotate:
		return "Held to " + p.GetAccount() + ", the others once it runs out"
	case p.GetPinned():
		return "Held to " + p.GetAccount()
	}
	return "Every account in order"
}

// Lines of the detail of an account: where its credential comes from, what stands against it, its place and who it signs in as
func (p *providers) detail(provider *providerv1.Provider, account *providerv1.Account) []string {
	t := p.kit.Env.Theme
	lines := []string{account.GetTitle() + " · " + provider.GetDisplayName()}
	if from := strings.TrimSpace(p.credential(account.GetCredentialOrigin(), account.GetCredentialSource())); from != "" {
		lines = append(lines, from)
	}
	if state := p.state(account); state != "" {
		lines = append(lines, state)
	}
	if reason := account.GetReason(); reason != "" {
		lines = append(lines, render.Dim(reason, t))
	}
	if n := len(provider.GetAccounts()); n > 1 {
		place := slices.IndexFunc(provider.GetAccounts(), func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
		lines = append(lines, fmt.Sprintf("Tried %d of %d", place+1, n))
	}
	if identity := account.GetIdentity(); identity != "" {
		lines = append(lines, "Signed in as "+identity)
	}
	if expires := account.GetExpires(); expires != nil {
		lines = append(lines, render.Dim("Sign-in expires "+expires.AsTime().Local().Format("Jan 2 15:04"), t))
	}
	if variable := account.GetKeyEnv(); variable != "" {
		lines = append(lines, render.Dim("Key read from "+variable, t))
	}
	if fingerprint := account.GetFingerprint(); fingerprint != "" {
		lines = append(lines, render.Dim("Secret ends …"+fingerprint, t))
	}
	if n := account.GetRequests(); n > 0 {
		lines = append(lines, render.Dim(render.Count(int(n), "request"), t))
	}
	return lines
}

// Sets one setting of the profile from YAML text
func (p *providers) set(key, value string) error {
	_, err := p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: value})
	return err
}

// Puts the account in or out of the provider's rotation
func (p *providers) toggle(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil {
		return nil
	}
	on := !account.GetRotate()
	key := pg.provider.GetSection() + "." + providerconfig.Accounts + "." + account.GetName() + "." + providerconfig.InRotation
	return tui.Call(p.hub, func() (struct{}, error) { return struct{}{}, p.set(key, strconv.FormatBool(on)) }, func(struct{}) tea.Cmd {
		done := account.GetTitle() + " takes turns with the accounts in rotation"
		if !on {
			done = account.GetTitle() + " left the rotation"
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
		return p.hub.Done(account.GetTitle() + " is tried first already")
	case to >= len(all):
		return p.hub.Done(account.GetTitle() + " is tried last already")
	}
	return tui.Call(p.hub, func() (*providerv1.MoveAccountResponse, error) {
		return p.service.MoveAccount(p.kit.Env.Ctx, &providerv1.MoveAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName(), Priority: int32(to)})
	}, func(moved *providerv1.MoveAccountResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(fmt.Sprintf("%s is tried %d of %d", account.GetTitle(), moved.GetPriority()+1, len(all))), p.load(pg.provider.GetName()))
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
		done := "Nothing held " + account.GetTitle() + " back"
		if reset.GetCleared() > 0 {
			done = "Forgot the limit of " + account.GetTitle()
		}
		return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
	})
}

// Renews the account's sign-in now
func (p *providers) renew(pg *page, account *providerv1.Account) tea.Cmd {
	if account == nil {
		return nil
	}
	return tui.Call(p.hub, func() (*providerv1.RenewResponse, error) {
		return p.service.Renew(p.kit.Env.Ctx, &providerv1.RenewRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
	}, func(*providerv1.RenewResponse) tea.Cmd {
		return tea.Batch(p.hub.Done("Renewed the sign-in of "+account.GetTitle()), p.load(pg.provider.GetName()))
	})
}

// Gives the account another name, asked for over the page
func (p *providers) rename(pg *page, account *providerv1.Account) tea.Cmd {
	field := cards.Field{Label: "Name", Value: account.GetName(), Help: "Lower case letters, digits, dashes or underscores"}
	return p.kit.Ask(p.hub, "Rename "+account.GetTitle(), field, "Rename", func(name string) tea.Cmd {
		if name == account.GetName() {
			return nil
		}
		return tui.Call(p.hub, func() (*providerv1.RenameAccountResponse, error) {
			return p.service.RenameAccount(p.kit.Env.Ctx, &providerv1.RenameAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName(), Name: name})
		}, func(renamed *providerv1.RenameAccountResponse) tea.Cmd {
			return tea.Batch(p.hub.Done("Renamed to "+renamed.GetAccount().GetName()), p.load(pg.provider.GetName()))
		})
	})
}

// Changes how the provider chooses among its accounts: one held to, the rotation, or every account in order; the rotation's order; and
// what an account held to does once it runs out
func (p *providers) rotation(pg *page) tea.Cmd {
	provider := pg.provider
	current := ""
	switch {
	case provider.GetRotate():
		current = providerconfig.Rotation
	case provider.GetPinned():
		current = provider.GetAccount()
	}
	uses := []cards.Choice{{Value: "", Label: "Every account in order", Meaning: "The first that works, the next once it runs out"},
		{Value: providerconfig.Rotation, Label: "Rotation", Meaning: "Takes turns between the accounts marked for rotation"}}
	for _, account := range provider.GetAccounts() {
		uses = append(uses, cards.Choice{Value: account.GetName(), Label: account.GetTitle(), Meaning: "Held to this account"})
	}
	var orders []cards.Choice
	for _, option := range providerconfig.Strategies() {
		orders = append(orders, cards.Choice{Value: option.Value, Label: option.Title, Meaning: option.About})
	}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Rotation of " + provider.GetDisplayName(),
		Submit: "Save",
		Fields: []cards.Field{
			{ID: "use", Label: "Use", Kind: cards.FieldChoice, Value: current, Choices: uses},
			{ID: "strategy", Label: "Rotation order", Kind: cards.FieldChoice, Value: provider.GetStrategy(), Choices: orders},
			{ID: "exhausted", Label: "When it runs out", Kind: cards.FieldChoice, Value: provider.GetOnExhausted(), Choices: []cards.Choice{
				{Value: providerconfig.Fail, Label: "Stop", Meaning: "Stop and say so"},
				{Value: providerconfig.Rotate, Label: "Rotate", Meaning: "Move on to the other accounts"}}},
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tui.Call(p.hub, func() (struct{}, error) {
				if use := values["use"]; use != current {
					if _, err := p.service.UseAccount(p.kit.Env.Ctx, &providerv1.UseAccountRequest{Profile: p.profile, Provider: provider.GetName(), Account: use}); err != nil {
						return struct{}{}, err
					}
				}
				if err := p.set(provider.GetSection()+"."+providerconfig.Strategy, strconv.Quote(values["strategy"])); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, p.set(provider.GetSection()+"."+providerconfig.OnExhausted, strconv.Quote(values["exhausted"]))
			}, func(struct{}) tea.Cmd {
				return tea.Batch(p.hub.Close(form), p.hub.Done("Saved the rotation of "+provider.GetDisplayName()), p.load(provider.GetName()))
			})
		},
	})
	return p.hub.Push(form)
}

// Links the account to an environment variable it reads its key from, the key kept outside icebath
func (p *providers) linked(target Target) tea.Cmd {
	field := cards.Field{ID: "variable", Label: "Variable", Placeholder: firstName(target.Provider.GetKeyEnv()), Required: true,
		Help: "Environment variable of this computer or the profile's .env that holds the key",
		Check: func(value string) string {
			if strings.ContainsAny(strings.TrimSpace(value), " =") {
				return "A variable's name has no spaces or ="
			}
			return ""
		}}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Connect " + target.Account.GetTitle(),
		Fields: []cards.Field{field},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			variable := strings.TrimSpace(values["variable"])
			return tui.Call(p.hub, func() (struct{}, error) {
				return struct{}{}, Store(p.kit.Env.Ctx, p.settings, target, map[string]any{providerconfig.KeyEnv: variable, providerconfig.APIKeyField: nil})
			}, func(struct{}) tea.Cmd {
				return tea.Batch(p.hub.Close(form), p.hub.Done("Connected "+target.Account.GetTitle()+" to "+variable), p.load(target.Provider.GetName()))
			})
		},
	})
	return p.hub.Push(form)
}

// First of the names, the placeholder of a variable field
func firstName(names []string) string {
	if len(names) > 0 {
		return names[0]
	}
	return "PROVIDER_API_KEY"
}

// Time left before a moment, in hours or minutes
func left(at time.Time) string {
	left := time.Until(at)
	if left >= time.Hour {
		return fmt.Sprintf("~%dh", int((left+time.Hour-1)/time.Hour))
	}
	return fmt.Sprintf("~%d min", int((left+time.Minute-1)/time.Minute))
}
