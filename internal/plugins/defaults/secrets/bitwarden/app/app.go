// Bitwarden on the page of Secret stores: setting it up as a job in steps, its access token, its region and a project picked from those the
// token sees, replacing its token, and its offline copy
package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	secretsapp "github.com/nickheyer/icebath/internal/secrets/app"
	bitwardenv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1/bitwardenv1connect"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Address of the Bitwarden cloud in the European Union
const eu = "https://vault.bitwarden.eu"

// Where an access token is made
const tokenHelp = "The access token of a machine account, made in the Bitwarden web app under Secrets Manager › Machine accounts › Access tokens."

// Ids of the regions, as the step that asks it holds them
const (
	us  = "us"
	own = "own"
)

type stores struct{}

func Register(registry *secretsapp.Registry) {
	registry.Register(stores{})
}

func (stores) Stores(conn *connect.Client) []secretsapp.Store {
	b := &bitwarden{service: bitwardenv1connect.NewBitwardenServiceClient(conn)}
	return []secretsapp.Store{{Source: bitwardenconfig.Name, Setup: b.setup, Token: b.token,
		Parts: []secretsapp.Part{{Section: bitwardenconfig.Encrypted, Title: bitwardenconfig.EncryptedTitle}}}}
}

type bitwarden struct {
	service bitwardenv1connect.BitwardenServiceClient
}

// Region a server address is in, as the step that asks it holds it
func region(server string) string {
	switch server {
	case "":
		return us
	case eu:
		return "eu"
	}
	return own
}

// Sets Bitwarden up in steps: the access token, the region, the address of a server of your own, then a project picked from those the
// token sees, the region and the project in use first in hand. Its secrets are fetched once before anything is kept
func (b *bitwarden) setup(p *secretsapp.Page, source *secretsv1.Source) tea.Cmd {
	return tui.Call(p.Hub, func() (*bitwardenv1.StateResponse, error) {
		return b.service.State(p.Kit.Env.Ctx, &bitwardenv1.StateRequest{Profile: p.Profile})
	}, func(state *bitwardenv1.StateResponse) tea.Cmd { return b.steps(p, source, state) })
}

// The steps of the setup over what Bitwarden holds now
func (b *bitwarden) steps(p *secretsapp.Page, source *secretsv1.Source, state *bitwardenv1.StateResponse) tea.Cmd {
	f := p.Kit.Flow(p.Hub, "Set up Bitwarden")
	field := cards.Field{ID: "token", Label: "Access token", Kind: cards.FieldSecret, Required: !source.GetTokenSet(),
		Help: tokenHelp + " It is kept in your keys file as " + source.GetTokenEnv() + "."}
	if source.GetTokenSet() {
		field.Help += " Leave it empty to keep the one kept now."
	}
	return f.Ask("", []cards.Field{field}, "", func(values map[string]string) tea.Cmd {
		token := strings.TrimSpace(values["token"])
		current := region(state.GetServerUrl())
		regions := []tui.Choice{
			{ID: us, Title: "US cloud", Detail: "vault.bitwarden.com"},
			{ID: "eu", Title: "EU cloud", Detail: "vault.bitwarden.eu"},
			{ID: own, Title: "Your own server", Detail: "A Bitwarden server you run"},
		}
		return f.Pick("Where your Bitwarden account is.", regions, current, func(choice tui.Choice) tea.Cmd {
			switch choice.ID {
			case us:
				return b.projects(p, f, state, token, "")
			case "eu":
				return b.projects(p, f, state, token, eu)
			}
			address := cards.Field{ID: "server", Label: "Server address", Required: true,
				Help: "The address of your Bitwarden server, such as https://vault.example.com.",
				Check: func(typed string) string {
					if typed = strings.TrimSpace(typed); !strings.HasPrefix(typed, "https://") && !strings.HasPrefix(typed, "http://") {
						return "An address starts with https://."
					}
					return ""
				}}
			if current == own {
				address.Value = state.GetServerUrl()
			}
			return f.Ask("", []cards.Field{address}, "", func(values map[string]string) tea.Cmd {
				return b.projects(p, f, state, token, strings.TrimRight(strings.TrimSpace(values["server"]), "/"))
			})
		})
	})
}

