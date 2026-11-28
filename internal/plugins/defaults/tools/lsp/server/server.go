// Connect service of the language servers, through which the lsp commands and the app reach the servers of the machine the server runs on
package server

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	lsptools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/tools"
	"github.com/nickheyer/icebath/internal/server"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
)

type service struct {
	config  *config.Config
	manager *lsptools.Manager
}

func Register(registry *server.Registry, cfg *config.Config, manager *lsptools.Manager) {
	registry.Register(&service{config: cfg, manager: manager})
}

func (s *service) Mount(services *connect.Server) {
	lspv1connect.RegisterLspServiceHandler(services, s)
}

// Stops every language server as the server shuts down
func (s *service) Close() error {
	return s.manager.Close()
}

func pairs(in [][2]string) []*lspv1.Pair {
	out := make([]*lspv1.Pair, len(in))
	for i, p := range in {
		out[i] = &lspv1.Pair{Server: p[0], Root: p[1]}
	}
	return out
}

func (s *service) Status(_ context.Context, request *lspv1.StatusRequest) (*lspv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	report, err := s.manager.Report(cfg)
	if err != nil {
		return nil, err
	}
	response := &lspv1.StatusResponse{Checking: report.Checking, InstallWhenNeeded: report.Auto, WaitMode: report.WaitMode, WaitTimeout: report.WaitTimeout,
		WarmupTimeout: report.WarmupTimeout, BrokenRetrySeconds: report.RetrySeconds, Skipped: report.Skipped, ExcludeRoots: report.Excludes,
		ExcludeRootsInvalid: report.ExcludesInvalid, TrustedWorkspaces: report.Trusted, Broken: pairs(report.Broken),
		UntrustedSkipped: pairs(report.Untrusted), Disabled: report.Disabled, Warnings: report.Warnings}
	for _, c := range report.Clients {
		response.Clients = append(response.Clients, &lspv1.Client{Server: c.Server, Root: c.Root, Folders: c.Folders, State: c.State, Running: c.Running})
	}
	for _, srv := range report.Servers {
		response.Servers = append(response.Servers, &lspv1.Server{Id: srv.ID, Description: srv.Description, Extensions: srv.Extensions, State: srv.State,
			Path: srv.Path, Install: srv.Install, Installable: srv.Installable, Added: srv.Added, Command: srv.Command, UntrustedSafe: srv.UntrustedSafe,
			Running: srv.Running, Failed: srv.Failed, Skipped: srv.Skipped, Retry: srv.Retry})
	}
	return response, nil
}

func (s *service) Install(ctx context.Context, request *lspv1.InstallRequest, out lspv1connect.LspServiceInstallServerStream) error {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return err
	}
	// Lines are sent one at a time, the first failure to send kept and the rest let go
	var mu sync.Mutex
	var sendErr error
	send := func(event *lspv1.InstallEvent) {
		mu.Lock()
		defer mu.Unlock()
		if sendErr == nil {
			sendErr = out.Send(event)
		}
	}
	stdout := func(text string) { send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_Out{Out: text}}) }
	var outcome lsptools.Outcome
	if request.GetAll() {
		outcome = s.manager.InstallAll(ctx, cfg, stdout)
	} else if outcome, err = s.manager.Install(ctx, cfg, request.GetServer(), stdout); err != nil {
		send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_Err{Err: err.Error()}})
		send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_ExitCode{ExitCode: 2}})
		return sendErr
	}
	told := &lspv1.Outcome{Done: outcome.Done}
	for _, done := range outcome.Installed {
		told.Installed = append(told.Installed, &lspv1.Installed{Server: done.Server, Path: done.Path, Already: done.Already})
	}
	for _, need := range outcome.Needs {
		told.Needs = append(told.Needs, &lspv1.Need{Servers: need.Servers, Text: need.Text, Setting: need.Setting, Value: need.Value,
			ByHand: need.ByHand})
	}
	send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_Outcome{Outcome: told}})
	code := 0
	if len(outcome.Needs) > 0 {
		code = 1
	}
	send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_ExitCode{ExitCode: int32(code)}})
	return sendErr
}

func (s *service) Which(_ context.Context, request *lspv1.WhichRequest) (*lspv1.WhichResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	path, err := s.manager.Which(cfg, request.GetServer())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &lspv1.WhichResponse{Path: path}, nil
}

func (s *service) Stop(_ context.Context, request *lspv1.StopRequest) (*lspv1.StopResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return &lspv1.StopResponse{Stopped: int32(s.manager.StopRunning(cfg))}, nil
}

func (s *service) Add(_ context.Context, request *lspv1.AddRequest) (*lspv1.AddResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.manager.Add(cfg, request.GetName(), request.GetCommand(), request.GetTypes()); err != nil {
		return nil, refusal(err)
	}
	return &lspv1.AddResponse{}, nil
}

func (s *service) Turn(_ context.Context, request *lspv1.TurnRequest) (*lspv1.TurnResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.manager.Turn(cfg, request.GetServer(), request.GetOn()); err != nil {
		return nil, refusal(err)
	}
	return &lspv1.TurnResponse{}, nil
}

func (s *service) Remove(_ context.Context, request *lspv1.RemoveRequest) (*lspv1.RemoveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.manager.Remove(cfg, request.GetServer()); err != nil {
		return nil, refusal(err)
	}
	return &lspv1.RemoveResponse{}, nil
}

func (s *service) Trust(_ context.Context, request *lspv1.TrustRequest) (*lspv1.TrustResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	folders, err := s.manager.Trust(cfg)
	if err != nil {
		return nil, refusal(err)
	}
	return &lspv1.TrustResponse{Folders: folders}, nil
}

// Error of a change as the caller reads it: a setting the administrator locks refused as such, any other refusal as an argument it takes
// no more
func refusal(err error) error {
	if locked, ok := errors.AsType[*config.LockedError](err); ok {
		return connect.NewError(connect.CodePermissionDenied, locked.Error())
	}
	return connect.NewError(connect.CodeInvalidArgument, err.Error())
}
