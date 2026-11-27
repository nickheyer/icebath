package botdesktop

import "github.com/nickheyer/icebath/internal/botdesktop"

func Register(host *botdesktop.Desktop) *botdesktop.Registry {
	registry := new(botdesktop.Registry)
	host.Register(registry)
	return registry
}
