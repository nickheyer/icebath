package speech

import "github.com/nickheyer/icebath/internal/speech"

func Register(host *speech.Speech) *speech.Registry {
	registry := new(speech.Registry)
	host.Register(registry)
	return registry
}
