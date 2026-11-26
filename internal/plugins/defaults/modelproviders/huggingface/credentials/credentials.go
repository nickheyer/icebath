package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	huggingfaceconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: huggingfaceconfig.Name, Variables: []string{huggingfaceconfig.APIKeyEnv}})
}
