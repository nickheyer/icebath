package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	commandcodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: commandcodeconfig.ChatName, Variables: []string{commandcodeconfig.APIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: commandcodeconfig.AnthropicName, Variables: []string{commandcodeconfig.APIKeyEnv}})
}
