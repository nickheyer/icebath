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
	"github.com/nickheyer/icebath/internal/transport"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
)

var origins = map[credentials.Origin]providerv1.Origin{
	credentials.Saved:       providerv1.Origin_ORIGIN_SAVED,
	credentials.Environment: providerv1.Origin_ORIGIN_ENVIRONMENT,
	credentials.External:    providerv1.Origin_ORIGIN_EXTERNAL,
	credentials.Unneeded:    providerv1.Origin_ORIGIN_UNNEEDED,
}

type service struct {
	config      *config.Config
	providers   *provider.Provider
	transports  *transport.Transport
	credentials *credentials.Credentials
	models      *models.Models
}

func Register(host *server.Server, cfg *config.Config, p *provider.Provider, t *transport.Transport, c *credentials.Credentials, m *models.Models) {
	host.Register(&service{config: cfg, providers: p, transports: t, credentials: c, models: m})
}

func (s *service) Mount(services *connect.Server) {
	providerv1connect.RegisterProviderServiceHandler(services, s)
}

func (s *service) List(ctx context.Context, request *providerv1.ListRequest) (*providerv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	wired := s.transports.Factories()
	response := &providerv1.ListResponse{}
	var asked []provider.Route
	for _, instance := range s.providers.Instances(cfg) {
		message := s.message(cfg, instance, wired)
		if instance.Spec.AccountUsage != nil {
			if route, err := s.credentials.Route(instance, ""); err == nil {
				asked = append(asked, route)
				if reading := s.models.CachedUsage(cfg, route); !models.StaleUsage(reading) {
					message.AccountUsage = reading.Message()
				}
			}
		}
		response.Providers = append(response.Providers, message)
	}
	s.models.RefreshUsage(ctx, cfg, asked)
	return response, nil
}

func (s *service) message(cfg *config.Config, instance provider.Instance, wired map[provider.APIMode]provider.Factory) *providerv1.Provider {
	spec := instance.Spec
	_, usable := wired[spec.Mode]
	credential, _ := s.credentials.Credential(credentials.Of(instance, ""))
	message := &providerv1.Provider{
		Name:             instance.ID,
		Spec:             spec.Name,
		DisplayName:      instance.Label(),
		Description:      spec.Description,
		SignupUrl:        spec.SignupURL,
		BaseUrl:          cmp.Or(credential.BaseURL, spec.BaseURL),
		ApiMode:          string(spec.Mode),
		DefaultModel:     spec.DefaultModel,
		Models:           instance.Models,
		Aliases:          spec.Aliases,
		Local:            spec.Local,
		Aggregator:       spec.Aggregator,
		CredentialOrigin: origins[credential.Origin],
		CredentialSource: credential.Source,
		KeyEnv:           spec.KeyEnvNames(),
		Keyed:            spec.Keyed(),
		Section:          instance.Settings.Prefix(),
		Usable:           instance.Known && usable && spec.BaseURL != "",
		Account:          instance.Account,
		Known:            instance.Known,
		Hidden:           s.models.Hidden(cfg, instance.ID, spec.Name),
		Usage:            spec.AccountUsage != nil,
		Strategy:         instance.Settings.GetString(providerconfig.Strategy),
		OnExhausted:      instance.Settings.GetString(providerconfig.OnExhausted),
		Pinned:           instance.Pinned,
		Rotate:           instance.Rotate,
		Ignored:          instance.Settings.GetStringSlice(providerconfig.Ignored),
	}
	for _, name := range instance.Accounts {
		message.Accounts = append(message.Accounts, s.account(instance, name))
	}
	return message
}

func (s *service) Specs(_ context.Context, request *providerv1.SpecsRequest) (*providerv1.SpecsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	wired := s.transports.Factories()
	instances := s.providers.Instances(cfg)
	response := &providerv1.SpecsResponse{}
	for _, spec := range s.providers.Specs() {
		if spec.Hidden {
			continue
		}
		_, usable := wired[spec.Mode]
		detected, _ := s.credentials.Detect(cfg, spec)
		message := &providerv1.Spec{
			Name:           spec.Name,
			DisplayName:    spec.Label(),
			Description:    spec.Description,
			SignupUrl:      spec.SignupURL,
			ApiMode:        string(spec.Mode),
			Local:          spec.Local,
			Aggregator:     spec.Aggregator,
			Usable:         usable && (spec.BaseURL != "" || spec.Resolve != nil),
			DetectedOrigin: origins[detected.Origin],
			DetectedSource: detected.Source,
			KeyEnv:         spec.KeyEnvNames(),
			Aliases:        spec.Aliases,
			Keyed:          spec.Keyed(),
		}
		for _, instance := range instances {
			if instance.Spec.Name == spec.Name {
				message.Providers = append(message.Providers, instance.ID)
			}
		}
		response.Specs = append(response.Specs, message)
	}
	return response, nil
}

func (s *service) instance(profile, name string) (provider.Instance, error) {
	cfg, err := server.Profile(s.config, profile)
	if err != nil {
		return provider.Instance{}, err
	}
	instance, ok := s.providers.Instance(cfg, name)
	if !ok {
		return instance, connect.NewError(connect.CodeNotFound, "Profile "+cfg.Profile()+" has no provider named "+name)
	}
	return instance, nil
}

