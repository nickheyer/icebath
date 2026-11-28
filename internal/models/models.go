package models

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Live model list of the routes of a kind of provider
type Registrant interface {
	Models(ctx context.Context, route provider.Route) ([]string, bool)
}

// What a registrant is asked about: the route, the settings of its profile, and the host to reach what else is known through
type Env struct {
	Host   *Models
	Config *config.Config
	Route  provider.Route
	// Read again now, whatever is kept
	Refresh bool
}

// File of the profile's caches with the name
func (e *Env) Cache(name string) string {
	return e.Host.of(e.Config).file(name)
}

// Registrant whose kind's catalog is its own work start to end, such as a live list cut down to the curated one: what it answers is offered
// as it is, without the curated models merged in
type Curator interface {
	Catalog(ctx context.Context, env *Env) (Catalog, bool)
}

// Registrant that knows the context window of a model of its kind from what the provider says of it, such as its models listing
type Sizer interface {
	ContextLength(ctx context.Context, env *Env) (int, bool)
}

// Registrant keeping a public directory of models with their windows and prices, in which a route of any kind can look its model up
type Directory interface {
	Lookup(ctx context.Context, env *Env, model string) (Row, bool)
}

// Registrant that publishes the prices of the models of its kind, by model id
type Pricer interface {
	Prices(ctx context.Context, env *Env) (map[string]provider.Price, bool)
}

// Registrant that names a model of its kind that sees images, for side jobs on one; answering with none leaves the kind out of them
type Seer interface {
	VisionModel(ctx context.Context, env *Env) (string, bool)
}

// Registrant that knows from what the provider lists whether a model of its kind takes reasoning fields, at which efforts, and whether its
// reasoning can be turned off
type Thinker interface {
	Thinking(ctx context.Context, env *Env) (provider.Thinking, bool)
}

type Registry struct {
	registrants []Registrant
	// Host the registry belongs to
	host *Models
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Host the registry belongs to, through which plugins reach the catalogs, the details and the prices of models
func (r *Registry) Host() *Models {
	return r.host
}

// Live model list of the route from the first registrant that answers for its kind
func (r *Registry) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	for _, registrant := range r.registrants {
		if ids, ok := registrant.Models(ctx, route); ok {
			return ids, true
		}
	}
	return nil, false
}

// Catalog of the route from the first registrant that makes it itself
func (r *Registry) curated(ctx context.Context, env *Env) (Catalog, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if catalog, ok := nested.curated(ctx, env); ok {
				return catalog, true
			}
		} else if curator, ok := registrant.(Curator); ok {
			if catalog, ok := curator.Catalog(ctx, env); ok {
				return catalog, true
			}
		}
	}
	return Catalog{}, false
}

// Context window of the route's model from the first registrant that knows it
func (r *Registry) sized(ctx context.Context, env *Env) (int, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if n, ok := nested.sized(ctx, env); ok {
				return n, true
			}
		} else if sizer, ok := registrant.(Sizer); ok {
			if n, ok := sizer.ContextLength(ctx, env); ok && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// Row of the model in the first public directory that lists it
func (r *Registry) lookup(ctx context.Context, env *Env, model string) (Row, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if row, ok := nested.lookup(ctx, env, model); ok {
				return row, true
			}
		} else if directory, ok := registrant.(Directory); ok {
			if row, ok := directory.Lookup(ctx, env, model); ok {
				return row, true
			}
		}
	}
	return Row{}, false
}

// Prices of the route's kind from the first registrant that publishes them
func (r *Registry) prices(ctx context.Context, env *Env) (map[string]provider.Price, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if prices, ok := nested.prices(ctx, env); ok {
				return prices, true
			}
		} else if pricer, ok := registrant.(Pricer); ok {
			if prices, ok := pricer.Prices(ctx, env); ok {
				return prices, true
			}
		}
	}
	return nil, false
}

// Vision model of the route's kind from the first registrant that names one
func (r *Registry) seen(ctx context.Context, env *Env) (string, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if model, ok := nested.seen(ctx, env); ok {
				return model, true
			}
		} else if seer, ok := registrant.(Seer); ok {
			if model, ok := seer.VisionModel(ctx, env); ok {
				return model, true
			}
		}
	}
	return "", false
}

