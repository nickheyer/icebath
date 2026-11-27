package hooks

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks"
)

func Register(registries *plugins.Registries) {
	shellhooks.Register(registries)
}
