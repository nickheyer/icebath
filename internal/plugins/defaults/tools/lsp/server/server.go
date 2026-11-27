// Connect service of the language servers, through which the lsp commands and the app reach the servers of the machine the server runs on
package server

import (
	"context"
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
	response := &lspv1.StatusResponse{Warnings: report.Warnings}
	if st := report.Service; st != nil {
		service := &lspv1.Service{WaitMode: st.WaitMode, WaitTimeout: st.WaitTimeout, InstallStrategy: st.InstallStrategy, Broken: pairs(st.Broken),
			Disabled: st.Disabled, BrokenRetrySeconds: st.RetrySeconds, WarmupTimeout: st.WarmupTimeout, ExcludeRoots: st.Excludes,
			ExcludeRootsInvalid: st.ExcludesInvalid, TrustedWorkspaces: st.Trusted, UntrustedSkipped: pairs(st.Untrusted)}
		for _, c := range st.Clients {
			service.Clients = append(service.Clients, &lspv1.Client{Server: c.Server, Root: c.Root, Folders: c.Folders, State: c.State, Running: c.Running})
		}
		response.Service = service
	}
	for _, srv := range report.Servers {
		response.Servers = append(response.Servers, &lspv1.Server{Id: srv.ID, Extensions: srv.Extensions, Description: srv.Description,
			Status: srv.Status, Path: srv.Path, UntrustedSafe: srv.UntrustedSafe, Custom: srv.Custom, Recipe: srv.Recipe, Automatic: srv.Automatic})
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
	stderr := func(text string) { send(&lspv1.InstallEvent{Kind: &lspv1.InstallEvent_Err{Err: text}}) }
	var code int
	if request.GetAll() {
		code = s.manager.InstallAll(ctx, cfg, request.GetIncludeManual(), stdout, stderr)
	} else {
		code = s.manager.Install(ctx, cfg, request.GetServer(), stdout, stderr)
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

func (s *service) Restart(context.Context, *lspv1.RestartRequest) (*lspv1.RestartResponse, error) {
	s.manager.Restart()
	return &lspv1.RestartResponse{}, nil
}
