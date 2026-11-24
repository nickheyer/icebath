package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

var settingsRegistry = &handlerRegistry{}

func init() {
	settingsRegistry.install()
	settings.Subscribe(func(context.Context, settings.Change) { dropCfgCache() })
}
