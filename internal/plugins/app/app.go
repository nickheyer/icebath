package app

import "github.com/nickheyer/icebath/internal/app"

func Register(host *app.App) *app.Registry {
	registry := new(app.Registry)
	host.Register(registry)
	return registry
}
