package generate

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/generation"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openrouter/config"
	openrouterprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// OpenRouter's images and videos, with the catalogs they read cached per address and key
type Services struct {
	providers *provider.Registry
	creds     *credentials.Registry
	mu        sync.Mutex
	ids       map[string]idSet
	rows      map[string]modelRows
	videos    map[string]videoCatalog
}

type idSet struct {
	at  time.Time
	ids map[string]bool
}

type modelRows struct {
	at     time.Time
	models []generation.Model
}

type videoCatalog struct {
	at      time.Time
	entries []map[string]any
}

func Register(registry *generation.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(&Services{providers: providers, creds: creds, ids: map[string]idSet{}, rows: map[string]modelRows{}, videos: map[string]videoCatalog{}})
}

// Key and address of the profile's OpenRouter provider or OpenRouter key, false when there is neither
func (s *Services) route(cfg *config.Config) (provider.Route, bool) {
	route, ok := s.creds.Kind(s.providers, cfg, openrouterprovider.Name)
	if !ok {
		return provider.Route{}, false
	}
	route.BaseURL = strings.TrimRight(strings.TrimSpace(route.BaseURL), "/")
	if route.BaseURL == "" {
		route.BaseURL = openrouterprovider.BaseURL
	}
	route.Provider, route.Spec = openrouterprovider.Name, openrouterprovider.Name
	return route, true
}

func (s *Services) Services() []generation.Service {
	return []generation.Service{{
		Name:  openrouterconfig.Name,
		Title: "OpenRouter",
		Ready: func(cfg *config.Config) bool { _, ok := s.route(cfg); return ok },
		Setup: generation.Setup{Badge: "paid", Prompt: "OpenRouter API key", Link: "https://openrouter.ai/keys",
			Providers: []string{openrouterprovider.Name}},
		Images: &generation.Images{
			About:    "Many image models in one place, needs an OpenRouter API key",
			Section:  openrouterconfig.Key,
			Model:    openrouterconfig.Model,
			Models:   s.imageModels,
			Default:  func(context.Context, *config.Config) string { return chatDefault },
			Takes:    imageTakes,
			Generate: s.draw,
		},
		Videos: &generation.Videos{
			About:    "Many video models in one place, needs an OpenRouter API key",
			Section:  openrouterconfig.VideoKey,
			Model:    openrouterconfig.VideoModel,
			Models:   s.videoModels,
			Default:  func(context.Context, *config.Config) string { return videoDefault },
			Takes:    s.videoTakes,
			Generate: s.makeVideo,
		},
	}}
}
