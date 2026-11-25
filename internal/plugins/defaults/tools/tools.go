package tools

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal"
)

func Register(registries *plugins.Registries) {
	terminal.Register(registries)
	files.Register(registries)
}
