package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name                = "anthropic"
	Key                 = pluginsconfig.Key + "." + Name
	BaseURL             = Key + ".base_url"
	APIKey              = Key + ".api_key"
	Token               = Key + ".token"
	OAuthToken          = Key + ".oauth_token"
	RefreshToken        = Key + ".refresh_token"
	TokenExpires        = Key + ".token_expires"
	AdoptExternalLogins = Key + ".adopt_external_logins"
	DefaultModel        = Key + ".default_model"
)

const (
	APIKeyEnv     = "ANTHROPIC_API_KEY"
	TokenEnv      = "ANTHROPIC_TOKEN"
	OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	BaseURLEnv    = "ANTHROPIC_BASE_URL"
)
