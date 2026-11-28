package server

import (
	"context"
	"maps"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	"github.com/nickheyer/icebath/internal/server"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
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

// Refusal of a change in a person's words
func refuse(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err.Error())
}

// Every store of the profile in the order they are asked, how each stands and what the last fetch did
func (s *service) sources(ctx context.Context, cfg *config.Config, report secrets.Report) *secretsv1.SourcesResponse {
	done := map[string]secrets.SourceReport{}
	for _, source := range report.Sources {
		done[source.Name] = source
	}
	response := &secretsv1.SourcesResponse{Conflicts: report.Conflicts}
	for _, source := range s.secrets.Asked(cfg) {
		spec := source.Spec()
		state := s.secrets.State(ctx, cfg, source)
		message := &secretsv1.Source{Name: spec.Name, Label: spec.Label, Shape: spec.Shape, Enabled: state.On, Needs: state.Needs, Lacks: state.Lacks,
			Fills: int32(state.Fills), Section: secretsconfig.Section(spec.Name), Scheme: spec.Scheme, Example: spec.Example, Find: spec.Find, TokenEnv: state.TokenEnv,
			TokenSet: state.TokenSet}
		if spec.Shape == secrets.Mapped {
			bindings := s.secrets.Bindings(cfg, spec.Name)
			for _, variable := range slices.Sorted(maps.Keys(bindings)) {
				message.Bindings = append(message.Bindings, &secretsv1.Binding{Variable: variable, Reference: bindings[variable]})
			}
		}
		if got, ok := done[spec.Name]; ok {
			message.Applied, message.Existing, message.Claimed, message.Protected, message.Invalid = got.Applied, got.Existing, got.Claimed, got.Protected, got.Invalid
			message.Error, message.Kind, message.Warnings = got.Result.Error, string(got.Result.Kind), got.Result.Warnings
			message.Remedy = s.secrets.Remedy(cfg, spec, got.Result.Kind)
		}
		response.Sources = append(response.Sources, message)
	}
	return response
}

func (s *service) Sources(ctx context.Context, request *secretsv1.SourcesRequest) (*secretsv1.SourcesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if request.GetBare() {
		response := &secretsv1.SourcesResponse{}
		for _, source := range s.secrets.Asked(cfg) {
			spec := source.Spec()
			response.Sources = append(response.Sources, &secretsv1.Source{Name: spec.Name, Label: spec.Label, Shape: spec.Shape,
				Section: secretsconfig.Section(spec.Name)})
		}
		return response, nil
	}
	return s.sources(ctx, cfg, s.secrets.Report(cfg)), nil
}

func (s *service) Reload(ctx context.Context, request *secretsv1.ReloadRequest) (*secretsv1.SourcesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.sources(ctx, cfg, s.secrets.Reset(cfg)), nil
}

// Label of the store of the name, the name itself for none
func (s *service) label(name string) string {
	if source, ok := s.secrets.Source(name); ok {
		return source.Spec().Label
	}
	return name
}

func (s *service) Switch(_ context.Context, request *secretsv1.SwitchRequest) (*secretsv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.secrets.Switch(cfg, request.GetName(), request.GetOn()); err != nil {
		return nil, refuse(err)
	}
	if request.GetOn() {
		return &secretsv1.ChangeResponse{Note: s.label(request.GetName()) + " is now On."}, nil
	}
	return &secretsv1.ChangeResponse{Note: s.label(request.GetName()) + " is now Off."}, nil
}

func (s *service) Bind(_ context.Context, request *secretsv1.BindRequest) (*secretsv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name := request.GetName()
	if err := s.secrets.Bind(cfg, name, request.GetVariable(), request.GetReference()); err != nil {
		return nil, refuse(err)
	}
	note := s.label(name) + " fills " + request.GetVariable() + " now."
	if !s.secrets.Enabled(cfg, name) {
		note = s.label(name) + " fills " + request.GetVariable() + " once it is on."
	}
	return &secretsv1.ChangeResponse{Note: note}, nil
}

func (s *service) Unbind(_ context.Context, request *secretsv1.UnbindRequest) (*secretsv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	removed, err := s.secrets.Unbind(cfg, request.GetName(), request.GetVariable())
	if err != nil {
		return nil, refuse(err)
	}
	if !removed {
		return nil, connect.NewError(connect.CodeNotFound, s.label(request.GetName())+" does not fill "+request.GetVariable()+".")
	}
	return &secretsv1.ChangeResponse{Note: s.label(request.GetName()) + " no longer fills " + request.GetVariable() + "."}, nil
}

func (s *service) Reorder(_ context.Context, request *secretsv1.ReorderRequest) (*secretsv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.secrets.Reorder(cfg, request.GetNames()); err != nil {
		return nil, refuse(err)
	}
	var labels []string
	for _, source := range s.secrets.Asked(cfg) {
		labels = append(labels, source.Spec().Label)
	}
	return &secretsv1.ChangeResponse{Note: "The stores are now asked in this order: " + strs.And(labels) + "."}, nil
}

func (s *service) Forget(_ context.Context, request *secretsv1.ForgetRequest) (*secretsv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	removed, err := s.secrets.Forget(cfg, request.GetName())
	if err != nil {
		return nil, refuse(err)
	}
	if !removed {
		return nil, connect.NewError(connect.CodeNotFound, "Your keys file holds no token of "+s.label(request.GetName())+".")
	}
	return &secretsv1.ChangeResponse{Note: "Removed the token of " + s.label(request.GetName()) + " from your keys file."}, nil
}

func (s *service) Preview(ctx context.Context, request *secretsv1.PreviewRequest) (*secretsv1.PreviewResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	outcomes, warnings, err := s.secrets.Preview(ctx, cfg, request.GetName(), request.GetApply())
	if err != nil {
		return nil, refuse(err)
	}
	response := &secretsv1.PreviewResponse{Warnings: warnings}
	for _, outcome := range outcomes {
		response.Outcomes = append(response.Outcomes, &secretsv1.Outcome{Name: outcome.Name, Action: outcome.Action})
	}
	return response, nil
}
