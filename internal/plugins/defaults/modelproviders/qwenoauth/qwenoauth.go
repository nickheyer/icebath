package qwenoauth

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
