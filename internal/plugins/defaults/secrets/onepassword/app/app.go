// 1Password on the page of Secret stores: setting it up as a job in steps, how it signs in then the token or the account, replacing its
// token, and the picker of its accounts that settings naming one use
package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Where the token of a service account is made
const tokenHelp = "A service account token, made at 1Password.com under Developer › Service accounts."

// Ids of the ways 1Password signs in, as the step that asks it holds them
const (
	byToken   = "token"
	byAccount = "account"
)

type stores struct{}

func Register(registry *secretsapp.Registry) {
	registry.Register(stores{})
}

func (stores) Stores(conn *connect.Client) []secretsapp.Store {
	o := &onepassword{service: onepasswordv1connect.NewOnePasswordServiceClient(conn)}
	return []secretsapp.Store{{Source: onepasswordconfig.Name, Setup: o.setup, Token: o.token,
		Pickers: map[string]tui.Picker{onepasswordconfig.Accounts: {Open: o.pickAccount}}}}
}

// Picker of a 1Password account: what is in effect with none first, then the accounts op knows on this computer, read as it opens
func (o *onepassword) pickAccount(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	hub.Busy("Reading the accounts of op…")
	return tui.Call(hub, func() (*onepasswordv1.AccountsResponse, error) {
		return o.service.Accounts(kit.Env.Ctx, &onepasswordv1.AccountsRequest{Profile: ask.Profile})
	}, func(listed *onepasswordv1.AccountsResponse) tea.Cmd {
		switch {
		case listed.GetBinaryMissing():
			return hub.Fail("op, the 1Password command line tool, is not installed, so its accounts can't be listed. Get it at " + onepasswordconfig.Docs + ".")
		case listed.GetError() != "":
			return hub.Fail(listed.GetError())
		}
		hub.Quiet()
		choices := slices.Clone(ask.First)
		for _, account := range listed.GetAccounts() {
			choices = append(choices, tui.Choice{ID: account.GetAddress(), Title: account.GetEmail(), Detail: account.GetAddress()})
		}
		var held string
		_ = yaml.Unmarshal([]byte(ask.Values[ask.Key]), &held)
		return kit.PickFrom(hub, ask.Title, ask.Intro, "choose", choices, held, func(choice tui.Choice) tea.Cmd {
			if choice.ID == "" {
				return ask.Done(map[string]string{ask.Key: "null"})
			}
			return ask.Done(map[string]string{ask.Key: strconv.Quote(choice.ID)})
		})
	})
}

type onepassword struct {
	service onepasswordv1connect.OnePasswordServiceClient
}

// Sets 1Password up in steps: how it signs in, then the token, or the account op signs in to, picked from those op knows. With a token kept
// already only the token is asked, empty keeping it. Nothing is kept until op signs in
func (o *onepassword) setup(p *secretsapp.Page, source *secretsv1.Source) tea.Cmd {
	f := p.Kit.Flow(p.Hub, "Set up 1Password")
	if source.GetTokenSet() {
		return o.tokenStep(p, f, source)
	}
	ways := []tui.Choice{
		{ID: byToken, Title: "A service account token", Detail: "Made at 1Password.com and kept in your keys file"},
		{ID: byAccount, Title: "Your account on this computer", Detail: "The sign-in of the 1Password app, through op"},
	}
	return f.Pick("How 1Password signs in.", ways, "", func(choice tui.Choice) tea.Cmd {
		if choice.ID == byToken {
			return o.tokenStep(p, f, source)
		}
		return o.accountStep(p, f)
	})
}

// Asks the service account token, empty keeping the one kept when there is one
func (o *onepassword) tokenStep(p *secretsapp.Page, f *tui.Flow, source *secretsv1.Source) tea.Cmd {
	field := cards.Field{ID: "token", Label: "Token", Kind: cards.FieldSecret, Required: !source.GetTokenSet(),
		Help: tokenHelp + " It is kept in your keys file as " + source.GetTokenEnv() + "."}
	if source.GetTokenSet() {
		field.Help += " Leave it empty to keep the one kept now."
	}
	return f.Ask("", []cards.Field{field}, "Set up", func(values map[string]string) tea.Cmd {
		return o.run(p, f, &onepasswordv1.SetupRequest{Token: strings.TrimSpace(values["token"])}, true)
	})
}

