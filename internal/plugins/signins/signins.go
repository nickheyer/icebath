package signins

import providercli "github.com/nickheyer/icebath/internal/provider/cli"

func Register(host *providercli.SignIns) *providercli.Registry {
	registry := new(providercli.Registry)
	host.Register(registry)
	return registry
}
