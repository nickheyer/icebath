package credentials

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

const early = 5 * time.Minute

var renewing sync.Mutex

type source struct {
	config *config.Config
}

func (s source) renew() {
	renewing.Lock()
	defer renewing.Unlock()
	refresh := s.config.GetString(anthropicconfig.RefreshToken)
	if refresh == "" || time.Now().Add(early).UnixMilli() < int64(s.config.GetInt(anthropicconfig.TokenExpires)) {
		return
	}
	state, err := RefreshAnthropicOAuthPure(PoolEnv{Config: s.config}, refresh, true)
	if err != nil {
		slog.Warn("Anthropic sign in could not be renewed", "error", err)
		if IsTerminalAnthropicRefreshError(err) {
			err = s.store(anthropicconfig.RefreshToken, nil, anthropicconfig.TokenExpires, nil)
		}
	} else {
		expires, _ := state["expires_at_ms"].(float64)
		err = s.store(anthropicconfig.Token, state["access_token"], anthropicconfig.RefreshToken, state["refresh_token"], anthropicconfig.TokenExpires, int64(expires))
	}
	if err != nil {
		slog.Warn("Anthropic sign in could not be saved", "error", err)
	}
}

func (s source) store(pairs ...any) error {
	for i := 0; i < len(pairs); i += 2 {
		if err := s.config.Set(pairs[i].(string), pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	if name != anthropicconfig.Name {
		return credentials.Credential{}, false
	}
	s.renew()
	token := ResolveAnthropicToken(PoolEnv{Config: s.config})
	if token == "" {
		return credentials.Credential{}, false
	}
	credential := credentials.Credential{APIKey: token, Auth: provider.AuthAPIKey, Source: "claude_code"}
	if IsAnthropicOAuthToken(token) {
		credential.Auth = provider.AuthOAuth
	}
	for _, bound := range [][2]string{
		{anthropicconfig.Token, anthropicconfig.TokenEnv},
		{anthropicconfig.OAuthToken, anthropicconfig.OAuthTokenEnv},
		{anthropicconfig.APIKey, anthropicconfig.APIKeyEnv},
	} {
		if found, ok := credentials.Lookup(s.config, bound[0], bound[1]); ok && strings.TrimSpace(found.APIKey) == token {
			credential.Source = found.Source
			break
		}
	}
	return credential, true
}
