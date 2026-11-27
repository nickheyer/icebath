// Bitwarden on the secret stores screen: set it up with a token, a region and a project, give it a new token, fetch from it now, turn it
// on or off
package app

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	bitwardenv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1/bitwardenv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Address of the Bitwarden cloud in the European Union
const eu = "https://vault.bitwarden.eu"

// What Bitwarden does on the screen, first in its detail
const about = "Loads API keys from a project of Bitwarden Secrets Manager each time icebath starts."

// Where a person sets what the screen does not, under the settings
const where = "in /settings under Secret stores › Bitwarden"

type stores struct{}

func Register(registry *secretsapp.Registry) {
	registry.Register(stores{})
}

func (stores) Stores(conn *connect.Client) []secretsapp.Store {
	b := &bitwarden{service: bitwardenv1connect.NewBitwardenServiceClient(conn)}
	return []secretsapp.Store{{Source: bitwardenconfig.Name, Rows: b.rows}}
}

type bitwarden struct {
	service bitwardenv1connect.BitwardenServiceClient
}

// Whether Bitwarden has what a fetch needs: a token and a project
func ready(state *bitwardenv1.StateResponse) bool {
	return state.GetTokenSet() && state.GetProjectId() != ""
}

func (b *bitwarden) rows(ctx context.Context) ([]secretsapp.Row, error) {
	state, err := b.service.State(ctx, &bitwardenv1.StateRequest{})
	if err != nil {
		return nil, err
	}
	return []secretsapp.Row{{Name: "Bitwarden", State: status(state), Detail: detail(state), Open: b.actions(state), Store: bitwardenconfig.Name,
		Words: "bws secrets manager " + state.GetProjectId()}}, nil
}

// State of Bitwarden in a few words: on or off, and what it lacks
func status(state *bitwardenv1.StateResponse) string {
	switch {
	case state.GetEnabled() && !state.GetTokenSet():
		return "On, no token"
	case state.GetEnabled() && state.GetProjectId() == "":
		return "On, no project"
	case !state.GetEnabled() && !ready(state):
		return "Off, not set up"
	}
	return secretsapp.State(state.GetEnabled())
}

// Lines of the detail of Bitwarden: what it does, then its token, project, server, how it treats your own values, how long it reuses what
// it fetched, and its program
func detail(state *bitwardenv1.StateResponse) []string {
	lines := []string{about}
	if state.GetTokenSet() {
		lines = append(lines, "The access token is in "+state.GetTokenEnv()+".")
	} else {
		lines = append(lines, state.GetTokenEnv()+" holds no access token yet. Set up gives it one.")
	}
	if project := state.GetProjectId(); project != "" {
		lines = append(lines, "Project ID: "+project)
	}
	server := "US cloud"
	switch state.GetServerUrl() {
	case "":
	case eu:
		server = "EU cloud"
	default:
		server = state.GetServerUrl()
	}
	lines = append(lines, "Server: "+server)
	if state.GetOverride() {
		lines = append(lines, "Its values replace the ones you set yourself.")
	} else {
		lines = append(lines, "The values you set yourself win over its own.")
	}
	if ttl := state.GetCacheTtl(); ttl > 0 {
		lines = append(lines, "Reuses what it fetched for "+secretsapp.Seconds(ttl)+".")
	}
	switch {
	case state.GetBinary() != "":
		lines = append(lines, "Uses "+strings.TrimSpace(state.GetBinary()+" "+state.GetVersion())+".")
	case state.GetAutoInstall():
		lines = append(lines, "Its program bws is not installed yet. Set up installs it.")
	default:
		lines = append(lines, "Its program bws is not installed. Install it, or turn on Install bws "+where+".")
	}
	return lines
}

