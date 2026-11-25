package credentials

import (
	"os"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/custom/config"
)

type source struct {
	config *config.Config
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	entry, ok := customconfig.Entries(s.config)[name]
	if !ok {
		return credentials.Credential{}, false
	}
	if key := customconfig.Field(entry, customconfig.APIKey); key != "" {
		return credentials.Credential{APIKey: key, Source: "config"}, true
	}
	if variable := customconfig.Field(entry, customconfig.KeyEnv); variable != "" && os.Getenv(variable) != "" {
		return credentials.Credential{APIKey: os.Getenv(variable), Source: variable}, true
	}
	return credentials.Credential{}, false
}
