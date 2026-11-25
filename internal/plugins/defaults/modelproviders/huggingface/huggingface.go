package huggingface

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