// What can be done with Bitwarden as it is now
func (b *bitwarden) actions(state *bitwardenv1.StateResponse) []secretsapp.Action {
	setup := func(s *secretsapp.Screen) tea.Cmd { return b.setup(s, state) }
	open := []secretsapp.Action{{Label: "Set up", Does: "Give an access token, a region and a project, then fetch once to check them", Verb: "set up", Run: setup}}
	switch {
	case state.GetEnabled():
		open = append(open,
			secretsapp.Action{Label: "New token", Does: "Replace the access token once Bitwarden accepts the new one", Verb: "replace", Run: b.token},
			secretsapp.Action{Label: "Fetch now", Does: "Ask Bitwarden now and show what each secret does", Verb: "fetch", Run: b.check},
			secretsapp.Action{Label: "Turn off", Does: "Stop loading from Bitwarden, keeping its token and settings", Verb: "turn off", Run: b.off})
	case ready(state):
		open = append(open, secretsapp.Action{Label: "Turn on", Does: "Load from the project you set up each time icebath starts", Verb: "turn on",
			Run: func(s *secretsapp.Screen) tea.Cmd { return s.Enable(bitwardenconfig.Name, "Bitwarden", true) }})
	}
	return open
}

// Rows of a preview for the screen's page
func rows(preview *bitwardenv1.Preview) [][2]string {
	out := make([][2]string, len(preview.GetRows()))
	for i, row := range preview.GetRows() {
		out[i] = [2]string{row.GetName(), row.GetAction()}
	}
	return out
}

// Region a server address is in, as the region field names it
func region(server string) string {
	switch server {
	case "":
		return "us"
	case eu:
		return "eu"
	}
	return "own"
}

// Fields of the setup form: the token, the region, and the server address while the region is a server of your own
func fields(state *bitwardenv1.StateResponse, values map[string]string) []cards.Field {
	value := func(id, fallback string) string {
		if v, ok := values[id]; ok {
			return v
		}
		return fallback
	}
	token := cards.Field{ID: "token", Label: "Access token", Kind: cards.FieldSecret, Required: !state.GetTokenSet(), Value: values["token"],
		Help: "In the Bitwarden web app: Secrets Manager › Machine accounts › Access tokens."}
	if state.GetTokenSet() {
		token.Placeholder = "Leave empty to keep the one set"
	}
	chosen := value("region", region(state.GetServerUrl()))
	out := []cards.Field{token, {ID: "region", Label: "Region", Kind: cards.FieldChoice, Value: chosen, Choices: []cards.Choice{
		{Value: "us", Label: "US cloud", Meaning: "vault.bitwarden.com"}, {Value: "eu", Label: "EU cloud", Meaning: "vault.bitwarden.eu"},
		{Value: "own", Label: "Your own server", Meaning: "A Bitwarden server you run"}}}}
	if chosen == "own" {
		own := ""
		if region(state.GetServerUrl()) == "own" {
			own = state.GetServerUrl()
		}
		out = append(out, cards.Field{ID: "server", Label: "Server address", Value: value("server", own), Required: true, Placeholder: "https://vault.example.com",
			Check: func(text string) string {
				if text = strings.TrimSpace(text); !strings.HasPrefix(text, "https://") && !strings.HasPrefix(text, "http://") {
					return "Start the address with https://."
				}
				return ""
			}})
	}
	return out
}

