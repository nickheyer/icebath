package web

import "github.com/nickheyer/icebath/internal/web"

func Register(host *web.Web) *web.Registry {
	registry := new(web.Registry)
	host.Register(registry)
	return registry
}
