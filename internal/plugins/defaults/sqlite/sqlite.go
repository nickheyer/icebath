package sqlite

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sqlite/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sqlite/sessions"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	sessions.Register(registries.Sessions, registries.Config)
}
