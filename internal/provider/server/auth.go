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

// Account of the provider as a listing shows it: where its credential comes from or why it has none, what stands against it, whether
// requests go out with it now, the one named by using, and its place in the rotation
func (s *service) account(instance provider.Instance, name, using string) *providerv1.Account {
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
		Title:            instance.AccountTitle(name),
		Section:          account.Fields.Prefix(),
		Active:           using != "" && name == using,
		CredentialOrigin: origins[found.Origin],
		CredentialSource: found.Source,
		Auth:             string(signsIn(instance, found)),
		Missing:          s.credentials.Missing(account),
		Status:           state,
		Reason:           strings.TrimPrefix(reason, ": "),
		Identity:         found.Identity,
		Fingerprint:      credentials.Fingerprint(found.APIKey),
		Rotate:           account.Fields.GetBool(providerconfig.InRotation),
		Priority:         int32(account.Fields.GetInt(providerconfig.Priority)),
		KeyEnv:           account.Fields.GetString(providerconfig.KeyEnv),
		Requests:         int64(account.Fields.GetInt(providerconfig.Requests)),
		Renewable:        !found.Expires.IsZero(),
		Program:          account.Program(),
	}
	if !resets.IsZero() {
		message.Resets = timestamppb.New(resets)
	}
	if !found.Expires.IsZero() {
		message.Expires = timestamppb.New(found.Expires)
	}
	message.State, message.Standing = accountState(instance, message, found)
	return message
}

// Account of the provider by name; for none, the one that requests go out with now, else the one its choice names
func (s *service) named(profile, name, account string) (provider.Instance, credentials.Account, error) {
	instance, err := s.instance(profile, name)
	if err != nil {
		return instance, credentials.Account{}, err
	}
	if account != "" && !slices.Contains(instance.Accounts, account) {
		return instance, credentials.Account{}, connect.NewError(connect.CodeNotFound, instance.Label()+" has no account named "+account+".")
	}
	if account == "" {
		if using, _, err := s.credentials.Ready(instance, s.transports.Factories()); err == nil {
			return instance, using, nil
		}
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
	return &providerv1.RenewResponse{Account: s.account(instance, account.Name, s.inUse(instance))}, nil
}

func (s *service) Reset(_ context.Context, request *providerv1.ResetRequest) (*providerv1.ResetResponse, error) {
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	names := instance.Accounts
	if name := request.GetAccount(); name != "" {
		if !slices.Contains(names, name) {
			return nil, connect.NewError(connect.CodeNotFound, instance.Label()+" has no account named "+name+".")
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

func (s *service) RenameAccount(ctx context.Context, request *providerv1.RenameAccountRequest) (*providerv1.RenameAccountResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, err := s.instance(request.GetProfile(), request.GetProvider())
	if err != nil {
		return nil, err
	}
	name, err := s.providers.RenameAccount(ctx, cfg, instance.ID, request.GetAccount(), request.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if instance, err = s.instance(request.GetProfile(), instance.ID); err != nil {
		return nil, err
	}
	return &providerv1.RenameAccountResponse{Account: s.account(instance, name, s.inUse(instance))}, nil
}

// Whether the provider is ready, by the one test every screen and the agent use, and with what: how it signs in, the account and where its
// credential comes from, as whom, until when, and what its kind adds; why not when it is not
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
	using, found, err := s.credentials.Ready(instance, s.transports.Factories())
	if err == nil {
		var route provider.Route
		route, err = s.credentials.Route(instance, "")
		using = credentials.Carried(instance, route)
		found, _ = s.credentials.Credential(using)
		if err == nil {
			account = using
			response.LoggedIn = true
			detail("Sign-in", authWords[cmp.Or(signsIn(instance, found), provider.AuthNone)])
			if using.Name != "" {
				detail("Account", instance.AccountTitle(using.Name))
			}
			detail("Key from", found.Source)
			detail("Signed in as", found.Identity)
			if !found.Expires.IsZero() {
				detail("Expires", found.Expires.Local().Format("2006-01-02 15:04"))
			}
			detail("Address", route.BaseURL)
		}
	}
	if err != nil {
		response.Error = err.Error()
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
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	instance, account, err := s.named(request.GetProfile(), request.GetProvider(), request.GetAccount())
	if err != nil {
		return nil, err
	}
	variable := account.Fields.GetString(providerconfig.KeyEnv)
	if account.Name == "" || variable == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "This account doesn't read its API key from an environment variable, so there is nothing to keep.")
	}
	found, ok := account.Lookup(providerconfig.APIKeyField)
	if !ok || strings.TrimSpace(found.APIKey) == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, variable+" is empty, so there is nothing to keep.")
	}
	if err := s.providers.KeepKey(cfg, instance.ID, account.Name, strings.TrimSpace(found.APIKey)); err != nil {
		return nil, err
	}
	if instance, err = s.instance(request.GetProfile(), instance.ID); err != nil {
		return nil, err
	}
	return &providerv1.KeepKeyResponse{Account: s.account(instance, account.Name, s.inUse(instance))}, nil
}

// How a credential of the provider signs in: as it says, else as the provider's kind does, a key on a kind that needs none being an API
// key; empty for no credential
func signsIn(instance provider.Instance, found credentials.Credential) provider.AuthKind {
	switch {
	case found.APIKey == "" && found.Sign == nil:
		return ""
	case found.Origin == credentials.Unneeded:
		return provider.AuthNone
	case found.Auth != "":
		return found.Auth
	case instance.Spec.Auth == provider.AuthNone:
		return provider.AuthAPIKey
	}
	return instance.Spec.Auth
}

// How an account signs in, in words
var authWords = map[provider.AuthKind]string{provider.AuthAPIKey: "API key", provider.AuthOAuth: "Signed in to the provider",
	provider.AuthNone: "None needed", provider.AuthAWSSigV4: "AWS credentials", provider.AuthGCPOAuth: "Google Cloud credentials"}
