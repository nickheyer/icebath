package examples

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample"
)

func Register(registries *plugins.Registries) {
	apluginexample.Register(registries)
}
