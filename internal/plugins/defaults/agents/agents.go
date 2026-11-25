package agents

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent"
)

func Register(registries *plugins.Registries) {
	agent.Register(registries)
}
