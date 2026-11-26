package profile

import "github.com/nickheyer/icebath/internal/profile"

func Register(host *profile.Profiles) *profile.Registry {
	registry := new(profile.Registry)
	host.Register(registry)
	return registry
}