// Reasoning of the route's model from the first registrant that knows it
func (r *Registry) thought(ctx context.Context, env *Env) (provider.Thinking, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if thinking, ok := nested.thought(ctx, env); ok {
				return thinking, true
			}
		} else if thinker, ok := registrant.(Thinker); ok {
			if thinking, ok := thinker.Thinking(ctx, env); ok {
				return thinking, true
			}
		}
	}
	return provider.Thinking{}, false
}

// Registrant whose kind's catalog tags each model with the surfaces it serves, such as DeepInfra's chat, tts, stt and image-gen: the models
// of a surface with what the catalog says of them, as it lists them
type Tagger interface {
	Tagged(ctx context.Context, env *Env, tag string) ([]map[string]any, bool)
}

// Models of a surface from the first registrant of the route's kind that tags them
func (r *Registry) tagged(ctx context.Context, env *Env, tag string) ([]map[string]any, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if models, ok := nested.tagged(ctx, env, tag); ok {
				return models, true
			}
		} else if tagger, ok := registrant.(Tagger); ok {
			if models, ok := tagger.Tagged(ctx, env, tag); ok {
				return models, true
			}
		}
	}
	return nil, false
}

// Models the catalog of the route's kind tags with a surface, each an id and its metadata; none when the catalog can't be read, false when
// no plugin of the kind tags its models
func (m *Models) Tagged(ctx context.Context, cfg *config.Config, route provider.Route, tag string) ([]map[string]any, bool) {
	return m.Registry.tagged(ctx, m.env(cfg, route, false), tag)
}

// Ids of the models the catalog of the route's kind tags with a surface, in its order
func (m *Models) TaggedIDs(ctx context.Context, cfg *config.Config, route provider.Route, tag string) []string {
	models, _ := m.Tagged(ctx, cfg, route, tag)
	var ids []string
	for _, model := range models {
		if id, _ := model["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// What the provider's listings say of the reasoning of the route's model, from the plugin of its kind; false when none knows it
func (m *Models) Thinking(ctx context.Context, cfg *config.Config, route provider.Route) (provider.Thinking, bool) {
	return m.Registry.thought(ctx, m.env(cfg, route, false))
}

// What is known of the models of every provider: their catalogs, the details models.dev keeps, the windows and prices providers state,
// what requests cost and what accounts may still spend. Each profile keeps its own caches in its folder
type Models struct {
	Registry
	providers *provider.Registry

	mu       sync.Mutex
	profiles map[string]*state
}

func New(cfg *config.Config, providers *provider.Registry) *Models {
	modelsconfig.Register(cfg, providers.Options(cfg), providerconfig.PickProvider)
	m := &Models{providers: providers, profiles: map[string]*state{}}
	m.Registry.host = m
	return m
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (m *Models) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = m
	}
	m.Registry.Register(registrant)
}

// What the profile of the settings keeps in memory, made on first use
func (m *Models) of(cfg *config.Config) *state {
	dir := cfg.ProfileDir(cfg.Profile())
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.profiles[dir]
	if !ok {
		s = newState(dir)
		m.profiles[dir] = s
	}
	return s
}

// File of the profile's caches with the name
func (s *state) file(name string) string {
	return filepath.Join(s.dir, "cache", name)
}

// What a registrant is asked about the route on the profile of the settings
func (m *Models) env(cfg *config.Config, route provider.Route, refresh bool) *Env {
	return &Env{Host: m, Config: cfg, Route: route, Refresh: refresh}
}

// Model of the route's kind that sees images, from its plugin when it names one, and whether it does: one that answers but finds none
// leaves the kind out of side jobs on images
func (m *Models) VisionModel(ctx context.Context, cfg *config.Config, route provider.Route) (string, bool) {
	return m.Registry.seen(ctx, m.env(cfg, route, false))
}

// Kind of the route, as its plugin registers it
func (m *Models) Spec(route provider.Route) provider.Spec {
	spec, _ := m.providers.Spec(route.Spec)
	return spec
}
