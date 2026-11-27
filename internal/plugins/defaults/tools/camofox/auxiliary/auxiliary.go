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
	return []auxiliary.Task{{Name: camofoxconfig.Task, Title: "Camofox browser vision", About: "The model that reads screenshots of the Camofox browser",
		Order: 70, Inherit: "vision", Vision: true}}
}
