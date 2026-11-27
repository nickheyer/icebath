package server

import (
	"context"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/web"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	webv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1/webv1connect"
)

type service struct {
	config   *config.Config
	backends *web.Registry
}

func Register(registry *server.Registry, cfg *config.Config, backends *web.Registry) {
	registry.Register(&service{config: cfg, backends: backends})
}

func (s *service) Mount(services *connect.Server) {
	webv1connect.RegisterWebServiceHandler(services, s)
}

func (s *service) List(_ context.Context, request *webv1.ListRequest) (*webv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	searching := s.backends.Settled(cfg, web.Searching)
	reading := s.backends.Settled(cfg, web.Reading)
	rotation := s.backends.Rotation(cfg)
	response := &webv1.ListResponse{
		Backend:        web.Chosen(cfg, webconfig.Backend),
		SearchBackend:  web.Chosen(cfg, webconfig.SearchBackend),
		ExtractBackend: web.Chosen(cfg, webconfig.ExtractBackend),
		Available:      s.backends.Available(cfg),
		Free:           web.FreeOn(cfg),
		Tiers:          map[string]string{},
	}
	tiers, _ := cfg.Get(webconfig.ProviderTier).(map[string]any)
	for name, tier := range tiers {
		if text, ok := tier.(string); ok {
			response.Tiers[name] = text
		}
	}
	for _, backend := range s.backends.Backends() {
		rotating := slices.ContainsFunc(rotation, func(free web.Backend) bool { return free.Name == backend.Name })
		entry := &webv1.Backend{
			Name:        backend.Name,
			Title:       backend.Title,
			Description: backend.About,
			Search:      backend.Search != nil,
			Extract:     backend.Extract != nil,
			Ready:       backend.Set(cfg),
			Free:        backend.FreeReady(cfg),
			Tier:        web.Tier(cfg, backend.Name),
			Searching:   backend.Search != nil && (searching == backend.Name || searching == "" && rotating),
			Reading:     backend.Extract != nil && (reading == backend.Name || reading == "" && rotating),
			Rotating:    rotating && (searching == "" || reading == ""),
			Explicit:    backend.Explicit,
			Settings:    backend.Settings,
		}
		for _, plan := range backend.Plans {
			entry.Plans = append(entry.Plans, &webv1.Plan{Title: plan.Title, Badge: plan.Badge, Description: plan.About, Tier: plan.Tier, Key: plan.Key, Prompt: plan.Prompt, Link: plan.Link})
		}
		response.Backends = append(response.Backends, entry)
	}
	return response, nil
}
