package sqlite

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/sessions"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	sessions.Register(registries.Sessions, registries.Config)
}
