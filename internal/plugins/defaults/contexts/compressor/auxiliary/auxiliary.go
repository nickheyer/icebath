package auxiliary

import (
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts/compressor/engine"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: engine.Task, Title: "Compression", About: "Model that summarises the older messages of long chats", Timeout: 120,
		Settings: []config.Setting{
			{Key: engine.AuxWindow, Title: "Compression context window", About: "Tokens the compression model takes in at once, when icebath guesses wrong; 0 uses the size icebath knows", Default: 0},
		}}}
}
