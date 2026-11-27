package generate

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/models"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/deepinfra/config"
	deepinfraprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// DeepInfra's images and videos, from the tagged catalog the DeepInfra provider keeps
type services struct {
	providers *provider.Registry
	creds     *credentials.Registry
	models    *models.Models
}

func Register(registry *generation.Registry, providers *provider.Registry, creds *credentials.Registry, m *models.Models) {
	registry.Register(&services{providers: providers, creds: creds, models: m})
}

// Key and address of the profile's DeepInfra provider or DeepInfra key
func (s *services) route(cfg *config.Config) (provider.Route, bool) {
	route, ok := s.creds.Kind(s.providers, cfg, deepinfraprovider.Name)
	route.BaseURL = strings.TrimRight(strs.Filled(route.BaseURL, deepinfraprovider.BaseURL), "/")
	route.Provider, route.Spec = deepinfraprovider.Name, deepinfraprovider.Name
	return route, ok
}

// Models of the catalog whose surface tags hold the tag; none when it can't be read
func (s *services) tagged(ctx context.Context, cfg *config.Config, tag string) []map[string]any {
	route, _ := s.route(cfg)
	items, _ := s.models.Tagged(ctx, cfg, route, tag)
	return items
}

func (s *services) Services() []generation.Service {
	return []generation.Service{{
		Name:  deepinfraconfig.Name,
		Title: "DeepInfra",
		Ready: func(cfg *config.Config) bool { _, ok := s.route(cfg); return ok },
		Setup: generation.Setup{Badge: "paid", Prompt: "DeepInfra API key", Link: "https://deepinfra.com/dash/api_keys", Provider: deepinfraprovider.Name},
		Images: &generation.Images{
			About:    "Open image models, needs a DeepInfra API key",
			Settings: []string{deepinfraconfig.Model, deepinfraconfig.BaseURL},
			Model:    deepinfraconfig.Model,
			Models:   s.imageModels,
			Default:  func(ctx context.Context, cfg *config.Config) string { return s.imageModel(ctx, cfg, "") },
			Takes:    func(*config.Config) generation.ImageTakes { return generation.ImageTakes{} },
			Generate: s.draw,
		},
		Videos: &generation.Videos{
			About:   "Open video models from text or an image, needs a DeepInfra API key",
			Models:  s.videoModels,
			Default: s.videoDefault,
			Takes: func(*config.Config) generation.VideoTakes {
				return generation.VideoTakes{Modalities: []string{"text", "image"}, Aspects: []string{"16:9", "9:16", "1:1"}, Resolutions: []string{"480p", "720p", "1080p"},
					MinDuration: 1, MaxDuration: 10, Negative: true, Seed: true}
			},
			Generate: s.makeVideo,
		},
	}}
}
