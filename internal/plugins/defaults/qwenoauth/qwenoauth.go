package qwenoauth

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/qwenoauth/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/qwenoauth/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	models.Register(registries.Models)
}
