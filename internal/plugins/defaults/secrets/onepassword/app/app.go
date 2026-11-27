// 1Password on the secret stores screen: set it up, fill variables from its references, rotate its token, check it, turn it off
package app

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

const about = "Variables take their values from op:// references, read through the 1Password op program each time icebath starts."

type stores struct{}

func Register(registry *secretsapp.Registry) {
	registry.Register(stores{})
}

func (stores) Stores(conn *connect.Client) []secretsapp.Store {
	o := &onepassword{service: onepasswordv1connect.NewOnePasswordServiceClient(conn)}
	return []secretsapp.Store{{Source: onepasswordconfig.Name, Rows: o.rows,
		Adds: []secretsapp.Action{{Label: "Fill a variable", Does: "a variable takes its value from a 1Password reference", Run: o.mapReference}}}}
}

type onepassword struct {
	service onepasswordv1connect.OnePasswordServiceClient
}

func (o *onepassword) rows(ctx context.Context) ([]secretsapp.Row, error) {
	state, err := o.service.State(ctx, &onepasswordv1.StateRequest{})
	if err != nil {
		return nil, err
	}
	detail := []string{"1Password · " + secretsapp.State(state.GetEnabled()), about}
	if state.GetTokenSet() {
		detail = append(detail, state.GetTokenEnv()+" holds a token.")
	} else {
		detail = append(detail, state.GetTokenEnv()+" holds no token.")
	}
	if state.GetAccount() != "" {
		detail = append(detail, state.GetAccount())
	}
	if state.GetBinary() == "" {
		detail = append(detail, "Its helper program is not installed.")
	} else {
		detail = append(detail, strings.TrimSpace(state.GetBinary()+" "+state.GetVersion()))
	}
	open := []secretsapp.Action{
		{Label: "Set up", Does: "account and token; the op program must be installed", Run: func(s *secretsapp.Screen) tea.Cmd { return o.setup(s, state) }},
		{Label: "Fill a variable", Does: "a variable takes its value from a 1Password reference", Run: o.mapReference},
	}
	if state.GetEnabled() {
		open = append(open,
			secretsapp.Action{Label: "New token", Does: "replace the token once the store accepts the new one", Run: o.token},
			secretsapp.Action{Label: "Check now", Does: "ask the store now and show what each variable gets", Run: o.check},
			secretsapp.Action{Label: "Turn off", Does: "stop pulling from it; its token and settings stay", Run: o.off})
	}
	rows := []secretsapp.Row{{Cells: [3]string{"1Password", secretsapp.State(state.GetEnabled()), state.GetAccount()}, Detail: detail, Open: open}}
	for _, ref := range state.GetReferences() {
		env := ref.GetEnv()
		rows = append(rows, secretsapp.Row{Cells: [3]string{env, "", ref.GetReference()}, Detail: []string{env, ref.GetReference()},
			Confirm: "Stop filling " + env + " from 1Password?",
			Remove: func(s *secretsapp.Screen) tea.Cmd {
				return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
					_, err := o.service.Unmap(ctx, &onepasswordv1.UnmapRequest{Env: env})
					return secretsapp.Outcome{Note: "Removed " + env + "."}, err
				})
			}})
	}
	return rows, nil
}

