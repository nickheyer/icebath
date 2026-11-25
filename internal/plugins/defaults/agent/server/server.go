package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/server"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
)

type Agent interface {
	Send(ctx context.Context, sessionID, text string) error
	Stop(sessionID string) error
	Watch(ctx context.Context, sessionID string, send func(*agentv1.WatchResponse) error) error
	Close() error
}

type service struct {
	agent Agent
}

func Register(registry *server.Registry, agent Agent) {
	registry.Register(&service{agent: agent})
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
