package server

import (
	"context"
	"strings"

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
	// Every write a person asks for of a choice of service chooses it as the page of Web search does, clearing what it replaces
	for key, use := range map[string]web.Use{webconfig.Backend: web.Both, webconfig.SearchBackend: web.Searches, webconfig.ExtractBackend: web.Pages} {
		cfg.Guard(key, config.Guard{Write: func(cfg *config.Config, _ string, value any) (bool, error) {
			name, ok := value.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return false, nil
			}
			_, err := backends.Choose(cfg, name, use, "")
			return true, err
		}})
	}
}

func (s *service) Mount(services *connect.Server) {
	webv1connect.RegisterWebServiceHandler(services, s)
}

func (s *service) List(_ context.Context, request *webv1.ListRequest) (*webv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &webv1.ListResponse{
		Backend:        web.Chosen(cfg, webconfig.Backend),
		SearchBackend:  web.Chosen(cfg, webconfig.SearchBackend),
		ExtractBackend: web.Chosen(cfg, webconfig.ExtractBackend),
		Available:      s.backends.Available(cfg),
		Free:           web.FreeOn(cfg),
		Tiers:          map[string]string{},
		Serving:        s.backends.InUse(cfg),
	}
	tiers, _ := cfg.Get(webconfig.ProviderTier).(map[string]any)
	for name, tier := range tiers {
		if text, ok := tier.(string); ok {
			response.Tiers[name] = text
		}
	}
	for _, backend := range s.backends.Backends() {
		standing := s.backends.Standing(cfg, backend)
		entry := &webv1.Backend{
			Name:        backend.Name,
			Title:       backend.Title,
			Description: backend.About,
			Search:      backend.Search != nil,
			Extract:     backend.Extract != nil,
			Ready:       backend.Set(cfg),
			Free:        backend.FreeReady(cfg),
			Tier:        web.Tier(cfg, backend.Name),
			Searching:   standing.Searching,
			Reading:     standing.Reading,
			Rotating:    standing.Rotating,
			Explicit:    backend.Explicit,
			Settings:    backend.Settings,
			State:       standing.State,
			Usable:      standing.Usable,
		}
		if len(backend.Settings) > 0 {
			if section, _, ok := cfg.Setting(backend.Settings[0]); ok {
				entry.Section = section.Key
			}
		}
		for _, plan := range backend.Plans {
			// What the plan asks for is told by the setting it fills: a secret is an API key, any other the address of a server; a plan
			// that fills none and still asks is a sign-in
			needs := webv1.Needs_NEEDS_NOTHING
			switch _, setting, known := cfg.Setting(plan.Key); {
			case known && setting.Secret:
				needs = webv1.Needs_NEEDS_API_KEY
			case plan.Key != "":
				needs = webv1.Needs_NEEDS_ADDRESS
			case plan.Prompt != "":
				needs = webv1.Needs_NEEDS_SIGN_IN
			}
			entry.Plans = append(entry.Plans, &webv1.Plan{Title: plan.Title, Badge: plan.Badge, Description: plan.About, Tier: plan.Tier, Key: plan.Key,
				Prompt: plan.Prompt, Link: plan.Link, Needs: needs, SignIn: plan.SignIn})
		}
		response.Backends = append(response.Backends, entry)
	}
	return response, nil
}

// Uses of a backend as the request names them
var uses = map[webv1.Use]web.Use{webv1.Use_USE_BOTH: web.Both, webv1.Use_USE_SEARCH: web.Searches, webv1.Use_USE_PAGES: web.Pages}

func (s *service) Choose(_ context.Context, request *webv1.ChooseRequest) (*webv1.ChooseResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	note, err := s.backends.Choose(cfg, request.GetBackend(), uses[request.GetUse()], request.GetPlan())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &webv1.ChooseResponse{Note: note}, nil
}
