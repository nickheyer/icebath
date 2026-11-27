package skills

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/bundled"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/optional"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills"
)

func Register(registries *plugins.Registries) {
	bundled.Register(registries)
	optional.Register(registries)
	skills.Register(registries)
}
