package views

import toolsapp "github.com/nickheyer/icebath/internal/tools/app"

func Register(host *toolsapp.Views) *toolsapp.Registry {
	registry := new(toolsapp.Registry)
	host.Register(registry)
	return registry
}
