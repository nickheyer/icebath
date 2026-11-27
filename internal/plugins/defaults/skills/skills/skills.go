package skills

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/profile"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/prompt"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/slash"
	"github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config, registries.Skills)
	hub := skills.Register(registries.Skills)
	tools.Register(registries.Tools, registries.Skills, registries.Terminal)
	prompt.Register(registries.Prompt, registries.Skills)
	taken := slash.Register(registries.Slash, registries.Config, registries.Skills, registries.Sessions)
	server.Register(registries.Server, registries.Config, registries.Skills, taken, hub)
	profile.Register(registries.Profile, registries.Config, registries.Skills)
	cli.Register(registries.CLI)
	app.Register(registries.App)
}
