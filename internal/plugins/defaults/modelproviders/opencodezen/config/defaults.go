package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(ZenBaseURL, "https://opencode.ai/zen/v1")
	cfg.SetDefault(ZenDefaultModel, "gemini-3-flash")
	cfg.MustBindEnv(ZenBaseURL, ZenBaseURLEnv)
	cfg.MustBindEnv(ZenAPIKey, ZenAPIKeyEnv)
	cfg.SetDefault(GoBaseURL, "https://opencode.ai/zen/go/v1")
	cfg.SetDefault(GoDefaultModel, "glm-5")
	cfg.MustBindEnv(GoBaseURL, GoBaseURLEnv)
	cfg.MustBindEnv(GoAPIKey, GoAPIKeyEnv)
}
