// Bitwarden on the secret stores screen: set it up with a token, a region and a project, rotate its token, check it, turn it off
package app

import (
	"context"
	"fmt"
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

const about = "Keys are pulled from a Bitwarden Secrets Manager project each time icebath starts."

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

func (b *bitwarden) rows(ctx context.Context) ([]secretsapp.Row, error) {
	state, err := b.service.State(ctx, &bitwardenv1.StateRequest{})
	if err != nil {
		return nil, err
	}
	detail := []string{"Bitwarden · " + secretsapp.State(state.GetEnabled()), about}
	if state.GetTokenSet() {
		detail = append(detail, state.GetTokenEnv()+" holds a token.")
	} else {
		detail = append(detail, state.GetTokenEnv()+" holds no token.")
	}
	if state.GetProjectId() != "" {
		detail = append(detail, "project "+state.GetProjectId())
	}
	if state.GetServerUrl() != "" {
		detail = append(detail, state.GetServerUrl())
	}
	if state.GetBinary() == "" {
		detail = append(detail, "Its helper program is not installed.")
	} else {
		detail = append(detail, strings.TrimSpace(state.GetBinary()+" "+state.GetVersion()))
	}
	setup := func(s *secretsapp.Screen) tea.Cmd { return b.setup(s, state) }
	open := []secretsapp.Action{{Label: "Set up", Does: "access token, region and project; one pull checks it works", Run: setup}}
	if state.GetEnabled() {
		open = append(open,
			secretsapp.Action{Label: "New token", Does: "replace the token once the store accepts the new one", Run: b.token},
			secretsapp.Action{Label: "Check now", Does: "ask the store now and show what each secret does", Run: b.check},
			secretsapp.Action{Label: "Turn off", Does: "stop pulling from it; its token and settings stay", Run: b.off})
	}
	return []secretsapp.Row{{Cells: [3]string{"Bitwarden", secretsapp.State(state.GetEnabled()), state.GetProjectId()}, Detail: detail, Open: open}}, nil
}

// Rows of a preview for the screen's page
func rows(preview *bitwardenv1.Preview) [][2]string {
	out := make([][2]string, len(preview.GetRows()))
	for i, row := range preview.GetRows() {
		out[i] = [2]string{row.GetName(), row.GetAction()}
	}
	return out
}

// A form of the access token and the region, then the projects that token sees to pick one from, then one pull of its secrets
func (b *bitwarden) setup(s *secretsapp.Screen, state *bitwardenv1.StateResponse) tea.Cmd {
	region := cards.Field{ID: "region", Label: "region", Kind: cards.FieldChoice, Value: "us", Choices: []cards.Choice{
		{Value: "us", Label: "US cloud"}, {Value: "eu", Label: "EU cloud"}, {Value: "own", Label: "your own server"}}}
	own := cards.Field{ID: "server", Label: "server address", Help: "Only for your own server, like https://vault.example.com."}
	switch state.GetServerUrl() {
	case "":
	case eu:
		region.Value = "eu"
	default:
		region.Value, own.Value = "own", state.GetServerUrl()
	}
	token := cards.Field{ID: "token", Label: "token", Kind: cards.FieldSecret, Required: !state.GetTokenSet(),
		Help: "From the Bitwarden web app: Secrets Manager, Machine accounts, Access tokens."}
	if state.GetTokenSet() {
		token.Placeholder = "leave empty to keep the one set"
	}
	spec := cards.FormSpec{Title: "Bitwarden", Intro: []string{about}, Submit: "Next", Fields: []cards.Field{token, region, own}}
	return s.Form(spec, func(values map[string]string) tea.Cmd {
		server := ""
		switch values["region"] {
		case "eu":
			server = eu
		case "own":
			server = strings.TrimSpace(values["server"])
		}
		newToken := strings.TrimSpace(values["token"])
		s.Hub.Busy("Asking Bitwarden for its projects…")
		return tui.Async(func() func() tea.Cmd {
			listed, err := b.service.Projects(s.Kit.Env.Ctx, &bitwardenv1.ProjectsRequest{Token: newToken, ServerUrl: server})
			return func() tea.Cmd {
				switch {
				case err != nil:
					return s.Hub.Fail(tui.Reason(err))
				case listed.GetFailure() != nil:
					return s.Hub.Fail(listed.GetFailure().GetText())
				case len(listed.GetProjects()) == 0:
					return s.Hub.Fail("This token sees no project. Give its machine account a project in the Bitwarden web app.")
				}
				s.Hub.Quiet()
				choices := make([]tui.Choice, len(listed.GetProjects()))
				for i, project := range listed.GetProjects() {
					choices[i] = tui.Choice{ID: project.GetId(), Title: project.GetName(), Detail: project.GetId()}
				}
				return s.Kit.Pick(s.Hub, "Project to pull keys from", "pick", choices, state.GetProjectId(), func(choice tui.Choice) tea.Cmd {
					return s.Change("Pulling the project's secrets…", func(ctx context.Context) (secretsapp.Outcome, error) {
						preview, err := b.service.Setup(ctx, &bitwardenv1.SetupRequest{Token: newToken, ServerUrl: server, ProjectId: choice.ID})
						if err != nil {
							return secretsapp.Outcome{}, err
						}
						return secretsapp.Outcome{Title: "Bitwarden", Lines: s.Preview(rows(preview), preview.GetWarnings())}, nil
					})
				})
			}
		})
	})
}

// Asks for a new token, kept once Bitwarden accepts it
func (b *bitwarden) token(s *secretsapp.Screen) tea.Cmd {
	return s.Kit.Ask(s.Hub, "Bitwarden", cards.Field{Label: "token", Kind: cards.FieldSecret}, "Keep", func(token string) tea.Cmd {
		if token == "" {
			return nil
		}
		return s.Change("Checking the token…", func(ctx context.Context) (secretsapp.Outcome, error) {
			rotated, err := b.service.Token(ctx, &bitwardenv1.TokenRequest{Token: token, Verify: true})
			switch {
			case err != nil:
				return secretsapp.Outcome{}, err
			case rotated.GetBinaryMissing():
				return secretsapp.Outcome{}, fmt.Errorf("bws could not be installed to check the token; nothing was kept")
			case rotated.GetFailure() != nil:
				return secretsapp.Outcome{}, fmt.Errorf("%s — nothing was kept", rotated.GetFailure().GetText())
			}
			return secretsapp.Outcome{Note: "Bitwarden uses the new token."}, nil
		})
	})
}

// Pulls the project's secrets now and pages what each would do
func (b *bitwarden) check(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Pulling the project's secrets…", func(ctx context.Context) (secretsapp.Outcome, error) {
		preview, err := b.service.Sync(ctx, &bitwardenv1.SyncRequest{})
		if err != nil {
			return secretsapp.Outcome{}, err
		}
		return secretsapp.Outcome{Title: "Bitwarden", Lines: s.Preview(rows(preview), preview.GetWarnings())}, nil
	})
}

func (b *bitwarden) off(s *secretsapp.Screen) tea.Cmd {
	return s.Change("Saving…", func(ctx context.Context) (secretsapp.Outcome, error) {
		_, err := b.service.Disable(ctx, &bitwardenv1.DisableRequest{})
		return secretsapp.Outcome{Note: "Bitwarden is off."}, err
	})
}
