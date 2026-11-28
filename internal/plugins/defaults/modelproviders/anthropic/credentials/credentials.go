package credentials

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// How long before its expiry a sign-in of icebath's own is renewed
const early = 5 * time.Minute

// Program a borrowed sign-in comes from, as its source reads and as an account that uses it names it
const claudeCode = "Claude Code"

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// Environment of the credential functions for the account
func Env(account credentials.Account) PoolEnv {
	return PoolEnv{Account: account.Fields, Provider: account.Provider.Settings, Ignores: account.Ignores}
}

// Credential of an account: its own sign-in, a setup token, an API key, the key in the variable it reads, or the sign-in of Claude Code on
// this computer for an account that uses that. Outside the accounts: a token or key in the variables Claude Code reads, else the sign-in
// of Claude Code on this computer
func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != anthropicconfig.Name {
		return credentials.Credential{}, false
	}
	if account.Name == "" {
		return outside(account)
	}
	fields := account.Fields
	if token := strings.TrimSpace(fields.GetString(anthropicconfig.Token)); credentials.Usable(token) {
		credential := credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved}
		if ms := int64(fields.GetInt(anthropicconfig.TokenExpires)); ms > 0 && fields.GetString(anthropicconfig.RefreshToken) != "" {
			credential.Expires = time.UnixMilli(ms)
		}
		return credential, true
	}
	if token := strings.TrimSpace(fields.GetString(anthropicconfig.OAuthToken)); credentials.Usable(token) {
		return credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved}, true
	}
	if found, ok := account.Lookup(anthropicconfig.APIKey); ok {
		found.Auth = auth(found.APIKey)
		return found, true
	}
	if account.Program() != claudeCode {
		return credentials.Credential{}, false
	}
	return borrowedSignIn(Env(account))
}

// Credential outside the accounts: the token or key in the variables Claude Code reads, Claude Code's renewable sign-in over a static token
// it holds too, else the sign-in of Claude Code on this computer
func outside(account credentials.Account) (credentials.Credential, bool) {
	env := Env(account)
	token := ResolveAnthropicToken(env)
	if token == "" {
		return credentials.Credential{}, false
	}
	for _, variable := range []string{anthropicconfig.TokenEnv, anthropicconfig.OAuthTokenEnv, anthropicconfig.APIKeyEnv} {
		if found, ok := account.Lookup("", variable); ok && strings.TrimSpace(found.APIKey) == token {
			found.Auth = auth(token)
			return found, true
		}
	}
	return claudeCodeCredential(env, token), true
}

// The sign-in of Claude Code on this computer, renewed when it ran out
func borrowedSignIn(env PoolEnv) (credentials.Credential, bool) {
	token := resolveClaudeCodeTokenFromCredentials(env, nil)
	if token == "" {
		return credentials.Credential{}, false
	}
	return claudeCodeCredential(env, token), true
}

// Credential of a token of the sign-in of Claude Code, with when that sign-in runs out
func claudeCodeCredential(env PoolEnv, token string) credentials.Credential {
	credential := credentials.Credential{APIKey: token, Auth: auth(token), Origin: credentials.External, Source: claudeCode}
	if creds := ReadClaudeCodeCredentials(env); creds != nil {
		if ms, ok := anyFloat(creds["expiresAt"]); ok && ms > 0 {
			credential.Expires = time.UnixMilli(int64(ms))
		}
	}
	return credential
}

// How a token signs in: a sign-in or setup token, else an API key
func auth(token string) provider.AuthKind {
	if IsAnthropicOAuthToken(token) {
		return provider.AuthOAuth
	}
	return provider.AuthAPIKey
}

// Why an account that uses the sign-in of Claude Code has no credential, or the credential outside the accounts lacks it: the setting that
// turns it off, the provider leaving it out, no sign-in where Claude Code keeps it, or one that ran out and could not be renewed. Nothing to
// say outside the accounts while Claude Code was never signed in here, or is left out on purpose
func (source) Why(account credentials.Account) (string, bool) {
	if account.Spec() != anthropicconfig.Name || account.Name != "" && account.Program() != claudeCode {
		return "", false
	}
	env := Env(account)
	label := account.Provider.Label()
	path := ClaudeCodeCredentialsPath(env)
	switch {
	case account.Name == "" && account.Ignores(claudeCode):
		return "", false
	case account.Ignores(claudeCode):
		return "It uses the sign-in of Claude Code, which " + label + " leaves out until you turn it on again.", true
	case ReadClaudeCodeCredentials(env) != nil:
		return "The sign-in of Claude Code in " + path + " ran out and could not be renewed. Sign in again with `claude`, then try again.", true
	case account.Name == "":
		return "", false
	}
	return "It uses the sign-in of Claude Code, and none was found in " + path + ". Sign in with `claude`, then try again.", true
}

// Whether the account's own sign-in expires within five minutes and keeps the refresh token that renews it
func (source) Due(account credentials.Account) bool {
	if account.Spec() != anthropicconfig.Name || account.Name == "" || account.Fields.GetString(anthropicconfig.RefreshToken) == "" {
		return false
	}
	return time.Now().Add(early).UnixMilli() >= int64(account.Fields.GetInt(anthropicconfig.TokenExpires))
}

// Whether the account holds its own sign-in, whose refresh token renews it once, so a copy in another profile can't share it
func (source) Rotates(account credentials.Account) bool {
	return account.Spec() == anthropicconfig.Name && account.Name != "" && account.Fields.GetString(anthropicconfig.RefreshToken) != ""
}

// Renews the account's own sign-in with its refresh token, or the borrowed Claude Code sign-in it or the credential outside the accounts
// uses, writing that one back where Claude Code keeps it. An account with neither has nothing to renew
func (source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != anthropicconfig.Name {
		return false, nil
	}
	env := Env(account)
	env.Ctx = ctx
	refresh := account.Fields.GetString(anthropicconfig.RefreshToken)
	if account.Name == "" || account.Program() == claudeCode {
		creds := ReadClaudeCodeCredentials(env)
		if creds == nil || anyString(creds["refreshToken"]) == "" {
			return false, nil
		}
		creds["expiresAt"] = float64(0)
		if refreshClaudeCodeOAuthToken(env, creds) == "" {
			return true, credentials.Failed(anthropicconfig.Name, "refresh_failed", "The Claude Code sign-in could not be renewed. Sign in to Claude Code again, "+
				"or add an account of your own to Anthropic.", false, nil)
		}
		return true, nil
	}
	if refresh == "" {
		return false, nil
	}
	state, err := RefreshAnthropicOAuthPure(env, refresh, true)
	if err != nil {
		if IsTerminalAnthropicRefreshError(err) {
			return true, credentials.Failed(anthropicconfig.Name, "invalid_grant", "The Claude sign-in of this account no longer works, so it leaves "+
				"the rotation. Run `"+credentials.Relogin(account.Provider.ID)+"` to sign in again.", true, err)
		}
		return true, err
	}
	expires, _ := state["expires_at_ms"].(float64)
	for _, field := range [][2]any{{anthropicconfig.Token, state["access_token"]}, {anthropicconfig.RefreshToken, state["refresh_token"]},
		{anthropicconfig.TokenExpires, int64(expires)}} {
		if err := account.Fields.Set(field[0].(string), field[1]); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Error of a sign-in that ended without a token
var errNoToken = errors.New("The sign-in did not finish, so no account was added. Start the sign-in again.")
