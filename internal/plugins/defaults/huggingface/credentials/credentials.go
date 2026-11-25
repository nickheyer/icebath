package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	huggingfaceconfig "github.com/nickheyer/icebath/internal/plugins/defaults/huggingface/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: huggingfaceconfig.Name, Key: huggingfaceconfig.APIKey, Variables: []string{huggingfaceconfig.APIKeyEnv}})
}
