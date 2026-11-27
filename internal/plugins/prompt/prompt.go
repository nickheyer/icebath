package prompt

import "github.com/nickheyer/icebath/internal/prompt"

func Register(host *prompt.Prompt) *prompt.Registry {
	registry := new(prompt.Registry)
	host.Register(registry)
	return registry
}
