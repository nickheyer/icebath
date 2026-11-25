package sessions

import "github.com/nickheyer/icebath/internal/sessions"

func Register(host *sessions.Sessions) *sessions.Registry {
	registry := new(sessions.Registry)
	host.Register(registry)
	return registry
}
