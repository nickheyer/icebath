package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	commandcodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/commandcode/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: commandcodeconfig.ChatName, Key: commandcodeconfig.ChatAPIKey, Variables: []string{commandcodeconfig.APIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: commandcodeconfig.AnthropicName, Key: commandcodeconfig.AnthropicAPIKey, Variables: []string{commandcodeconfig.APIKeyEnv}})
}
