package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	"github.com/nickheyer/icebath/internal/server"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
)

type service struct {
	config  *config.Config
	secrets *secrets.Secrets
}

func Register(srv *server.Server, cfg *config.Config, host *secrets.Secrets) {
	srv.Register(&service{config: cfg, secrets: host})
}

func (s *service) Mount(services *connect.Server) {
	secretsv1connect.RegisterSecretServiceHandler(services, s)
}

// Every store of the profile with what the last fetch did
func (s *service) sources(cfg *config.Config, report secrets.Report) *secretsv1.SourcesResponse {
	done := map[string]secrets.SourceReport{}
	for _, source := range report.Sources {
		done[source.Name] = source
	}
	response := &secretsv1.SourcesResponse{Conflicts: report.Conflicts}
	for _, source := range s.secrets.Ordered(cfg) {
		spec := source.Spec()
		message := &secretsv1.Source{Name: spec.Name, Label: spec.Label, Shape: spec.Shape, Enabled: s.secrets.Enabled(cfg, spec.Name)}
		if got, ok := done[spec.Name]; ok {
			message.Applied, message.Existing, message.Claimed, message.Protected, message.Invalid = got.Applied, got.Existing, got.Claimed, got.Protected, got.Invalid
			message.Error, message.Kind, message.Warnings = got.Result.Error, string(got.Result.Kind), got.Result.Warnings
			message.Remedy = s.secrets.Remedy(cfg, spec, got.Result.Kind)
		}
		response.Sources = append(response.Sources, message)
	}
	return response
}

func (s *service) Sources(_ context.Context, request *secretsv1.SourcesRequest) (*secretsv1.SourcesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.sources(cfg, s.secrets.Report(cfg)), nil
}

func (s *service) Reload(_ context.Context, request *secretsv1.ReloadRequest) (*secretsv1.SourcesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.sources(cfg, s.secrets.Reset(cfg)), nil
}

func (s *service) Enable(_ context.Context, request *secretsv1.EnableRequest) (*secretsv1.SourcesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if _, ok := s.secrets.Source(request.GetName()); !ok {
		return nil, connect.NewError(connect.CodeNotFound, "unknown secret store: "+request.GetName())
	}
	if err := cfg.Set(secretsconfig.Of(request.GetName(), secretsconfig.Enabled), request.GetOn()); err != nil {
		return nil, err
	}
	return s.sources(cfg, s.secrets.Reset(cfg)), nil
}
