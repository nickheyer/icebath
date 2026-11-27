package skills

import "github.com/nickheyer/icebath/internal/skills"

func Register(host *skills.Skills) *skills.Registry {
	registry := new(skills.Registry)
	host.Register(registry)
	return registry
}
