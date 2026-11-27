package bundled

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/bundled/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/bundled/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/bundled/skills"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	skills.Register(registries.Skills)
	packages.Register(registries.Packages)
}
