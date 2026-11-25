package agent

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/slash"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	agent := server.Register(
		registries.Server,
		registries.Config,
		registries.Provider,
		registries.Transport,
		registries.Credentials,
		registries.Models,
		registries.Tools,
		registries.Sessions,
		registries.Slash,
	)
	slash.Register(registries.Slash, agent)
	app.Register(registries.App)
}