// Reads the accounts op knows, then asks which one it signs in to, the one it uses by default first
func (o *onepassword) accountStep(p *secretsapp.Page, f *tui.Flow) tea.Cmd {
	p.Hub.Busy("Reading the accounts of op…")
	return tui.Call(p.Hub, func() (*onepasswordv1.AccountsResponse, error) {
		return o.service.Accounts(p.Kit.Env.Ctx, &onepasswordv1.AccountsRequest{Profile: p.Profile})
	}, func(listed *onepasswordv1.AccountsResponse) tea.Cmd {
		switch {
		case listed.GetBinaryMissing():
			return p.Hub.Fail("op, the 1Password command line tool, is not installed. Get it at " + onepasswordconfig.Docs + ", then set up again.")
		case listed.GetError() != "":
			return p.Hub.Fail(listed.GetError())
		case len(listed.GetAccounts()) == 0:
			return p.Hub.Fail("op knows no 1Password account on this computer yet. Add one with op account add, or set up with a token.")
		}
		p.Hub.Quiet()
		choices := []tui.Choice{{ID: "", Title: "The account op uses by default", Detail: "The one op signs in to when none is named"}}
		for _, account := range listed.GetAccounts() {
			choices = append(choices, tui.Choice{ID: account.GetAddress(), Title: account.GetEmail(), Detail: account.GetAddress()})
		}
		return f.PickVerb("Which account op signs in to.", choices, "", "Set up", func(choice tui.Choice) tea.Cmd {
			return o.run(p, f, &onepasswordv1.SetupRequest{Account: choice.ID}, false)
		})
	})
}

// Sets 1Password up as the steps asked, ending the job once op signed in; a refusal is said and the job stays on its last step
func (o *onepassword) run(p *secretsapp.Page, f *tui.Flow, request *onepasswordv1.SetupRequest, token bool) tea.Cmd {
	request.Profile = p.Profile
	p.Hub.Busy("Setting up 1Password…")
	return tui.Call(p.Hub, func() (*onepasswordv1.SetupResponse, error) {
		return o.service.Setup(p.Kit.Env.Ctx, request)
	}, func(set *onepasswordv1.SetupResponse) tea.Cmd {
		switch {
		case set.GetBinaryMissing():
			return p.Hub.Fail("op, the 1Password command line tool, is not installed. Get it at " + onepasswordconfig.Docs + ", then set up again.")
		case set.GetNotSignedIn() && token:
			return p.Hub.Fail("1Password refused the token, so nothing changed.")
		case set.GetNotSignedIn():
			return p.Hub.Fail("op is not signed in to that account. Turn on its sign-in in the 1Password app, then set up again.")
		}
		return tea.Batch(f.End(), p.Done("1Password is on, signed in as "+set.GetIdentity()+"."))
	})
}

// Asks for a new service account token, kept once 1Password takes it
func (o *onepassword) token(p *secretsapp.Page, source *secretsv1.Source) tea.Cmd {
	field := cards.Field{Label: "Token", Kind: cards.FieldSecret,
		Help: tokenHelp + " It takes the place of the one in " + source.GetTokenEnv() + " once 1Password takes it."}
	return p.Kit.Ask(p.Hub, "Replace the token", field, "Replace", func(token string) tea.Cmd {
		p.Hub.Busy("Checking the token with 1Password…")
		return tui.Call(p.Hub, func() (*onepasswordv1.TokenResponse, error) {
			return o.service.Token(p.Kit.Env.Ctx, &onepasswordv1.TokenRequest{Profile: p.Profile, Token: token, Verify: true})
		}, func(rotated *onepasswordv1.TokenResponse) tea.Cmd {
			switch {
			case rotated.GetBinaryMissing():
				return p.Hub.Fail("op is not installed, so the token can't be checked and nothing changed.")
			case rotated.GetRejected():
				return p.Hub.Fail("1Password refused the token, so nothing changed.")
			case rotated.GetDisabled():
				return p.Done("Kept the new token. 1Password is off, so it is used once you turn it on.")
			}
			return p.Done("1Password uses the new token, signed in as " + rotated.GetIdentity() + ".")
		})
	})
}
