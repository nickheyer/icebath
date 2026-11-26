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

// Settings of the profile a request names, the one in use when it names none
func (s *service) in(profile string) (*config.Config, error) {
	if profile != "" && !s.config.Exists(profile) {
		return nil, connect.NewError(connect.CodeNotFound, "No profile named "+profile)
	}
	return s.config.In(profile), nil
}

func (s *service) List(_ context.Context, request *providerv1.ListRequest) (*providerv1.ListResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	wired := s.transports.Factories()
	response := &providerv1.ListResponse{}
	for _, instance := range s.providers.Instances(cfg) {
		response.Providers = append(response.Providers, s.message(instance, wired))
	}
	return response, nil
}

func (s *service) message(instance provider.Instance, wired map[provider.APIMode]provider.Factory) *providerv1.Provider {
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
	}
	for _, name := range instance.Accounts {
		account := credentials.Of(instance, name)
		found, _ := s.credentials.Credential(account)
		message.Accounts = append(message.Accounts, &providerv1.Account{
			Name:             name,
			Title:            cmp.Or(account.Fields.GetString(providerconfig.Title), name),
			Section:          account.Fields.Prefix(),
			Active:           name == instance.Account,
			CredentialOrigin: origins[found.Origin],
			CredentialSource: found.Source,
		})
	}
	return message
}

func (s *service) Specs(_ context.Context, request *providerv1.SpecsRequest) (*providerv1.SpecsResponse, error) {
	cfg, err := s.in(request.GetProfile())
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
		detected, _ := s.credentials.Detect(spec)
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
	cfg, err := s.in(profile)
	if err != nil {
		return provider.Instance{}, err
	}
	instance, ok := s.providers.Instance(cfg, name)
	if !ok {
		return instance, connect.NewError(connect.CodeNotFound, "Profile "+cfg.Profile()+" has no provider named "+name)
	}
	return instance, nil
}

// Models of the provider as it lists them now
func (s *service) Models(ctx context.Context, request *providerv1.ModelsRequest) (*providerv1.ModelsResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	route, err := s.credentials.Route(instance, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	ids, ok := s.models.Models(ctx, route)
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, instance.Label()+" didn't return a model list")
	}
	return &providerv1.ModelsResponse{Models: ids}, nil
}

func (s *service) Add(_ context.Context, request *providerv1.AddRequest) (*providerv1.AddResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.providers.Add(cfg, request.GetSpec(), request.GetName(), request.GetTitle())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &providerv1.AddResponse{Provider: s.message(instance, s.transports.Factories())}, nil
}

func (s *service) Remove(_ context.Context, request *providerv1.RemoveRequest) (*providerv1.RemoveResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.Remove(cfg, request.GetProvider()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.RemoveResponse{}, nil
}

func (s *service) AddAccount(_ context.Context, request *providerv1.AddAccountRequest) (*providerv1.AddAccountResponse, error) {
	cfg, err := s.in(request.GetProfile())
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
	for _, account := range s.message(instance, nil).GetAccounts() {
		if account.GetName() == name {
			return &providerv1.AddAccountResponse{Account: account}, nil
		}
	}
	return nil, connect.NewError(connect.CodeInternal, "the account was added but can't be read")
}

func (s *service) RemoveAccount(_ context.Context, request *providerv1.RemoveAccountRequest) (*providerv1.RemoveAccountResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.RemoveAccount(cfg, request.GetProvider(), request.GetAccount()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.RemoveAccountResponse{}, nil
}

func (s *service) UseAccount(_ context.Context, request *providerv1.UseAccountRequest) (*providerv1.UseAccountResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.UseAccount(cfg, request.GetProvider(), request.GetAccount()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.UseAccountResponse{}, nil
}
