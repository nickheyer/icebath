package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(CopilotBaseURL, "https://api.githubcopilot.com")
	cfg.SetDefault(CopilotDefaultModel, "")
	cfg.MustBindEnv(CopilotBaseURL, CopilotBaseURLEnv)
	cfg.SetDefault(ACPBaseURL, ACPMarkerURL)
	cfg.SetDefault(ACPDefaultModel, "")
	cfg.MustBindEnv(ACPBaseURL, ACPBaseURLEnv)
}
