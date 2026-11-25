package server

import "github.com/nickheyer/icebath/internal/server"

func Register(host *server.Server) *server.Registry {
	registry := new(server.Registry)
	host.Register(registry)
	return registry
}
