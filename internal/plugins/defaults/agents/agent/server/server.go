package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/delegation"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/internal/worktrees"
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
	helpers *auxiliary.Registry,
	prompts *prompt.Registry,
	checks *approvals.Registry,
	searches *web.Registry,
	skillset *skills.Registry,
	engines *contexts.Registry,
	shells *terminal.Registry,
	trees *worktrees.Registry,
	backends *delegation.Registry,
	desktops *botdesktop.Registry,
	points *hooks.Registry,
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
		auxiliary:   helpers,
		prompts:     prompts,
		approvals:   checks.Host(),
		web:         searches,
		skills:      skillset,
		contexts:    engines.Host(),
		slots:       make(chan struct{}, compressionSlots),
		terminal:    shells.Host(),
		learned:     auxiliary.NewState(),
		life:        newAgentLife(),
		runs:        map[string]*run{},
		worktrees:   trees.Host(),
		delegation:  backends,
		tree:        newSpawnTree(),
		hooks:       points.Host(),
		ended:       map[string]bool{},
		commands:    map[string]*observing{},
	}
	agent.async = newBackground(agent)
	stores.OnStart(agent.serve)
	stores.OnRelease(func(_ context.Context, session string) { agent.async.stop(session) })
	registry.Register(&service{agent: agent})
	skillset.Register(runner{agent})
	toolset.Register(edits{agent})
	toolset.Register(delegates{agent})
	helpers.Register(reviews{})
	desktops.Register(screens{agent: agent, desktop: desktops})
	return agent
}

func (s *service) Mount(services *connect.Server) {
	agentv1connect.RegisterAgentServiceHandler(services, s)
}

func (s *service) Close() error {
	return s.agent.Close()
}

func (s *service) Send(ctx context.Context, request *agentv1.SendRequest) (*agentv1.SendResponse, error) {
	if err := s.agent.freeze(ctx, request.GetSessionId(), request.GetClientEnv(), request.GetClientHost()); err != nil {
		return nil, err
	}
	if err := s.agent.launched(ctx, request.GetSessionId(), request.GetLaunch()); err != nil {
		return nil, err
	}
	r, err := s.agent.run(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	if request.GetYolo() {
		s.agent.setYolo(r, true)
	}
	r.finite.Store(request.GetFinite())
	if left := request.GetLeft(); left != "" && left != request.GetSessionId() {
		if err := s.agent.reset(ctx, left, request.GetSessionId()); err != nil {
			return nil, err
		}
	}
	if err := s.agent.Message(ctx, request.GetSessionId(), request.GetText(), request.GetVoice(), request.GetInterrupted()); err != nil {
		return nil, err
	}
	return &agentv1.SendResponse{}, nil
}

func (s *service) Finalize(ctx context.Context, request *agentv1.FinalizeRequest) (*agentv1.FinalizeResponse, error) {
	return &agentv1.FinalizeResponse{Messages: s.agent.Finalize(ctx, request.GetSessionIds(), request.GetReason())}, nil
}

func (s *service) Observe(ctx context.Context, request *agentv1.ObserveRequest) (*agentv1.ObserveResponse, error) {
	s.agent.precommand(ctx, request.GetSessionId(), request.GetCommand(), strings.ToLower(request.GetAlias()), request.GetArgs())
	return &agentv1.ObserveResponse{}, nil
}

func (s *service) Stop(_ context.Context, request *agentv1.StopRequest) (*agentv1.StopResponse, error) {
	if err := s.agent.Stop(request.GetSessionId()); err != nil {
		return nil, err
	}
	return &agentv1.StopResponse{}, nil
}

func (s *service) Watch(ctx context.Context, request *agentv1.WatchRequest, stream agentv1connect.AgentServiceWatchServerStream) error {
	return s.agent.Watch(ctx, request.GetSessionId(), request.GetClient(), stream.Send)
}

func (s *service) Live(context.Context, *agentv1.LiveRequest) (*agentv1.LiveResponse, error) {
	return &agentv1.LiveResponse{Sessions: s.agent.Live()}, nil
}

func (s *service) Shut(_ context.Context, request *agentv1.ShutRequest) (*agentv1.ShutResponse, error) {
	if err := s.agent.Shut(request.GetSessionId()); err != nil {
		return nil, err
	}
	return &agentv1.ShutResponse{}, nil
}

func (s *service) Edit(ctx context.Context, request *agentv1.EditRequest) (*agentv1.EditResponse, error) {
	removed, err := s.agent.Edit(ctx, request.GetSessionId(), request.GetMessageId(), request.GetText(), request.GetInterrupted())
	if err != nil {
		if _, ok := errors.AsType[*connect.Error](err); ok {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.EditResponse{Removed: uint32(removed)}, nil
}

func (s *service) Approve(_ context.Context, request *agentv1.ApproveRequest) (*agentv1.ApproveResponse, error) {
	choice := approvals.Once
	switch request.GetScope() {
	case agentv1.ApprovalScope_APPROVAL_SCOPE_SESSION:
		choice = approvals.Session
	case agentv1.ApprovalScope_APPROVAL_SCOPE_ALWAYS:
		choice = approvals.Always
	}
	count, err := s.agent.Approve(request.GetSessionId(), approvals.Answer{ID: request.GetId(), Choice: choice, All: request.GetAll()})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.ApproveResponse{Count: int32(count)}, nil
}

func (s *service) Confirm(ctx context.Context, request *agentv1.ConfirmRequest) (*agentv1.ConfirmResponse, error) {
	ok, note, err := s.agent.Confirm(ctx, request.GetSessionId(), request.GetCommand(), request.GetDetail())
	if err != nil {
		return nil, err
	}
	return &agentv1.ConfirmResponse{Ok: ok, Note: note}, nil
}

func (s *service) Deny(_ context.Context, request *agentv1.DenyRequest) (*agentv1.DenyResponse, error) {
	count, err := s.agent.Deny(request.GetSessionId(), approvals.Answer{ID: request.GetId(), All: request.GetAll(), Reason: request.GetReason()})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.DenyResponse{Count: int32(count)}, nil
}
