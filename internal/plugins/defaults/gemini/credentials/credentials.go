package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	geminiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/gemini/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: geminiconfig.Name, Key: geminiconfig.APIKey, Variables: []string{geminiconfig.GoogleAPIKeyEnv, geminiconfig.GeminiAPIKeyEnv}})
}
