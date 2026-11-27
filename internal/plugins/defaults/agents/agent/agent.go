package agent

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/launch"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/profile"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/slash"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config, registries.Provider.Options(registries.Config), prompt.Options(registries.Config))
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
		registries.Auxiliary,
		registries.Prompt,
		registries.Approvals,
		registries.Web,
		registries.Skills,
		registries.Contexts,
		registries.Terminal,
		registries.Worktrees,
		registries.Delegation,
		registries.Desktop,
		registries.Hooks,
	)
	prompt.Register(registries.Prompt, agent, registries.Skills)
	profile.Register(registries.Profile, registries.Config, agent)
	slash.Register(registries.Slash, agent)
	asked := launch.Register(registries.Launch)
	app.Register(registries.App, registries.Views, asked, registries.Speech.Host())
	cli.Register(registries.CLI, registries.Launch, registries.SignIns, asked)
}
