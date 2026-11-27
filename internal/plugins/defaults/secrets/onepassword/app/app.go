// 1Password on the secret stores screen: set it up, fill variables from its references, give it a new token, fetch from it now, turn it
// on or off
package app

import (
	"context"
	"errors"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// What 1Password does on the screen, first in its detail
const about = "Fills variables from op:// references through the 1Password program op each time icebath starts."

// Heading of the variables 1Password fills
const filled = "Filled from 1Password"

// Where the token of a service account is made
const tokenHelp = "A service account token, made at 1Password.com under Developer › Service accounts."

type stores struct{}

func Register(registry *secretsapp.Registry) {
	registry.Register(stores{})
}

func (stores) Stores(conn *connect.Client) []secretsapp.Store {
	o := &onepassword{service: onepasswordv1connect.NewOnePasswordServiceClient(conn)}
	return []secretsapp.Store{{Source: onepasswordconfig.Name, Rows: o.rows, Adds: []secretsapp.Action{o.fill()}}}
}

type onepassword struct {
	service onepasswordv1connect.OnePasswordServiceClient
}

// Action that fills a variable from a reference
func (o *onepassword) fill() secretsapp.Action {
	return secretsapp.Action{Label: "Fill a variable", Does: "Take the value of a variable from a 1Password reference", Verb: "fill variable",
		Run: func(s *secretsapp.Screen) tea.Cmd { return o.mapReference(s, "", "") }}
}

// Whether op can read references: a token is set or op is signed in
func signedIn(state *onepasswordv1.StateResponse) bool {
	return state.GetTokenSet() || state.GetSession() != ""
}

func (o *onepassword) rows(ctx context.Context) ([]secretsapp.Row, error) {
	state, err := o.service.State(ctx, &onepasswordv1.StateRequest{})
	if err != nil {
		return nil, err
	}
	rows := []secretsapp.Row{{Name: "1Password", State: status(state), Detail: detail(state), Open: o.actions(state), Store: onepasswordconfig.Name,
		Words: "op " + state.GetAccount()}}
	for _, ref := range state.GetReferences() {
		env, reference := ref.GetEnv(), ref.GetReference()
		rows = append(rows, secretsapp.Row{Name: env, State: reference, Group: filled, Words: reference,
			Detail: []string{env + " takes its value from " + reference + ".", "icebath reads it through op each time it starts, while 1Password is on."},
			Open: []secretsapp.Action{{Label: "Change the reference", Does: "Take the value of " + env + " from another reference", Verb: "change",
				Run: func(s *secretsapp.Screen) tea.Cmd { return o.mapReference(s, env, reference) }}},
			Confirm: "Stop filling " + env + " from 1Password? icebath no longer sets it as it starts.",
			Remove: func(s *secretsapp.Screen) tea.Cmd {
				return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
					_, err := o.service.Unmap(ctx, &onepasswordv1.UnmapRequest{Env: env})
					return secretsapp.Outcome{Note: "1Password no longer fills " + env + "."}, err
				})
			}})
	}
	return rows, nil
}

// State of 1Password in a few words: on or off, and what it lacks
func status(state *onepasswordv1.StateResponse) string {
	switch {
	case state.GetEnabled() && state.GetBinary() == "":
		return "On, op not installed"
	case state.GetEnabled() && !signedIn(state):
		return "On, not signed in"
	case state.GetEnabled():
		return "On, " + fills(len(state.GetReferences()))
	}
	return secretsapp.State(false)
}

// Number of variables 1Password fills, in words
func fills(n int) string {
	switch n {
	case 0:
		return "fills no variable yet"
	case 1:
		return "fills 1 variable"
	}
	return "fills " + strconv.Itoa(n) + " variables"
}

// Lines of the detail of 1Password: what it does, then how op signs in, the account, how it treats your own values, how long it reuses
// what it read, and its program
func detail(state *onepasswordv1.StateResponse) []string {
	lines := []string{about}
	switch {
	case state.GetTokenSet():
		lines = append(lines, "A service account token is in "+state.GetTokenEnv()+".")
	case state.GetSession() != "":
		lines = append(lines, "op is signed in as "+state.GetSession()+".")
	default:
		lines = append(lines, "No token is set and op is not signed in. Set up gives it a token, or run op signin yourself.")
	}
	if account := state.GetAccount(); account != "" {
		lines = append(lines, "Account: "+account)
	}
	if state.GetOverride() {
		lines = append(lines, "Its values replace the ones you set yourself.")
	} else {
		lines = append(lines, "The values you set yourself win over its own.")
	}
	if ttl := state.GetCacheTtl(); ttl > 0 {
		lines = append(lines, "Reuses what it read for "+secretsapp.Seconds(ttl)+".")
	}
	if state.GetBinary() == "" {
		lines = append(lines, "The op program is not installed. Get it at "+onepasswordconfig.Docs+".")
	} else {
		lines = append(lines, "Uses "+strings.TrimSpace(state.GetBinary()+" "+state.GetVersion())+".")
	}
	return lines
}

