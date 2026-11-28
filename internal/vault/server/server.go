package server

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/vault"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1/vaultv1connect"
)

type service struct {
	config *config.Config
	vault  *vault.Vault
}

func Register(srv *server.Server, cfg *config.Config, host *vault.Vault) {
	srv.Register(&service{config: cfg, vault: host})
}

func (s *service) Mount(services *connect.Server) {
	vaultv1connect.RegisterVaultServiceHandler(services, s)
}

// A failure of the vault the person reads as it is, others as they come
func refusal(err error) error {
	var shown *vault.Error
	var locked *vault.Locked
	if errors.As(err, &shown) || errors.As(err, &locked) {
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return err
}

func item(listed vault.Listed) *vaultv1.Item {
	return &vaultv1.Item{Handle: listed.ID, Source: listed.Source, Kind: listed.Kind, Label: listed.Label, Identifier: listed.Identifier, Origin: listed.Origin,
		Local: listed.Local, Otp: listed.OTP}
}

func field(f vault.Field) *vaultv1.Field {
	return &vaultv1.Field{Key: f.Key, Required: f.Required, Secret: f.Secret, Choices: f.Choices}
}

func key(view vault.KeyView) *vaultv1.Key {
	return &vaultv1.Key{Storage: view.Storage, Path: view.Path, Service: view.Service, Account: view.Account, Configured: view.Configured,
		Keychain: view.Keychain, KeychainLeft: view.Left}
}

func (s *service) List(ctx context.Context, request *vaultv1.ListRequest) (*vaultv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if request.GetBare() {
		response := &vaultv1.ListResponse{}
		items, err := s.vault.Store(cfg).Items()
		if err != nil {
			return nil, refusal(err)
		}
		for _, own := range items {
			response.Items = append(response.Items, item(vault.Listed{Item: own, Source: vault.OwnTitle, Local: true}))
		}
		for _, backend := range s.vault.Backends() {
			response.Sources = append(response.Sources, &vaultv1.Source{Name: backend.Name, DisplayName: backend.Title})
		}
		return response, nil
	}
	rows, locked, err := s.vault.Listing(ctx, cfg)
	if err != nil {
		return nil, refusal(err)
	}
	view, err := s.vault.Key(cfg)
	if err != nil {
		return nil, refusal(err)
	}
	response := &vaultv1.ListResponse{Locked: locked, Key: key(view)}
	for _, row := range rows {
		response.Items = append(response.Items, item(row))
	}
	for _, source := range s.vault.Sources(cfg) {
		response.Sources = append(response.Sources, &vaultv1.Source{Name: source.Name, DisplayName: source.Title, State: source.State, Locked: source.Locked,
			Ready: source.Ready})
	}
	for _, kind := range vault.Kinds {
		message := &vaultv1.Kind{Kind: kind}
		for _, each := range vault.KindFields(kind) {
			message.Fields = append(message.Fields, field(each))
		}
		response.Kinds = append(response.Kinds, message)
	}
	return response, nil
}

func (s *service) Add(_ context.Context, request *vaultv1.AddRequest) (*vaultv1.AddResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	added, err := s.vault.Add(cfg, request.GetKind(), request.GetLabel(), request.GetOrigin(), request.GetValues())
	if err != nil {
		return nil, refusal(err)
	}
	return &vaultv1.AddResponse{Item: item(vault.Listed{Item: added, Source: vault.OwnTitle, Local: true})}, nil
}

func (s *service) Item(ctx context.Context, request *vaultv1.ItemRequest) (*vaultv1.ItemResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	listed, fields, err := s.vault.Item(ctx, cfg, request.GetHandle())
	if err != nil {
		return nil, refusal(err)
	}
	response := &vaultv1.ItemResponse{Item: item(listed)}
	for _, shown := range fields {
		response.Values = append(response.Values, &vaultv1.Value{Field: field(shown.Field), Value: shown.Value, Set: shown.Set})
	}
	return response, nil
}

func (s *service) Change(ctx context.Context, request *vaultv1.ChangeRequest) (*vaultv1.AddResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	changed, err := s.vault.Change(ctx, cfg, request.GetHandle(), request.GetField(), request.GetValue())
	if err != nil {
		return nil, refusal(err)
	}
	return &vaultv1.AddResponse{Item: item(vault.Listed{Item: changed, Source: vault.OwnTitle, Local: true})}, nil
}

func (s *service) Remove(ctx context.Context, request *vaultv1.RemoveRequest) (*vaultv1.RemoveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	removed, err := s.vault.Remove(ctx, cfg, request.GetHandle())
	if err != nil {
		return nil, refusal(err)
	}
	return &vaultv1.RemoveResponse{Removed: removed}, nil
}

func (s *service) SetSource(_ context.Context, request *vaultv1.SetSourceRequest) (*vaultv1.SetSourceResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	backend, err := s.vault.SetSource(cfg, request.GetName(), request.GetOn())
	if err != nil {
		return nil, refusal(err)
	}
	return &vaultv1.SetSourceResponse{DisplayName: backend.Title}, nil
}

func (s *service) MoveKey(_ context.Context, request *vaultv1.MoveKeyRequest) (*vaultv1.Key, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	view, err := s.vault.MoveKey(cfg, request.GetTo())
	if err != nil {
		return nil, refusal(err)
	}
	return key(view), nil
}

func (s *service) Unlock(ctx context.Context, request *vaultv1.UnlockRequest) (*vaultv1.UnlockResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.vault.Unlock(ctx, cfg, request.GetName(), request.GetMasterPassword(), ""); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &vaultv1.UnlockResponse{}, nil
}

func (s *service) Lock(_ context.Context, request *vaultv1.LockRequest) (*vaultv1.LockResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if _, err := s.vault.Lock(cfg, request.GetName()); err != nil {
		return nil, refusal(err)
	}
	return &vaultv1.LockResponse{}, nil
}
