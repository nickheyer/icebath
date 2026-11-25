package tencenttokenhub

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
