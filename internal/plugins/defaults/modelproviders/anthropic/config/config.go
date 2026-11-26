package config

import providerconfig "github.com/nickheyer/icebath/internal/provider/config"

const (
	Name         = "anthropic"
	Title        = "Anthropic"
	BaseURL      = "https://api.anthropic.com"
	DefaultModel = "claude-haiku-4-5-20251001"
)

// Fields of an account, below its name
const (
	APIKey       = providerconfig.APIKeyField
	Token        = "token"
	OAuthToken   = "oauth_token"
	RefreshToken = "refresh_token"
	TokenExpires = "token_expires"
)

// Settings of a provider, below its name
const AdoptExternalLogins = "adopt_external_logins"

const (
	APIKeyEnv     = "ANTHROPIC_API_KEY"
	TokenEnv      = "ANTHROPIC_TOKEN"
	OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	BaseURLEnv    = "ANTHROPIC_BASE_URL"
)