// Asks Bitwarden which projects the token sees on the server, then which one is loaded; a refusal is said and the job stays on its step
func (b *bitwarden) projects(p *secretsapp.Page, f *tui.Flow, state *bitwardenv1.StateResponse, token, server string) tea.Cmd {
	p.Hub.Busy("Asking Bitwarden which projects the token sees…")
	return tui.Call(p.Hub, func() (*bitwardenv1.ProjectsResponse, error) {
		return b.service.Projects(p.Kit.Env.Ctx, &bitwardenv1.ProjectsRequest{Profile: p.Profile, Token: token, ServerUrl: server})
	}, func(listed *bitwardenv1.ProjectsResponse) tea.Cmd {
		switch failure := listed.GetFailure(); {
		case failure != nil && failure.GetCause() == "region":
			return p.Hub.Fail(strs.Full(failure.GetText()) + " The token may be of another region.")
		case failure != nil:
			return p.Hub.Fail(strs.Full(failure.GetText()))
		case len(listed.GetProjects()) == 0:
			return p.Hub.Fail("This token sees no project. Give its machine account a project in the Bitwarden web app.")
		}
		p.Hub.Quiet()
		choices := make([]tui.Choice, len(listed.GetProjects()))
		for i, project := range listed.GetProjects() {
			choices[i] = tui.Choice{ID: project.GetId(), Title: project.GetName()}
		}
		return f.PickVerb("Which project Bitwarden loads its secrets from.", choices, state.GetProjectId(), "Set up", func(choice tui.Choice) tea.Cmd {
			p.Hub.Busy("Fetching the secrets of " + choice.Title + "…")
			return tui.Call(p.Hub, func() (*bitwardenv1.Preview, error) {
				return b.service.Setup(p.Kit.Env.Ctx, &bitwardenv1.SetupRequest{Profile: p.Profile, Token: token, ServerUrl: server, ProjectId: choice.ID})
			}, func(preview *bitwardenv1.Preview) tea.Cmd {
				set := 0
				for _, row := range preview.GetRows() {
					if row.GetAction() == "export" || row.GetAction() == "override" {
						set++
					}
				}
				return tea.Batch(f.End(), p.Done("Bitwarden is on. It fills "+strs.Count(set, "variable")+" from "+choice.Title+"."))
			})
		})
	})
}

// Asks for a new access token, kept once Bitwarden takes it
func (b *bitwarden) token(p *secretsapp.Page, source *secretsv1.Source) tea.Cmd {
	field := cards.Field{Label: "Access token", Kind: cards.FieldSecret,
		Help: tokenHelp + " It takes the place of the one in " + source.GetTokenEnv() + " once Bitwarden takes it."}
	return p.Kit.Ask(p.Hub, "Replace the token", field, "Replace", func(token string) tea.Cmd {
		p.Hub.Busy("Checking the token with Bitwarden…")
		return tui.Call(p.Hub, func() (*bitwardenv1.TokenResponse, error) {
			return b.service.Token(p.Kit.Env.Ctx, &bitwardenv1.TokenRequest{Profile: p.Profile, Token: token, Verify: true})
		}, func(rotated *bitwardenv1.TokenResponse) tea.Cmd {
			switch {
			case rotated.GetBinaryMissing():
				return p.Hub.Fail("bws could not be installed to check the token, so nothing changed.")
			case rotated.GetFailure() != nil:
				return p.Hub.Fail("Bitwarden refused the token, so nothing changed. " + strs.Full(rotated.GetFailure().GetText()))
			case rotated.GetHiddenProject() != "":
				return p.Hub.Fail("Bitwarden took the new token, but it can't see the project in use. Set up again to pick one it sees.")
			case rotated.GetDisabled():
				return p.Done("Kept the new token. Bitwarden is off, so it is used once you turn it on.")
			}
			return p.Done("Bitwarden uses the new token.")
		})
	})
}