// A form of the access token and the region, then the projects that token sees to pick one from, then one fetch of its secrets
func (b *bitwarden) setup(s *secretsapp.Screen, state *bitwardenv1.StateResponse) tea.Cmd {
	spec := cards.FormSpec{Title: "Set up", Intro: []string{about}, Submit: "Next", Fields: fields(state, map[string]string{}),
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "region" {
				f.SetFields(fields(state, f.Values()))
			}
			return nil
		}}
	return s.Form(spec, func(form *cards.Form, values map[string]string) tea.Cmd {
		server := ""
		switch values["region"] {
		case "eu":
			server = eu
		case "own":
			server = strings.TrimRight(strings.TrimSpace(values["server"]), "/")
		}
		newToken := strings.TrimSpace(values["token"])
		form.SetBusy(true)
		s.Hub.Busy("Asking Bitwarden which projects the token sees…")
		return tui.Async(func() func() tea.Cmd {
			listed, err := b.service.Projects(s.Kit.Env.Ctx, &bitwardenv1.ProjectsRequest{Token: newToken, ServerUrl: server})
			return func() tea.Cmd {
				form.SetBusy(false)
				s.Hub.Quiet()
				switch {
				case err != nil:
					form.SetError("token", strings.TrimSuffix(tui.Reason(err), ".")+".")
					return nil
				case listed.GetFailure() != nil:
					field := "token"
					if listed.GetFailure().GetCause() == "region" {
						field = "region"
					}
					form.SetError(field, strings.TrimSuffix(listed.GetFailure().GetText(), ".")+".")
					return nil
				case len(listed.GetProjects()) == 0:
					form.SetError("token", "This token sees no project. Give its machine account a project in the Bitwarden web app.")
					return nil
				}
				choices := make([]tui.Choice, len(listed.GetProjects()))
				for i, project := range listed.GetProjects() {
					choices[i] = tui.Choice{ID: project.GetId(), Title: project.GetName(), Detail: "ID " + project.GetId()}
				}
				return tea.Batch(s.Hub.Close(form), s.Kit.Pick(s.Hub, "Project", "use", choices, state.GetProjectId(), func(choice tui.Choice) tea.Cmd {
					return s.Change("Fetching the secrets of "+choice.Title+"…", func(ctx context.Context) (secretsapp.Outcome, error) {
						preview, err := b.service.Setup(ctx, &bitwardenv1.SetupRequest{Token: newToken, ServerUrl: server, ProjectId: choice.ID})
						if err != nil {
							return secretsapp.Outcome{}, err
						}
						return secretsapp.Outcome{Title: "Bitwarden", Lines: s.Preview("Bitwarden", rows(preview), preview.GetWarnings())}, nil
					})
				}))
			}
		})
	})
}

// Asks for a new token, kept once Bitwarden accepts it
func (b *bitwarden) token(s *secretsapp.Screen) tea.Cmd {
	field := cards.Field{Label: "Access token", Kind: cards.FieldSecret, Help: "In the Bitwarden web app: Secrets Manager › Machine accounts › Access tokens."}
	return s.Kit.Ask(s.Hub, "New token", field, "Save", func(token string) tea.Cmd {
		if token == "" {
			return nil
		}
		return s.Change("Checking the token with Bitwarden…", func(ctx context.Context) (secretsapp.Outcome, error) {
			rotated, err := b.service.Token(ctx, &bitwardenv1.TokenRequest{Token: token, Verify: true})
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case rotated.GetBinaryMissing():
				return secretsapp.Outcome{}, errors.New("bws could not be installed to check the token, so nothing changed")
			case rotated.GetFailure() != nil:
				return secretsapp.Outcome{}, errors.New("Bitwarden refused the token, so nothing changed: " + strings.TrimSuffix(rotated.GetFailure().GetText(), "."))
			case rotated.GetHiddenProject() != "":
				return secretsapp.Outcome{Note: "Bitwarden took the new token, but it can't see the project " + rotated.GetHiddenProject() +
					". Set up again to pick a project it sees.", Warn: true}, nil
			case rotated.GetDisabled():
				return secretsapp.Outcome{Note: "Kept the new token. Bitwarden is off, so it is used once you turn Bitwarden on."}, nil
			}
			return secretsapp.Outcome{Note: "Bitwarden uses the new token now."}, nil
		})
	})
}

// Fetches the project's secrets now and pages what each does
func (b *bitwarden) check(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Fetching the secrets of the project…", func(ctx context.Context) (secretsapp.Outcome, error) {
		preview, err := b.service.Sync(ctx, &bitwardenv1.SyncRequest{})
		if err != nil {
			return secretsapp.Outcome{}, err
		}
		return secretsapp.Outcome{Title: "Bitwarden", Lines: s.Preview("Bitwarden", rows(preview), preview.GetWarnings())}, nil
	})
}

func (b *bitwarden) off(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Turning Bitwarden off…", func(ctx context.Context) (secretsapp.Outcome, error) {
		_, err := b.service.Disable(ctx, &bitwardenv1.DisableRequest{})
		return secretsapp.Outcome{Note: "Bitwarden is now Off. Its token and settings stay for when you turn it on."}, err
	})
}