// Models the provider offers with what is known of each: their windows, prices and what they can do
func (s *service) Models(ctx context.Context, request *providerv1.ModelsRequest) (*providerv1.ModelsResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	route, err := s.credentials.Route(instance, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if request.GetRefresh() {
		s.models.RefreshCatalogs(cfg)
	}
	catalog := s.models.Catalog(ctx, cfg, route, request.GetRefresh())
	if len(catalog.IDs) == 0 {
		return nil, connect.NewError(connect.CodeUnavailable, instance.Label()+" didn't return a model list")
	}
	prices := s.models.Prices(ctx, cfg, route, request.GetRefresh())
	response := &providerv1.ModelsResponse{Models: catalog.IDs, Fallback: catalog.Fallback}
	for _, id := range catalog.IDs {
		model := route
		model.Model = id
		detail := &providerv1.Model{Id: id, Note: catalog.Notes[id]}
		if caps, ok := s.models.Capabilities(cfg, model, false); ok {
			detail.Context, detail.MaxOutput, detail.Reasoning = int64(caps.Context), int64(caps.MaxOutput), caps.Reasoning
		}
		// Efforts the model takes: those its kind declares, else those the provider's listing names
		if instance.Spec.Efforts != nil {
			if efforts, ok := instance.Spec.Efforts(ctx, id); ok {
				detail.Efforts = efforts
			}
		}
		if thinking, ok := s.models.Thinking(ctx, cfg, model); ok && thinking.Known {
			if len(detail.Efforts) == 0 {
				detail.Efforts = thinking.Efforts
			}
			detail.MandatoryReasoning = thinking.Mandatory
			if detail.Reasoning == nil {
				detail.Reasoning = new(thinking.Takes)
			}
		}
		if info, ok := s.models.Info(cfg, model); ok {
			detail.Abilities = info.Abilities()
		}
		if price, ok := s.models.ShownPrice(cfg, model, prices); ok {
			detail.InputPrice, detail.OutputPrice, detail.CacheReadPrice = price.Input, price.Output, price.CacheRead
		}
		response.Details = append(response.Details, detail)
	}
	return response, nil
}

// What the provider says the account in use may still spend, the reading remembered for its route
func (s *service) Usage(ctx context.Context, request *providerv1.UsageRequest) (*providerv1.UsageResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name := request.GetProvider()
	if name == "" {
		for _, instance := range s.providers.Instances(cfg) {
			if _, err := s.credentials.Route(instance, ""); err == nil && instance.Spec.AccountUsage != nil {
				name = instance.ID
				break
			}
		}
		if name == "" {
			return nil, connect.NewError(connect.CodeNotFound, "No connected provider of profile "+cfg.Profile()+" can say what its account may still spend")
		}
	}
	instance, err := s.instance(request.GetProfile(), name)
	if err != nil {
		return nil, err
	}
	if instance.Spec.AccountUsage == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, instance.Label()+" can't say what its accounts may still spend")
	}
	route, err := s.credentials.Route(instance, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	reading := s.models.Usage(ctx, cfg, route)
	if reading == nil {
		return &providerv1.UsageResponse{}, nil
	}
	return &providerv1.UsageResponse{Usage: reading.Message()}, nil
}

func (s *service) Add(_ context.Context, request *providerv1.AddRequest) (*providerv1.AddResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.providers.Add(cfg, request.GetSpec(), request.GetName(), request.GetTitle())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &providerv1.AddResponse{Provider: s.message(cfg, instance, s.transports.Factories())}, nil
}

func (s *service) Remove(_ context.Context, request *providerv1.RemoveRequest) (*providerv1.RemoveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.Remove(cfg, request.GetProvider()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.RemoveResponse{}, nil
}

func (s *service) AddAccount(_ context.Context, request *providerv1.AddAccountRequest) (*providerv1.AddAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name, err := s.providers.AddAccount(cfg, request.GetProvider(), request.GetName(), request.GetTitle())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	for _, account := range s.message(cfg, instance, nil).GetAccounts() {
		if account.GetName() == name {
			return &providerv1.AddAccountResponse{Account: account}, nil
		}
	}
	return nil, connect.NewError(connect.CodeInternal, "the account was added but can't be read")
}

func (s *service) RemoveAccount(_ context.Context, request *providerv1.RemoveAccountRequest) (*providerv1.RemoveAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if source := request.GetSource(); source != "" {
		cleared, err := s.providers.Ignore(cfg, request.GetProvider(), source)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err.Error())
		}
		return &providerv1.RemoveAccountResponse{Cleared: cleared}, nil
	}
	cleared, err := s.providers.RemoveAccount(cfg, request.GetProvider(), request.GetAccount())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.RemoveAccountResponse{Cleared: cleared}, nil
}

func (s *service) UseAccount(_ context.Context, request *providerv1.UseAccountRequest) (*providerv1.UseAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.UseAccount(cfg, request.GetProvider(), request.GetAccount()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.UseAccountResponse{}, nil
}
