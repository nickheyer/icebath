package transport

import (
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

type Registrant interface {
	Factories() map[provider.APIMode]provider.Factory
}

type Registry struct {
	registrants []Registrant
	// What the host gives every client it makes, nil in a registry nested in it
	host *host
	// What the host and the registries nested in it share
	common *common
}

// What every registry of one host shares: the scaffolds whose end a prompt cache boundary goes at
type common struct {
	scaffolds *scaffolds
}

// What the registry shares with its host, its own when it belongs to none
func (r *Registry) shared() *common {
	if r.common == nil {
		r.common = &common{scaffolds: &scaffolds{}}
	}
	return r.common
}

// Settings and kinds of provider the clients of the host read, and the stalls of each session
type host struct {
	config    *config.Config
	providers *provider.Registry
	stalls    *stalls
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
	client, err := r.make(route)
	if err != nil || route.Keeper == nil {
		return client, err
	}
	return &kept{registry: r, client: client}, nil
}

// Client of the route's API, its broken streams asked again. A chat completions route at Google's own Gemini API goes to the native Gemini
// wire, the only one that address takes
func (r *Registry) make(route provider.Route) (provider.Client, error) {
	if route.Mode == provider.ModeChatCompletions && provider.NativeGemini(route.BaseURL) {
		route.Mode = provider.ModeGeminiNative
	}
	factory, ok := r.Factories()[route.Mode]
	if !ok {
		return nil, fmt.Errorf("%s uses the %s API, which icebath doesn't support.", route.Provider, route.Mode)
	}
	client, err := factory(route)
	if err != nil || r.host == nil {
		return client, err
	}
	return &retried{client: client, config: r.host.config, stalls: r.host.stalls}, nil
}

type Transport struct {
	Registry
}

func New(cfg *config.Config) *Transport {
	transportconfig.Register(cfg)
	t := &Transport{}
	t.Registry.host = &host{config: cfg, stalls: &stalls{sessions: map[string]int{}}}
	t.Registry.common = &common{scaffolds: &scaffolds{}}
	return t
}

// Adds a registrant, a nested registry sharing what the host keeps
func (t *Transport) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.common = t.Registry.common
	}
	t.Registry.Register(registrant)
}

// Keeps the static scaffold of a user message just built from a template, such as a skill command's, so the prompt cache boundary of the
// requests that carry it goes where the scaffold ends
func (t *Transport) Scaffold(prefix string) {
	t.shared().scaffolds.add(prefix)
}

// Kinds of provider the host's clients name in what they tell the person, such as the provider whose credential was renewed
func (t *Transport) Describe(providers *provider.Registry) {
	t.host.providers = providers
}
