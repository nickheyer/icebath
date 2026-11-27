package auxiliary

import (
	"github.com/nickheyer/icebath/internal/auxiliary"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: mcpconfig.Task, Title: "MCP sampling", About: "Model that answers MCP servers asking the agent's model to write for them",
		Timeout: 30}}
}
