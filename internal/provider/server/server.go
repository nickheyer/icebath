package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/transport"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
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
	// What was last found on this computer for a kind of provider of a profile, and when, so the list of kinds does not look again for
	// what it just learned
	mu    sync.Mutex
	found map[string]finding
}

// What looking for a kind's credential on this computer found, and when it was done
type finding struct {
	credential credentials.Credential
	at         time.Time
}

func Register(host *server.Server, cfg *config.Config, p *provider.Provider, t *transport.Transport, c *credentials.Credentials, m *models.Models) {
	s := &service{config: cfg, providers: p, transports: t, credentials: c, models: m}
	s.guard()
	host.Register(s)
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
		if instance.Spec.AccountUsage != nil && message.GetReady() {
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

// Provider as a listing shows it: whether it is ready by the one test every screen and the agent use and why not, the account and the
// credential that requests go out with now, the credential found outside its accounts, and each account
func (s *service) message(cfg *config.Config, instance provider.Instance, wired map[provider.APIMode]provider.Factory) *providerv1.Provider {
	spec := instance.Spec
	_, usable := wired[spec.Mode]
	using, credential, unready := s.credentials.Ready(instance, wired)
	message := &providerv1.Provider{
		Name:         instance.ID,
		Spec:         spec.Name,
		DisplayName:  instance.Label(),
		Description:  spec.Description,
		SignupUrl:    spec.SignupURL,
		BaseUrl:      cmp.Or(credential.BaseURL, spec.BaseURL),
		ApiMode:      string(spec.Mode),
		DefaultModel: cmp.Or(spec.DefaultModel, s.models.DefaultModel(cfg, provider.Route{Provider: instance.ID, Spec: spec.Name})),
		Models:       instance.Models,
		Aliases:      spec.Aliases,
		Local:        spec.Local,
		Aggregator:   spec.Aggregator,
		KeyEnv:       spec.KeyEnvNames(),
		Keyed:        spec.Keyed(),
		Section:      instance.Settings.Prefix(),
		Usable:       instance.Known && usable && cmp.Or(credential.BaseURL, spec.BaseURL) != "",
		Account:      instance.Account,
		Known:        instance.Known,
		Hidden:       !instance.Offered(cfg),
		Usage:        spec.AccountUsage != nil,
		Strategy:     instance.Settings.GetString(providerconfig.Strategy),
		OnExhausted:  instance.Settings.GetString(providerconfig.OnExhausted),
		Pinned:       instance.Pinned,
		Rotate:       instance.Rotate,
		Ignored:      instance.Settings.GetStringSlice(providerconfig.Ignored),
		Ready:        unready == nil,
		Own:          instance.Own,
		SignsIn:      spec.SignIn,
	}
	if worded, ok := errors.AsType[*credentials.Unready](unready); ok {
		message.Instead, message.InsteadOf = worded.Instead, worded.Of
	}
	switch {
	case unready != nil:
		message.Why = unready.Error()
	case credential.APIKey == "" && credential.Sign == nil:
		message.CredentialOrigin, message.Using = providerv1.Origin_ORIGIN_UNNEEDED, using.Name
	default:
		message.CredentialOrigin, message.CredentialSource, message.Using = origins[credential.Origin], credential.Source, using.Name
	}
	if outside, ok := s.credentials.Credential(credentials.Outside(instance)); ok && (outside.APIKey != "" || outside.Sign != nil) {
		message.OutsideOrigin, message.OutsideSource = origins[outside.Origin], outside.Source
		message.OutsideUsed = unready == nil && using.Name == "" && (credential.APIKey != "" || credential.Sign != nil)
	}
	for _, name := range instance.Accounts {
		message.Accounts = append(message.Accounts, s.account(instance, name, message.GetUsing()))
	}
	message.State, message.Standing = providerState(instance, message, credential, unready)
	return message
}

// Account that requests to the provider go out with now, empty for the credential outside its accounts and while it is not ready
func (s *service) inUse(instance provider.Instance) string {
	using, _, err := s.credentials.Ready(instance, s.transports.Factories())
	if err != nil {
		return ""
	}
	return using.Name
}

func (s *service) Specs(_ context.Context, request *providerv1.SpecsRequest) (*providerv1.SpecsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	wired := s.transports.Factories()
	instances := s.providers.Instances(cfg)
	response := &providerv1.SpecsResponse{}
	specs := slices.DeleteFunc(s.providers.Specs(), func(spec provider.Spec) bool { return spec.Hidden })
	found := s.detect(cfg, specs)
	for i, spec := range specs {
		_, usable := wired[spec.Mode]
		detected := found[i]
		message := &providerv1.Spec{
			Name:           spec.Name,
			DisplayName:    spec.Label(),
			Description:    spec.Description,
			SignupUrl:      spec.SignupURL,
			ApiMode:        string(spec.Mode),
			Local:          spec.Local,
			Aggregator:     spec.Aggregator,
			Usable:         usable,
			DetectedOrigin: origins[detected.Origin],
			DetectedSource: detected.Source,
			KeyEnv:         spec.KeyEnvNames(),
			Aliases:        spec.Aliases,
			Keyed:          spec.Keyed(),
			Rank:           uint32(max(spec.Rank, 0)),
			SignsIn:        spec.SignIn,
			Own:            spec.Own(),
			NeedsAddress:   spec.BaseURL == "",
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

// Longest the list of kinds waits to learn what is already on this computer for them, and how long what it learned stays good
const (
	detectWait = 1500 * time.Millisecond
	detectKeep = time.Minute
)

// What is found on this computer for each of the kinds, all looked for at once: looking for a cloud sign-in asks services that can take
// seconds to say no, and the list of kinds does not wait for them. A kind still being looked for when the wait is over counts as not
// found now, and what the look finds is kept for the next list
func (s *service) detect(cfg *config.Config, specs []provider.Spec) []credentials.Credential {
	type answer struct {
		at    int
		found credentials.Credential
	}
	out := make([]credentials.Credential, len(specs))
	answers := make(chan answer, len(specs))
	waiting := 0
	s.mu.Lock()
	if s.found == nil {
		s.found = map[string]finding{}
	}
	for i, spec := range specs {
		key := cfg.Profile() + "\x00" + spec.Name
		if kept, ok := s.found[key]; ok && time.Since(kept.at) < detectKeep {
			out[i] = kept.credential
			continue
		}
		waiting++
		go func() {
			found, _ := s.credentials.Detect(cfg, spec)
			s.mu.Lock()
			s.found[key] = finding{found, time.Now()}
			s.mu.Unlock()
			answers <- answer{i, found}
		}()
	}
	s.mu.Unlock()
	deadline := time.After(detectWait)
	for range waiting {
		select {
		case a := <-answers:
			out[a.at] = a.found
		case <-deadline:
			return out
		}
	}
	return out
}

func (s *service) instance(profile, name string) (provider.Instance, error) {
	cfg, err := server.Profile(s.config, profile)
	if err != nil {
		return provider.Instance{}, err
	}
	instance, ok := s.providers.Instance(cfg, name)
	if !ok {
		return instance, connect.NewError(connect.CodeNotFound, "The profile "+cfg.Profile()+" has no provider named "+name+".")
	}
	return instance, nil
}

// Models the provider offers with what is known of each: their windows, prices and what they can do; with why it gave no list of its own
// when it gave none. Refused while the provider is not ready, saying why
func (s *service) Models(ctx context.Context, request *providerv1.ModelsRequest) (*providerv1.ModelsResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if _, _, err := s.credentials.Ready(instance, s.transports.Factories()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	route, err := s.credentials.Route(instance, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if request.GetRefresh() {
		s.models.RefreshCatalogs(cfg)
	}
	catalog := s.models.Catalog(ctx, cfg, route, request.GetRefresh())
	prices := s.models.Prices(ctx, cfg, route, request.GetRefresh())
	response := &providerv1.ModelsResponse{Models: catalog.IDs, Fallback: catalog.Fallback}
	switch {
	case catalog.Unlisted != nil:
		response.Unlisted = catalog.Unlisted.Reason(instance.Label())
	case len(catalog.IDs) == 0:
		response.Unlisted = instance.Label() + " gives no list of its models that icebath can read."
	}
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
			return nil, connect.NewError(connect.CodeNotFound, "No connected provider of the profile "+cfg.Profile()+" can say how much its account may still use.")
		}
	}
	instance, err := s.instance(request.GetProfile(), name)
	if err != nil {
		return nil, err
	}
	if instance.Spec.AccountUsage == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, instance.Label()+" can't say how much its accounts may still use.")
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
	instance, err := s.providers.Add(cfg, request.GetSpec(), request.GetName(), request.GetTitle(), request.GetBaseUrl())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &providerv1.AddResponse{Provider: s.message(cfg, instance, s.transports.Factories())}, nil
}

func (s *service) Remove(ctx context.Context, request *providerv1.RemoveRequest) (*providerv1.RemoveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	removed, err := s.providers.Remove(ctx, cfg, instance.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &providerv1.RemoveResponse{Cleared: removed.Variables, Named: removed.Named, Said: said("Removed "+instance.Label()+".", removed)}, nil
}

// What a removal did in a sentence or two: the act, then what it cleared from the keys file and what named the thing removed
func said(done string, removed provider.Removed) string {
	if len(removed.Variables) > 0 {
		done = strings.TrimSuffix(done, ".") + " and cleared " + strs.AndMore(removed.Variables, 4) + " from your keys file."
	}
	if cleared := provider.Cleared(removed.Named); cleared != "" {
		done += " " + cleared
	}
	return done
}

func (s *service) Rename(ctx context.Context, request *providerv1.RenameRequest) (*providerv1.RenameResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	to, err := s.providers.Rename(ctx, cfg, instance.ID, request.GetTitle())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	renamed, err := s.instance(request.GetProfile(), to)
	if err != nil {
		return nil, err
	}
	return &providerv1.RenameResponse{Provider: s.message(cfg, renamed, s.transports.Factories())}, nil
}

func (s *service) Connect(_ context.Context, request *providerv1.ConnectRequest) (*providerv1.ConnectResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	for name, text := range request.GetFields() {
		var value any
		if err := yaml.Unmarshal([]byte(text), &value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("The value given for %s can't be read: %s.", name,
				strings.TrimPrefix(err.Error(), "yaml: ")))
		}
		fields[name] = value
	}
	account := cmp.Or(request.GetAccount(), s.signedAs(instance, request.GetIdentity()))
	before, settings := instance.Settings.Stored(""), instance.Settings
	name, err := s.providers.Connect(cfg, instance.ID, account, request.GetTitle(), request.GetIdentity(), fields)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if instance, err = s.instance(request.GetProfile(), instance.ID); err != nil {
		return nil, err
	}
	if !slices.Contains(instance.Accounts, name) {
		return nil, connect.NewError(connect.CodeInternal, "The account was kept, but icebath can't read it back.")
	}
	// The sign-in of another program is read where the server runs, with the account's own environment, which may not be where the person
	// asked for it: an account that would use one the server does not find is not kept, the provider put back as it was
	if program, _ := fields[providerconfig.Program].(string); strings.TrimSpace(program) != "" {
		if missing := s.credentials.Missing(credentials.Of(instance, name)); missing != "" {
			if err := settings.Replace(before); err != nil {
				return nil, err
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition, missing)
		}
	}
	return &providerv1.ConnectResponse{Account: s.account(instance, name, s.inUse(instance))}, nil
}

// Account of the provider that signs in as the identity, which a sign-in as the same one connects again rather than adding a second;
// empty for none or no identity
func (s *service) signedAs(instance provider.Instance, identity string) string {
	if identity = strings.TrimSpace(identity); identity == "" {
		return ""
	}
	for _, name := range instance.Accounts {
		if found, _ := s.credentials.Credential(credentials.Of(instance, name)); strings.EqualFold(found.Identity, identity) {
			return name
		}
	}
	return ""
}

func (s *service) RemoveAccount(ctx context.Context, request *providerv1.RemoveAccountRequest) (*providerv1.RemoveAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	title := instance.AccountTitle(request.GetAccount())
	removed, err := s.providers.RemoveAccount(ctx, cfg, instance.ID, request.GetAccount())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.RemoveAccountResponse{Cleared: removed.Variables, Named: removed.Named, Said: said("Removed the account "+title+".", removed)}, nil
}

func (s *service) UseAccount(_ context.Context, request *providerv1.UseAccountRequest) (*providerv1.UseAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.UseAccount(cfg, request.GetProvider(), request.GetAccount()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &providerv1.UseAccountResponse{}, nil
}

func (s *service) SetTurns(_ context.Context, request *providerv1.SetTurnsRequest) (*providerv1.SetTurnsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	if err := s.providers.SetTurns(cfg, instance.ID, request.GetAccount(), request.GetOn()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	title := instance.AccountTitle(request.GetAccount())
	if request.GetOn() {
		return &providerv1.SetTurnsResponse{Said: title + " now takes turns with the other accounts of " + instance.Label() + " that take turns."}, nil
	}
	return &providerv1.SetTurnsResponse{Said: title + " no longer takes turns."}, nil
}

func (s *service) SetOutside(_ context.Context, request *providerv1.SetOutsideRequest) (*providerv1.SetOutsideResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	// A source is turned by the variable or program it names, whichever store supplied the variable
	source := credentials.SourceName(request.GetSource())
	if _, err := s.providers.SetOutside(cfg, instance.ID, source, request.GetOn()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	what := credentials.Words(credentials.Credential{Origin: credentials.External, Source: source})
	if config.EnvName(source) {
		what = credentials.Words(credentials.Credential{Origin: credentials.Environment, Source: source})
	}
	if request.GetOn() {
		return &providerv1.SetOutsideResponse{Said: instance.Label() + " uses " + what + " again."}, nil
	}
	return &providerv1.SetOutsideResponse{Said: instance.Label() + " no longer uses " + what + "."}, nil
}

func (s *service) SetOffered(_ context.Context, request *providerv1.SetOfferedRequest) (*providerv1.SetOfferedResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	if err := s.providers.SetOffered(cfg, instance.ID, request.GetOn()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if request.GetOn() {
		return &providerv1.SetOfferedResponse{Said: "The model list offers the models of " + instance.Label() + "."}, nil
	}
	return &providerv1.SetOfferedResponse{Said: "The model list no longer offers the models of " + instance.Label() + "."}, nil
}
