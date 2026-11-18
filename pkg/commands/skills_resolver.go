package commands

import "github.com/nickheyer/icebath/pkg/skills"

// Lets skill slash commands detect collisions with built in commands
func init() {
	skills.RegisterCoreCommandResolver(func(name string) bool { return Resolve(name) != nil })
}
