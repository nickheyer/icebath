package credentials

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Sign-in with a Claude Pro or Max subscription: the link opened, the code pasted back
var Subscription = credentials.SignIn{Spec: anthropicconfig.Name, Title: "Claude subscription", Type: credentials.OAuth, Run: subscription}

// Setup token claude setup-token printed, pasted
var Pasted = credentials.SignIn{Spec: anthropicconfig.Name, Title: "Setup token from claude setup-token", Type: setupToken, Run: pasted}

// Setup token from running claude setup-token in this terminal, pasted when the Claude CLI isn't here
var SetupToken = credentials.SignIn{Spec: anthropicconfig.Name, Title: "Setup token from claude setup-token", Type: setupToken, Run: run}

// Claude Code's own sign-in on this computer, used as it renews it
var ClaudeCode = credentials.SignIn{Spec: anthropicconfig.Name, Title: "Claude Code sign-in on this computer", Type: "claude-code", Run: borrowed}

// Name auth add --type takes the setup token by
const setupToken = "setup-token"

func subscription(p *credentials.Prompter) (credentials.Signed, error) {
	state := RunIcebathOAuthLoginTo(PoolEnv{Ctx: p.Ctx}, p.Out, func(label string) (string, bool) { return p.Ask(label, false) },
		func(link string) {
			if p.Show != nil {
				p.Show(link, "")
			}
		}, p.Browser)
	token, _ := state["access_token"].(string)
	if token == "" {
		if p.Ctx.Err() != nil {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		return credentials.Signed{}, errNoToken
	}
	refresh, _ := state["refresh_token"].(string)
	expires, _ := state["expires_at_ms"].(float64)
	fields := map[string]any{anthropicconfig.Token: token, anthropicconfig.OAuthToken: nil, anthropicconfig.RefreshToken: nil, anthropicconfig.TokenExpires: nil}
	if refresh != "" {
		fields[anthropicconfig.RefreshToken], fields[anthropicconfig.TokenExpires] = refresh, int64(expires)
	}
	return credentials.Signed{Fields: fields}, nil
}

// Fields of an account holding a setup token, any sign-in of its own cleared so the token is the one used
func tokenFields(token string) credentials.Signed {
	return credentials.Signed{Fields: map[string]any{anthropicconfig.OAuthToken: token, anthropicconfig.Token: nil, anthropicconfig.RefreshToken: nil,
		anthropicconfig.TokenExpires: nil}}
}

func pasted(p *credentials.Prompter) (credentials.Signed, error) {
	p.Say("Run claude setup-token, then paste the token it prints.")
	token, ok := p.Ask("Token: ", true)
	if !ok {
		return credentials.Signed{}, credentials.ErrCancelled
	}
	if token = strings.TrimSpace(token); token == "" {
		return credentials.Signed{}, errors.New("No token was entered, so nothing was added.")
	}
	return tokenFields(token), nil
}

func run(p *credentials.Prompter) (credentials.Signed, error) {
	token, err := RunOAuthSetupToken(PoolEnv{Ctx: p.Ctx})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.Say("%s", err.Error())
		return pasted(p)
	case err != nil:
		return credentials.Signed{}, err
	case token == "":
		if p.Ctx.Err() != nil {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		p.Say("claude setup-token did not leave a token behind, so paste the one it printed.")
		return pasted(p)
	}
	return tokenFields(token), nil
}

// Account that uses the sign-in Claude Code keeps, as Claude Code renews it. The sign-in is read by the server, on its computer and with
// the account's environment, so the server is the one that looks for it as it keeps the account, and refuses one it does not find
func borrowed(*credentials.Prompter) (credentials.Signed, error) {
	return credentials.Signed{Fields: map[string]any{providerconfig.Program: claudeCode}}, nil
}