// A form of the account, a service account token, the variable that holds it and where the op program lives
func (o *onepassword) setup(s *secretsapp.Screen, state *onepasswordv1.StateResponse) tea.Cmd {
	token := cards.Field{ID: "token", Label: "token", Kind: cards.FieldSecret, Help: "A service account token; leave empty to use the session op is signed in to."}
	if state.GetTokenSet() {
		token.Placeholder = "leave empty to keep the one set"
	}
	spec := cards.FormSpec{Title: "1Password", Intro: []string{about}, Submit: "Set up", Fields: []cards.Field{
		{ID: "account", Label: "account", Help: "Account shorthand or sign in address, when op knows more than one.", Value: state.GetAccount()}, token,
		{ID: "token_env", Label: "variable holding the token", Value: state.GetTokenEnv(), More: true},
		{ID: "binary", Label: "path of op", Help: "Only when op is not on the path.", Value: state.GetBinaryPath(), More: true},
	}}
	return s.Form(spec, func(values map[string]string) tea.Cmd {
		request := &onepasswordv1.SetupRequest{Account: strings.TrimSpace(values["account"]), Token: strings.TrimSpace(values["token"]),
			TokenEnv: strings.TrimSpace(values["token_env"]), BinaryPath: strings.TrimSpace(values["binary"])}
		return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
			set, err := o.service.Setup(ctx, request)
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case set.GetBinaryMissing():
				return secretsapp.Outcome{}, errors.New("the 1Password op program was not found; install it from " + onepasswordconfig.Docs)
			case set.GetIdentity() == "":
				return secretsapp.Outcome{Note: "1Password is on, but op is not signed in and no token is set."}, nil
			}
			return secretsapp.Outcome{Note: "1Password is on, signed in as " + set.GetIdentity() + "."}, nil
		})
	})
}

// Form binding a variable to the 1Password reference it takes its value from
func (o *onepassword) mapReference(s *secretsapp.Screen) tea.Cmd {
	spec := cards.FormSpec{Title: "Fill a variable", Fields: []cards.Field{
		{ID: "env", Label: "variable", Placeholder: "OPENAI_API_KEY", Required: true},
		{ID: "reference", Label: "reference", Placeholder: "op://Private/OpenAI/api key", Required: true},
	}}
	return s.Form(spec, func(values map[string]string) tea.Cmd {
		env := strings.TrimSpace(values["env"])
		return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
			mapped, err := o.service.Map(ctx, &onepasswordv1.MapRequest{Env: env, Reference: strings.TrimSpace(values["reference"])})
			if err != nil {
				return secretsapp.Outcome{}, err
			}
			if mapped.GetReference() == "" {
				return secretsapp.Outcome{}, errors.New(strings.Join(mapped.GetWarnings(), "; "))
			}
			return secretsapp.Outcome{Note: "Saved " + env + "."}, nil
		})
	})
}

func (o *onepassword) token(s *secretsapp.Screen) tea.Cmd {
	return s.Kit.Ask(s.Hub, "1Password", cards.Field{Label: "token", Kind: cards.FieldSecret}, "Keep", func(token string) tea.Cmd {
		if token == "" {
			return nil
		}
		return s.Change("Checking the token…", func(ctx context.Context) (secretsapp.Outcome, error) {
			rotated, err := o.service.Token(ctx, &onepasswordv1.TokenRequest{Token: token, Verify: true})
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case rotated.GetBinaryMissing():
				return secretsapp.Outcome{}, errors.New("the 1Password op program was not found; install it from " + onepasswordconfig.Docs)
			case rotated.GetRejected():
				return secretsapp.Outcome{}, errors.New("1Password did not accept the token; nothing was changed")
			}
			return secretsapp.Outcome{Note: rotated.GetTokenEnv() + " holds the new token."}, nil
		})
	})
}

func (o *onepassword) check(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Resolving the references…", func(ctx context.Context) (secretsapp.Outcome, error) {
		synced, err := o.service.Sync(ctx, &onepasswordv1.SyncRequest{})
		if err != nil {
			return secretsapp.Outcome{}, err
		}
		rows := make([][2]string, len(synced.GetRows()))
		for i, row := range synced.GetRows() {
			rows[i] = [2]string{row.GetName(), row.GetAction()}
		}
		return secretsapp.Outcome{Title: "1Password", Lines: s.Preview(rows, synced.GetWarnings())}, nil
	})
}

func (o *onepassword) off(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
		_, err := o.service.Disable(ctx, &onepasswordv1.DisableRequest{})
		return secretsapp.Outcome{Note: "1Password is off."}, err
	})
}
