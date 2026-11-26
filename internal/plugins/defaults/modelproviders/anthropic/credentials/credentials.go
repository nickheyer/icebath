package credentials

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

const early = 5 * time.Minute

var renewing sync.Mutex

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// Environment of the credential functions for the account
func Env(account credentials.Account) PoolEnv {
	return PoolEnv{Account: account.Fields, Provider: account.Provider.Settings}
}

// Renews the sign-in of the account before it expires, keeping the new tokens in it
func renew(account credentials.Account) {
	renewing.Lock()
	defer renewing.Unlock()
	fields := account.Fields
	refresh := fields.GetString(anthropicconfig.RefreshToken)
	if refresh == "" || time.Now().Add(early).UnixMilli() < int64(fields.GetInt(anthropicconfig.TokenExpires)) {
		return
	}
	state, err := RefreshAnthropicOAuthPure(Env(account), refresh, true)
	if err != nil {
		slog.Warn("Anthropic sign in could not be renewed", "account", account.Name, "error", err)
		if IsTerminalAnthropicRefreshError(err) {
			err = store(account, anthropicconfig.RefreshToken, nil, anthropicconfig.TokenExpires, nil)
		}
	} else {
		expires, _ := state["expires_at_ms"].(float64)
		err = store(account, anthropicconfig.Token, state["access_token"], anthropicconfig.RefreshToken, state["refresh_token"], anthropicconfig.TokenExpires, int64(expires))
	}
	if err != nil {
		slog.Warn("Anthropic sign in could not be saved", "account", account.Name, "error", err)
	}
}

func store(account credentials.Account, pairs ...any) error {
	for i := 0; i < len(pairs); i += 2 {
		if err := account.Fields.Set(pairs[i].(string), pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}

func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != anthropicconfig.Name {
		return credentials.Credential{}, false
	}
	if account.Name != "" {
		renew(account)
	}
	token := ResolveAnthropicToken(Env(account))
	if token == "" {
		return credentials.Credential{}, false
	}
	credential := credentials.Credential{APIKey: token, Auth: provider.AuthAPIKey, Origin: credentials.External, Source: "Claude Code"}
	if IsAnthropicOAuthToken(token) {
		credential.Auth = provider.AuthOAuth
	}
	for _, bound := range [][2]string{
		{anthropicconfig.Token, anthropicconfig.TokenEnv},
		{anthropicconfig.OAuthToken, anthropicconfig.OAuthTokenEnv},
		{anthropicconfig.APIKey, anthropicconfig.APIKeyEnv},
	} {
		if found, ok := credentials.Lookup(account.Fields, bound[0], bound[1]); ok && strings.TrimSpace(found.APIKey) == token {
			credential.Origin, credential.Source = found.Origin, found.Source
			break
		}
	}
	return credential, true
}
