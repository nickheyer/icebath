package sessionsearch

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/sessionsearch/prompt"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/sessionsearch/tools"
)

func Register(registries *plugins.Registries) {
	tools.Register(registries.Tools, registries.Config, registries.Sessions)
	prompt.Register(registries.Prompt)
}