// What can be done with 1Password as it is now
func (o *onepassword) actions(state *onepasswordv1.StateResponse) []secretsapp.Action {
	open := []secretsapp.Action{
		{Label: "Set up", Does: "Give the account and a token that op signs in with", Verb: "set up",
			Run: func(s *secretsapp.Screen) tea.Cmd { return o.setup(s, state) }},
		o.fill(),
	}
	if state.GetEnabled() {
		return append(open,
			secretsapp.Action{Label: "New token", Does: "Replace the token once 1Password accepts the new one", Verb: "replace", Run: o.token},
			secretsapp.Action{Label: "Fetch now", Does: "Read every reference now and show what each variable gets", Verb: "fetch", Run: o.check},
			secretsapp.Action{Label: "Turn off", Does: "Stop filling variables from 1Password, keeping its token and references", Verb: "turn off", Run: o.off})
	}
	return append(open, secretsapp.Action{Label: "Turn on", Does: "Fill the variables from their references each time icebath starts", Verb: "turn on",
		Run: func(s *secretsapp.Screen) tea.Cmd { return s.Enable(onepasswordconfig.Name, "1Password", true) }})
}

// A form of the account, a service account token, the variable that holds it and where the op program lives
func (o *onepassword) setup(s *secretsapp.Screen, state *onepasswordv1.StateResponse) tea.Cmd {
	token := cards.Field{ID: "token", Label: "Token", Kind: cards.FieldSecret, Help: tokenHelp + "\nLeave it empty to use the session op is signed in to."}
	if state.GetTokenSet() {
		token.Placeholder = "Leave empty to keep the one set"
	}
	spec := cards.FormSpec{Title: "Set up", Intro: []string{about}, Submit: "Set up", Fields: []cards.Field{
		{ID: "account", Label: "Account", Value: state.GetAccount(), Help: "The account shorthand or sign-in address, when op knows more than one."}, token,
		{ID: "token_env", Label: "Token variable", Value: state.GetTokenEnv(), More: true, Help: "The variable the token is kept in.",
			Check: func(value string) string {
				if value = strings.TrimSpace(value); value != "" && !config.EnvName(value) {
					return "A variable name has only letters, digits and underscores, and starts with a letter."
				}
				return ""
			}},
		{ID: "binary", Label: "op program", Value: state.GetBinaryPath(), More: true, Help: "Where op is, only when it is not on your PATH."},
	}}
	return s.Form(spec, func(form *cards.Form, values map[string]string) tea.Cmd {
		request := &onepasswordv1.SetupRequest{Account: strings.TrimSpace(values["account"]), Token: strings.TrimSpace(values["token"]),
			TokenEnv: strings.TrimSpace(values["token_env"]), BinaryPath: strings.TrimSpace(values["binary"])}
		return tea.Batch(s.Hub.Close(form), s.Change("Setting up 1Password…", func(ctx context.Context) (secretsapp.Outcome, error) {
			set, err := o.service.Setup(ctx, request)
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case set.GetBinaryMissing():
				return secretsapp.Outcome{}, errors.New("The op program was not found. Install it from " + onepasswordconfig.Docs + ", then set up again")
			case set.GetIdentity() == "":
				return secretsapp.Outcome{Note: "1Password is on, but op is not signed in and no token is set. Run op signin, or set up again with a token.",
					Warn: true}, nil
			}
			return secretsapp.Outcome{Note: "1Password is on and signed in as " + set.GetIdentity() + "."}, nil
		}))
	})
}

