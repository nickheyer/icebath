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

// Program a borrowed sign-in comes from, as its source reads
const claudeCode = "Claude Code"

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// Environment of the credential functions for the account
func Env(account credentials.Account) PoolEnv {
	return PoolEnv{Account: account.Fields, Provider: account.Provider.Settings, Ignores: account.Ignores}
}

func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != anthropicconfig.Name {
		return credentials.Credential{}, false
	}
	env := Env(account)
	token := ResolveAnthropicToken(env)
	if token == "" {
		return credentials.Credential{}, false
	}
	credential := credentials.Credential{APIKey: token, Auth: provider.AuthAPIKey, Origin: credentials.External, Source: claudeCode}
	if IsAnthropicOAuthToken(token) {
		credential.Auth = provider.AuthOAuth
	}
	for _, bound := range [][2]string{
		{anthropicconfig.Token, anthropicconfig.TokenEnv},
		{anthropicconfig.OAuthToken, anthropicconfig.OAuthTokenEnv},
		{anthropicconfig.APIKey, anthropicconfig.APIKeyEnv},
	} {
		if found, ok := account.Lookup(bound[0], bound[1]); ok && strings.TrimSpace(found.APIKey) == token {
			credential.Origin, credential.Source = found.Origin, found.Source
			break
		}
	}
	switch {
	case credential.Origin == credentials.Saved && token == strings.TrimSpace(account.Fields.GetString(anthropicconfig.Token)):
		if ms := int64(account.Fields.GetInt(anthropicconfig.TokenExpires)); ms > 0 && account.Fields.GetString(anthropicconfig.RefreshToken) != "" {
			credential.Expires = time.UnixMilli(ms)
		}
	case credential.Origin == credentials.External:
		if creds := ReadClaudeCodeCredentials(env); creds != nil {
			if ms, ok := anyFloat(creds["expiresAt"]); ok && ms > 0 {
				credential.Expires = time.UnixMilli(int64(ms))
			}
		}
	}
	return credential, true
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

// Renews the account's own sign-in with its refresh token, or the borrowed Claude Code sign-in it uses, writing that one back where
// Claude Code keeps it
func (source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != anthropicconfig.Name {
		return false, nil
	}
	env := Env(account)
	env.Ctx = ctx
	refresh := account.Fields.GetString(anthropicconfig.RefreshToken)
	if account.Name == "" || refresh == "" {
		creds := ReadClaudeCodeCredentials(env)
		if creds == nil || anyString(creds["refreshToken"]) == "" {
			return false, nil
		}
		creds["expiresAt"] = float64(0)
		if refreshClaudeCodeOAuthToken(env, creds) == "" {
			return true, credentials.Failed(anthropicconfig.Name, "refresh_failed", "The Claude Code sign-in could not be renewed. Sign in to Claude Code again, "+
				"or sign in to Anthropic from /providers.", false, nil)
		}
		return true, nil
	}
	state, err := RefreshAnthropicOAuthPure(env, refresh, true)
	if err != nil {
		if IsTerminalAnthropicRefreshError(err) {
			return true, credentials.Failed(anthropicconfig.Name, "invalid_grant", "Anthropic OAuth refresh token is terminally invalid ("+err.Error()+
				"); the credential leaves rotation. Re-run `"+credentials.Relogin(account.Provider.ID)+"` to sign in again.", true, err)
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
var errNoToken = errors.New("Sign-in didn't finish. Close this and try again.")
