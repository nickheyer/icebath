package server

import (
	"cmp"
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/server"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
)

type service struct {
	config      *config.Config
	providers   *provider.Provider
	credentials *credentials.Credentials
	models      *models.Models
}

func Register(host *server.Server, cfg *config.Config, p *provider.Provider, c *credentials.Credentials, m *models.Models) {
	providerconfig.Register(cfg)
	host.Register(&service{config: cfg, providers: p, credentials: c, models: m})
}

func (s *service) Mount(services *connect.Server) {
	providerv1connect.RegisterProviderServiceHandler(services, s)
}

func (s *service) List(context.Context, *providerv1.ListRequest) (*providerv1.ListResponse, error) {
	response := &providerv1.ListResponse{}
	for _, profile := range s.providers.Profiles() {
		if profile.Hidden || providerconfig.IsRemoved(s.config, profile.Name) {
			continue
		}
		credential, _ := s.credentials.Credential(profile.Name)
		response.Providers = append(response.Providers, &providerv1.Provider{
			Name:             profile.Name,
			DisplayName:      profile.Label(),
			Description:      profile.Description,
			SignupUrl:        profile.SignupURL,
			BaseUrl:          cmp.Or(credential.BaseURL, profile.BaseURL),
			ApiMode:          string(profile.Mode),
			DefaultModel:     profile.DefaultModel,
			Aliases:          profile.Aliases,
			Local:            profile.Local,
			Aggregator:       profile.Aggregator,
			CredentialSource: credential.Source,
			KeyEnv:           profile.KeyEnvNames(),
		})
	}
	return response, nil
}

func (s *service) Models(ctx context.Context, request *providerv1.ModelsRequest) (*providerv1.ModelsResponse, error) {
	profile, ok := s.providers.Profile(request.GetProvider())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "unknown provider "+request.GetProvider())
	}
	route, err := s.credentials.Route(profile, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	ids, ok := s.models.Models(ctx, route)
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, "provider "+profile.Name+" did not list its models")
	}
	return &providerv1.ModelsResponse{Models: ids}, nil
}
