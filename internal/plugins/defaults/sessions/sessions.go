package sessions

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite"
)

func Register(registries *plugins.Registries) {
	sqlite.Register(registries)
}
