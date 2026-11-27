package auxiliary

import (
	"github.com/nickheyer/icebath/internal/auxiliary"
	camofoxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: camofoxconfig.Task, Title: "Camofox browser vision", About: "Screenshot analysis for browser_vision on the Camofox " +
		"backend", Inherit: "vision", Vision: true}}
}
