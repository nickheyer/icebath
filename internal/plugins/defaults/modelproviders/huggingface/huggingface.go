package huggingface

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
