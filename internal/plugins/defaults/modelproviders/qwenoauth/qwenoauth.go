package qwenoauth

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	models.Register(registries.Models)
}
