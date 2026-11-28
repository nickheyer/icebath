package server

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
)

// Limits and abilities set for the models of the profile's providers, by provider and model, the entry for every model of a provider first;
// with the families of models a limit may name
func (s *service) Limits(_ context.Context, request *providerv1.LimitsRequest) (*providerv1.LimitsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &providerv1.LimitsResponse{Families: s.models.Families(cfg)}
	overrides, _ := cfg.Get(modelsconfig.Overrides).(map[string]any)
	for _, instance := range s.providers.Instances(cfg) {
		models, _ := overrides[instance.ID].(map[string]any)
		for _, model := range slices.SortedFunc(maps.Keys(models), func(a, b string) int {
			return strings.Compare(strings.TrimPrefix(a, modelsconfig.EveryModel), strings.TrimPrefix(b, modelsconfig.EveryModel))
		}) {
			entry, ok := models[model].(map[string]any)
			if !ok {
				continue
			}
			limit := limitOf(entry)
			limit.Provider, limit.ProviderTitle = instance.ID, instance.Label()
			if model != modelsconfig.EveryModel {
				limit.Model = model
			}
			response.Limits = append(response.Limits, limit)
		}
	}
	return response, nil
}

// Limit an entry of the overrides sets
func limitOf(entry map[string]any) *providerv1.Limit {
	limit := &providerv1.Limit{}
	if n, ok := config.Whole(entry[modelsconfig.OverrideContext]); ok && n > 0 {
		limit.ContextWindow = int64(n)
	}
	for field, into := range map[string]**bool{modelsconfig.OverrideTools: &limit.Tools, modelsconfig.OverrideVision: &limit.Vision,
		modelsconfig.OverrideReasoning: &limit.Reasoning} {
		if on, ok := entry[field].(bool); ok {
			*into = &on
		}
	}
	limit.Family, _ = entry[modelsconfig.OverrideFamily].(string)
	return limit
}

// Entry of the overrides a limit sets, nil when it sets nothing
func entryOf(limit *providerv1.Limit) map[string]any {
	entry := map[string]any{}
	if n := limit.GetContextWindow(); n > 0 {
		entry[modelsconfig.OverrideContext] = int(n)
	}
	for field, value := range map[string]*bool{modelsconfig.OverrideTools: limit.Tools, modelsconfig.OverrideVision: limit.Vision,
		modelsconfig.OverrideReasoning: limit.Reasoning} {
		if value != nil {
			entry[field] = *value
		}
	}
	if family := strings.TrimSpace(limit.GetFamily()); family != "" {
		entry[modelsconfig.OverrideFamily] = family
	}
	if len(entry) == 0 {
		return nil
	}
	return entry
}

// Sets the limits and abilities of a model of a provider, or of every model of it, each typed field checked on the way in; a limit that
// sets nothing removes the entry
func (s *service) SetLimit(ctx context.Context, request *providerv1.SetLimitRequest) (*providerv1.SetLimitResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	limit := request.GetLimit()
	instance, err := s.instance(request.GetProfile(), limit.GetProvider())
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(limit.GetModel())
	if model != "" {
		if err := s.checkModel(ctx, cfg, instance, model); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
	}
	if limit.GetContextWindow() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The context window can't be less than 0.")
	}
	if err := keepLimit(cfg, instance.ID, model, entryOf(limit)); err != nil {
		return nil, refusal(err)
	}
	shown := limitOf(entryOf(limit))
	shown.Provider, shown.Model, shown.ProviderTitle = instance.ID, model, instance.Label()
	return &providerv1.SetLimitResponse{Limit: shown}, nil
}

func (s *service) RemoveLimit(_ context.Context, request *providerv1.RemoveLimitRequest) (*providerv1.RemoveLimitResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	if err := keepLimit(cfg, instance.ID, strings.TrimSpace(request.GetModel()), nil); err != nil {
		return nil, refusal(err)
	}
	return &providerv1.RemoveLimitResponse{}, nil
}

// Keeps the entry of the overrides of a model of a provider, the one for every model of it for no model, nil taking it away, after which
// a provider with no entry left keeps none. The model is one name whatever it holds, as many models are named with a dot
func keepLimit(cfg *config.Config, provider, model string, entry map[string]any) error {
	name := cmp.Or(model, modelsconfig.EveryModel)
	if entry != nil {
		return cfg.SetNamed(modelsconfig.Overrides, entry, provider, name)
	}
	if err := cfg.SetNamed(modelsconfig.Overrides, nil, provider, name); err != nil {
		return err
	}
	if left, _ := cfg.Stored(modelsconfig.Overrides + "." + provider).(map[string]any); len(left) == 0 {
		return cfg.Set(modelsconfig.Overrides+"."+provider, nil)
	}
	return nil
}

// Whether the overrides keep an entry of the model of the provider
func limited(cfg *config.Config, provider, model string) bool {
	kept, _ := cfg.Stored(modelsconfig.Overrides + "." + provider).(map[string]any)
	return kept[model] != nil
}

// Lists of models of single kinds of provider, by kind
func (s *service) Catalogs(_ context.Context, request *providerv1.CatalogsRequest) (*providerv1.CatalogsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &providerv1.CatalogsResponse{}
	catalogs, _ := cfg.Get(modelsconfig.CatalogProviders).(map[string]any)
	for _, kind := range slices.Sorted(maps.Keys(catalogs)) {
		entry, _ := catalogs[kind].(map[string]any)
		address, _ := entry[catalogURL].(string)
		title := kind
		if spec, ok := s.providers.Spec(kind); ok {
			title = spec.Label()
		}
		response.Catalogs = append(response.Catalogs, &providerv1.Catalog{Kind: kind, Address: address, Title: title})
	}
	return response, nil
}

// Name of the address in an entry of the catalogs of single kinds
const catalogURL = "url"

func (s *service) SetCatalog(_ context.Context, request *providerv1.SetCatalogRequest) (*providerv1.SetCatalogResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	catalog := request.GetCatalog()
	address := strings.TrimSpace(catalog.GetAddress())
	if err := s.checkCatalog(catalog.GetKind(), map[string]any{catalogURL: address}); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := cfg.Set(modelsconfig.CatalogProviders+"."+strings.ToLower(catalog.GetKind()), map[string]any{catalogURL: address}); err != nil {
		return nil, refusal(err)
	}
	return &providerv1.SetCatalogResponse{}, nil
}

func (s *service) RemoveCatalog(_ context.Context, request *providerv1.RemoveCatalogRequest) (*providerv1.RemoveCatalogResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := cfg.Set(modelsconfig.CatalogProviders+"."+strings.ToLower(request.GetKind()), nil); err != nil {
		return nil, refusal(err)
	}
	return &providerv1.RemoveCatalogResponse{}, nil
}

// Why an entry of the catalogs of single kinds can't be used: a kind no plugin registers, an address that is no address
func (s *service) checkCatalog(kind string, entry any) error {
	spec, ok := s.providers.Spec(kind)
	if !ok || spec.Name != strings.ToLower(strings.TrimSpace(kind)) {
		return fmt.Errorf("There is no kind of provider named %s.", kind)
	}
	fields, ok := entry.(map[string]any)
	if !ok {
		return fmt.Errorf("The list of models of %s is given by its address alone.", spec.Label())
	}
	address, _ := fields[catalogURL].(string)
	if err := provider.Address(strings.TrimSpace(address)); err != nil {
		return err
	}
	return nil
}
