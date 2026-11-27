package optional

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/optional/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/optional/skills"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	skills.Register(registries.Skills)
}
