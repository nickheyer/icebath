package server

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
)

type service struct {
	config   *config.Config
	terminal *terminal.Terminal
}

func Register(host *server.Server, cfg *config.Config, t *terminal.Terminal) {
	host.Register(&service{config: cfg, terminal: t})
}

func (s *service) Mount(services *connect.Server) {
	terminalv1connect.RegisterProcessServiceHandler(services, s)
	terminalv1connect.RegisterBackendServiceHandler(services, s)
}

func (s *service) List(_ context.Context, request *terminalv1.ListRequest) (*terminalv1.ListResponse, error) {
	response := &terminalv1.ListResponse{}
	if id := request.GetId(); id != "" {
		if view, ok := s.terminal.Jobs.Find(id); ok {
			response.Processes = append(response.Processes, process(view))
		}
		return response, nil
	}
	for _, view := range s.terminal.Jobs.Views(request.GetSessionId()) {
		response.Processes = append(response.Processes, process(view))
	}
	return response, nil
}

// A background process as the service answers it
func process(view terminal.View) *terminalv1.Process {
	p := &terminalv1.Process{
		Id:               view.ID,
		Command:          view.Command,
		SessionId:        view.Session,
		Profile:          view.Profile,
		Status:           view.Status,
		CompletionReason: view.Reason,
		UptimeSeconds:    int64(time.Since(view.Started).Seconds()),
		OutputTail:       view.Tail,
		Cwd:              view.Cwd,
		Pid:              int32(view.Pid),
	}
	if view.Code != nil {
		p.HasExitCode, p.ExitCode = true, int32(*view.Code)
	}
	if !view.Ended.IsZero() {
		p.UptimeSeconds = int64(view.Ended.Sub(view.Started).Seconds())
		p.SinceExitSeconds = int64(time.Since(view.Ended).Seconds())
	}
	return p
}

// Kills the process in whichever session it runs, as /processes kill does
func (s *service) Kill(ctx context.Context, request *terminalv1.KillRequest) (*terminalv1.KillResponse, error) {
	report := s.terminal.Jobs.Kill(ctx, nil, "", request.GetId(), "process.kill", false)
	return &terminalv1.KillResponse{Status: report.Status, Error: report.Error}, nil
}

func (s *service) Stop(ctx context.Context, request *terminalv1.StopRequest) (*terminalv1.StopResponse, error) {
	return &terminalv1.StopResponse{Killed: int32(s.terminal.Jobs.KillIn(ctx, request.GetSessionId()))}, nil
}
