package server

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/server"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
)

// Most characters of a refusal shown with an account
const reasonLength = 160

// Account of the provider as a listing shows it: where its credential comes from, what stands against it and its place in the rotation
func (s *service) account(instance provider.Instance, name string) *providerv1.Account {
	account := credentials.Of(instance, name)
	found, _ := s.credentials.Credential(account)
	state, resets := s.credentials.State(account)
	status := credentials.StatusOf(account)
	reason := status.Reason
	if message := strings.Join(strings.Fields(status.Message), " "); message != "" {
		if len(message) > reasonLength {
			message = message[:reasonLength] + "…"
		}
		reason = strings.TrimSpace(reason + ": " + message)
	}
	message := &providerv1.Account{
		Name:             name,
		Title:            cmp.Or(account.Fields.GetString(providerconfig.Title), name),
		Section:          account.Fields.Prefix(),
		Active:           name == instance.Account,
		CredentialOrigin: origins[found.Origin],
		CredentialSource: found.Source,
		Auth:             string(cmp.Or(found.Auth, instance.Spec.Auth)),
		Status:           state,
		Reason:           strings.TrimPrefix(reason, ": "),
		Identity:         found.Identity,
		Fingerprint:      credentials.Fingerprint(found.APIKey),
		Rotate:           account.Fields.GetBool(providerconfig.InRotation),
		Priority:         int32(account.Fields.GetInt(providerconfig.Priority)),
		KeyEnv:           account.Fields.GetString(providerconfig.KeyEnv),
		Requests:         int64(account.Fields.GetInt(providerconfig.Requests)),
		Renewable:        !found.Expires.IsZero(),
	}
	if !resets.IsZero() {
		message.Resets = timestamppb.New(resets)
	}
	if !found.Expires.IsZero() {
		message.Expires = timestamppb.New(found.Expires)
	}
	return message
}

// Account of the provider by name, the one in use for none
func (s *service) named(profile, name, account string) (provider.Instance, credentials.Account, error) {
	instance, err := s.instance(profile, name)
	if err != nil {
		return instance, credentials.Account{}, err
	}
	if account != "" && !slices.Contains(instance.Accounts, account) {
		return instance, credentials.Account{}, connect.NewError(connect.CodeNotFound, instance.Label()+" has no account named "+account)
	}
	return instance, credentials.Of(instance, account), nil
}

func (s *service) Renew(ctx context.Context, request *providerv1.RenewRequest) (*providerv1.RenewResponse, error) {
	instance, account, err := s.named(request.GetProfile(), request.GetProvider(), request.GetAccount())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.credentials.Refresh(ctx, account, true); err != nil {
		code := connect.CodeFailedPrecondition
		if errors.Is(err, credentials.ErrNotRenewable) {
			code = connect.CodeUnimplemented
		}
		return nil, connect.NewError(code, err.Error())
	}
	if _, err := credentials.Reset(account); err != nil {
		return nil, err
	}
	instance, err = s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	if account.Name == "" {
		return &providerv1.RenewResponse{}, nil
	}
	return &providerv1.RenewResponse{Account: s.account(instance, account.Name)}, nil
}

func (s *service) Reset(_ context.Context, request *providerv1.ResetRequest) (*providerv1.ResetResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	names := instance.Accounts
	if name := request.GetAccount(); name != "" {
		if !slices.Contains(names, name) {
			return nil, connect.NewError(connect.CodeNotFound, instance.Label()+" has no account named "+name)
		}
		names = []string{name}
	}
	response := &providerv1.ResetResponse{}
	for _, name := range names {
		cleared, err := credentials.Reset(credentials.Of(instance, name))
		if err != nil {
			return nil, err
		}
		if cleared {
			response.Cleared++
		}
	}
	return response, nil
}

func (s *service) MoveAccount(_ context.Context, request *providerv1.MoveAccountRequest) (*providerv1.MoveAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	place, err := s.providers.MoveAccount(cfg, request.GetProvider(), request.GetAccount(), int(request.GetPriority()))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &providerv1.MoveAccountResponse{Priority: int32(place)}, nil
}

func (s *service) RenameAccount(_ context.Context, request *providerv1.RenameAccountRequest) (*providerv1.RenameAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.providers.RenameAccount(cfg, request.GetProvider(), request.GetAccount(), request.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	return &providerv1.RenameAccountResponse{Account: s.account(instance, strings.ToLower(strings.TrimSpace(request.GetName())))}, nil
}

// Whether the provider's account in use is signed in: how, from where, as whom, until when, and what its kind adds
func (s *service) Status(ctx context.Context, request *providerv1.StatusRequest) (*providerv1.StatusResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	account := credentials.Of(instance, "")
	response := &providerv1.StatusResponse{}
	detail := func(name, value string) {
		if value = strings.TrimSpace(value); value != "" {
			response.Details = append(response.Details, &providerv1.Detail{Name: name, Value: value})
		}
	}
	route, err := s.credentials.Route(instance, "")
	found, ok := s.credentials.Credential(credentials.Of(instance, route.Account))
	switch {
	case err != nil:
		response.Error = err.Error()
	case !ok && !instance.Spec.Local:
		response.Error = "no credential"
	default:
		response.LoggedIn = true
		account = credentials.Of(instance, route.Account)
		detail("auth_type", string(cmp.Or(found.Auth, instance.Spec.Auth)))
		detail("account", route.Account)
		detail("source", found.Source)
		detail("identity", found.Identity)
		if !found.Expires.IsZero() {
			detail("expires_at", found.Expires.UTC().Format(time.RFC3339))
		}
		detail("api_base_url", route.BaseURL)
	}
	reportCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if extra, ok := s.credentials.Report(reportCtx, account); ok {
		for _, d := range extra {
			detail(d.Name, d.Value)
		}
	}
	return response, nil
}

func (s *service) KeepKey(_ context.Context, request *providerv1.KeepKeyRequest) (*providerv1.KeepKeyResponse, error) {
	instance, account, err := s.named(request.GetProfile(), request.GetProvider(), request.GetAccount())
	if err != nil {
		return nil, err
	}
	variable := account.Fields.GetString(providerconfig.KeyEnv)
	if account.Name == "" || variable == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "The account doesn't read its key from an environment variable.")
	}
	found, ok := account.Lookup(providerconfig.APIKeyField)
	if !ok || strings.TrimSpace(found.APIKey) == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, variable+" is empty, nothing to keep.")
	}
	if err := account.Fields.Set(providerconfig.APIKeyField, strings.TrimSpace(found.APIKey)); err != nil {
		return nil, err
	}
	if err := account.Fields.Set(providerconfig.KeyEnv, nil); err != nil {
		return nil, err
	}
	return &providerv1.KeepKeyResponse{Account: s.account(instance, account.Name)}, nil
}
