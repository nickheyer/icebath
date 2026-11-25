package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.anthropic.com")
	cfg.SetDefault(DefaultModel, "claude-haiku-4-5-20251001")
	cfg.SetDefault(AdoptExternalLogins, true)
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
	cfg.MustBindEnv(Token, TokenEnv)
	cfg.MustBindEnv(OAuthToken, OAuthTokenEnv)
}