// Form binding a variable to the 1Password reference it takes its value from, the variable named already when its reference changes
func (o *onepassword) mapReference(s *secretsapp.Screen, env, reference string) tea.Cmd {
	title := "Fill a variable"
	if env != "" {
		title = "Change the reference"
	}
	spec := cards.FormSpec{Title: title, Intro: []string{"A variable that takes its value from 1Password each time icebath starts."}, Submit: "Save",
		Fields: []cards.Field{
			{ID: "env", Label: "Variable", Value: env, Placeholder: "OPENAI_API_KEY", Required: true, Help: "The name of the variable to fill.",
				Check: func(value string) string {
					if !config.EnvName(strings.TrimSpace(value)) {
						return "A variable name has only letters, digits and underscores, and starts with a letter."
					}
					return ""
				}},
			{ID: "reference", Label: "Reference", Value: reference, Placeholder: "op://Private/OpenAI/api key", Required: true,
				Help: "In 1Password, Copy Secret Reference on the field gives it.",
				Check: func(value string) string {
					if !strings.HasPrefix(strings.TrimSpace(value), "op://") {
						return "A reference starts with op://."
					}
					return ""
				}},
		}}
	return s.Form(spec, func(form *cards.Form, values map[string]string) tea.Cmd {
		name := strings.TrimSpace(values["env"])
		form.SetBusy(true)
		return tui.Async(func() func() tea.Cmd {
			mapped, err := o.service.Map(s.Kit.Env.Ctx, &onepasswordv1.MapRequest{Env: name, Reference: strings.TrimSpace(values["reference"])})
			return func() tea.Cmd {
				form.SetBusy(false)
				switch {
				case err != nil:
					form.SetError("reference", strings.TrimSuffix(tui.Reason(err), ".")+".")
					return nil
				case mapped.GetReference() == "":
					form.SetError("reference", strings.Join(mapped.GetWarnings(), " "))
					return nil
				}
				done := name + " now takes its value from 1Password."
				if !mapped.GetEnabled() {
					done += " Turn 1Password on to fill it."
				}
				return tea.Batch(s.Hub.Close(form), s.Change("Reading the stores again…", func(context.Context) (secretsapp.Outcome, error) {
					return secretsapp.Outcome{Note: done}, nil
				}))
			}
		})
	})
}

// Asks for a new service account token, kept once 1Password accepts it
func (o *onepassword) token(s *secretsapp.Screen) tea.Cmd {
	return s.Kit.Ask(s.Hub, "New token", cards.Field{Label: "Token", Kind: cards.FieldSecret, Help: tokenHelp}, "Save", func(token string) tea.Cmd {
		if token == "" {
			return nil
		}
		return s.Change("Checking the token with 1Password…", func(ctx context.Context) (secretsapp.Outcome, error) {
			rotated, err := o.service.Token(ctx, &onepasswordv1.TokenRequest{Token: token, Verify: true})
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case rotated.GetBinaryMissing():
				return secretsapp.Outcome{}, errors.New("The op program was not found to check the token, so nothing changed. Install it from " + onepasswordconfig.Docs)
			case rotated.GetRejected():
				return secretsapp.Outcome{}, errors.New("1Password refused the token, so nothing changed")
			case rotated.GetDisabled():
				return secretsapp.Outcome{Note: "Kept the new token. 1Password is off, so it is used once you turn 1Password on."}, nil
			case rotated.GetIdentity() != "":
				return secretsapp.Outcome{Note: "1Password uses the new token, signed in as " + rotated.GetIdentity() + "."}, nil
			}
			return secretsapp.Outcome{Note: rotated.GetTokenEnv() + " holds the new token now."}, nil
		})
	})
}

// Reads every reference now and pages what each variable gets
func (o *onepassword) check(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Reading the references from 1Password…", func(ctx context.Context) (secretsapp.Outcome, error) {
		synced, err := o.service.Sync(ctx, &onepasswordv1.SyncRequest{})
		if err != nil {
			return secretsapp.Outcome{}, err
		}
		rows := make([][2]string, len(synced.GetRows()))
		for i, row := range synced.GetRows() {
			rows[i] = [2]string{row.GetName(), row.GetAction()}
		}
		return secretsapp.Outcome{Title: "1Password", Lines: s.Preview("1Password", rows, synced.GetWarnings())}, nil
	})
}

func (o *onepassword) off(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Turning 1Password off…", func(ctx context.Context) (secretsapp.Outcome, error) {
		_, err := o.service.Disable(ctx, &onepasswordv1.DisableRequest{})
		return secretsapp.Outcome{Note: "1Password is now Off. Its token and references stay for when you turn it on."}, err
	})
}
