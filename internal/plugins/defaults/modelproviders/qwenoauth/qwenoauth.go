package qwenoauth

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	models.Register(registries.Models)
}
