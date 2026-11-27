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
	return []auxiliary.Task{{Name: mcpconfig.Task, Title: "MCP sampling", About: "The model that writes text that MCP servers ask for", Order: 80,
		Timeout: 30}}
}
