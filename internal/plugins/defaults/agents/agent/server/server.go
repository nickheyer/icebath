package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
)

type service struct {
	agent *Agent
}

func Register(
	registry *server.Registry,
	cfg *config.Config,
	providers *provider.Registry,
	transports *transport.Registry,
	creds *credentials.Registry,
	catalog *models.Registry,
	toolset *tools.Registry,
	stores *sessions.Registry,
	commands *slash.Registry,
) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	agent := &Agent{
		ctx:         ctx,
		cancel:      cancel,
		config:      cfg,
		providers:   providers,
		transports:  transports,
		credentials: creds,
		models:      catalog,
		tools:       toolset,
		sessions:    stores,
		slash:       commands,
		runs:        map[string]*run{},
	}
	registry.Register(&service{agent: agent})
	return agent
}

func (s *service) Mount(services *connect.Server) {
	agentv1connect.RegisterAgentServiceHandler(services, s)
}

func (s *service) Close() error {
	return s.agent.Close()
}

func (s *service) Send(ctx context.Context, request *agentv1.SendRequest) (*agentv1.SendResponse, error) {
	if err := s.agent.Send(ctx, request.GetSessionId(), request.GetText()); err != nil {
		return nil, err
	}
	return &agentv1.SendResponse{}, nil
}

func (s *service) Stop(_ context.Context, request *agentv1.StopRequest) (*agentv1.StopResponse, error) {
	if err := s.agent.Stop(request.GetSessionId()); err != nil {
		return nil, err
	}
	return &agentv1.StopResponse{}, nil
}

func (s *service) Watch(ctx context.Context, request *agentv1.WatchRequest, stream agentv1connect.AgentServiceWatchServerStream) error {
	return s.agent.Watch(ctx, request.GetSessionId(), stream.Send)
}

func (s *service) Approve(_ context.Context, request *agentv1.ApproveRequest) (*agentv1.ApproveResponse, error) {
	scope := ""
	switch request.GetScope() {
	case agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION:
		scope = "session"
	case agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS:
		scope = "always"
	}
	if err := s.agent.Approve(request.GetSessionId(), scope); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.ApproveResponse{}, nil
}

func (s *service) Deny(_ context.Context, request *agentv1.DenyRequest) (*agentv1.DenyResponse, error) {
	if err := s.agent.Deny(request.GetSessionId(), request.GetReason()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.DenyResponse{}, nil
}
