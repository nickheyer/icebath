package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	geminiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: geminiconfig.Name, Variables: []string{geminiconfig.GoogleAPIKeyEnv, geminiconfig.GeminiAPIKeyEnv}})
}
