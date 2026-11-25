package transport

import (
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/internal/provider"
)

type Registrant interface {
	Factories() map[provider.APIMode]provider.Factory
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Factories() map[provider.APIMode]provider.Factory {
	factories := map[provider.APIMode]provider.Factory{}
	for _, registrant := range r.registrants {
		maps.Copy(factories, registrant.Factories())
	}
	return factories
}

func (r *Registry) Client(route provider.Route) (provider.Client, error) {
	factory, ok := r.Factories()[route.Mode]
	if !ok {
		return nil, fmt.Errorf("provider %s: no adapter registered for api mode %q", route.Provider, route.Mode)
	}
	return factory(route)
}

type Transport struct {
	Registry
}

func New() *Transport {
	return &Transport{}
}
