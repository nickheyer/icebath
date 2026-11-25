package credentials

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type source struct {
	config *config.Config
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	if name != anthropicconfig.Name {
		return credentials.Credential{}, false
	}
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
