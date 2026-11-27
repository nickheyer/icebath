package server

import (
	"cmp"
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/approvals"
	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1/approvalsv1connect"
)

type service struct {
	approvals *approvals.Approvals
	config    *config.Config
	sessions  *sessions.Sessions
	terminal  *terminal.Terminal
	tools     *tools.Registry
}

func Register(host *server.Server, a *approvals.Approvals, cfg *config.Config, s *sessions.Sessions, tm *terminal.Terminal, toolset *tools.Registry) {
	host.Register(&service{approvals: a, config: cfg, sessions: s, terminal: tm, tools: toolset})
}

func (s *service) Mount(services *connect.Server) {
	approvalsv1connect.RegisterApprovalsServiceHandler(services, s)
}

func (s *service) Test(_ context.Context, request *approvalsv1.TestRequest) (*approvalsv1.TestResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	backend, found := s.terminal.Named(cfg, request.GetBackend())
	if request.GetBackend() == "" {
		if backend, err = s.terminal.Configured(cfg); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
		}
	} else if !found {
		return nil, connect.NewError(connect.CodeInvalidArgument, "No terminal backend named "+request.GetBackend()+".")
	}
	e := s.approvals.Evaluate(cfg, "", request.GetCommand(), backend.Name, backend.Isolated)
	return &approvalsv1.TestResponse{Command: e.Command, Backend: e.Backend, Verdict: e.Verdict, ExitCode: int32(e.Code), Rule: e.Rule,
		Detail: e.Detail, Variants: e.Variants}, nil
}

func (s *service) Suggest(ctx context.Context, request *approvalsv1.SuggestRequest) (*approvalsv1.SuggestResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	store, err := s.sessions.Store(cmp.Or(request.GetStore(), cfg.GetString(sessionsconfig.Store)))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	days := 90
	if request.Days != nil {
		days = int(request.GetDays())
	}
	proposals, err := s.approvals.Suggest(ctx, cfg, store, days, cmp.Or(int(request.GetMinCount()), 2), cmp.Or(int(request.GetLimit()), 20))
	if err != nil {
		return nil, err
	}
	response := &approvalsv1.SuggestResponse{Days: int32(days), Allowed: int32(len(cfg.GetStringSlice(approvalsconfig.Allowlist)))}
	for _, p := range proposals {
		response.Proposals = append(response.Proposals, &approvalsv1.Proposal{Pattern: p.Pattern, Kind: p.Kind, Count: int32(p.Count), Classes: p.Classes,
			Examples: p.Examples})
	}
	return response, nil
}

func (s *service) List(_ context.Context, request *approvalsv1.ListRequest) (*approvalsv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &approvalsv1.ListResponse{Allowed: cfg.GetStringSlice(approvalsconfig.Allowlist)}
	for _, kind := range s.tools.Risks() {
		response.Kinds = append(response.Kinds, &approvalsv1.Kind{Value: kind.Value, About: kind.About})
	}
	return response, nil
}

func (s *service) Allow(_ context.Context, request *approvalsv1.AllowRequest) (*approvalsv1.AllowResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	added, size, err := s.approvals.Allow(cfg, request.GetPatterns())
	if err != nil {
		return nil, err
	}
	return &approvalsv1.AllowResponse{Added: added, Size: int32(size)}, nil
}

func (s *service) Disallow(_ context.Context, request *approvalsv1.DisallowRequest) (*approvalsv1.DisallowResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.approvals.Disallow(cfg, request.GetPatterns()); err != nil {
		return nil, err
	}
	return &approvalsv1.DisallowResponse{}, nil
}
