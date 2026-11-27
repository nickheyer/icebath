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
	for _, view := range s.terminal.Jobs.Views(request.GetSessionId()) {
		process := &terminalv1.Process{
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
			process.HasExitCode, process.ExitCode = true, int32(*view.Code)
		}
		if !view.Ended.IsZero() {
			process.UptimeSeconds = int64(view.Ended.Sub(view.Started).Seconds())
			process.SinceExitSeconds = int64(time.Since(view.Ended).Seconds())
		}
		response.Processes = append(response.Processes, process)
	}
	return response, nil
}

func (s *service) Kill(ctx context.Context, request *terminalv1.KillRequest) (*terminalv1.KillResponse, error) {
	id, session := request.GetId(), request.GetSessionId()
	if session != "" {
		job := s.terminal.Jobs.Get(nil, session, id)
		if job == nil || job.Session != session {
			return &terminalv1.KillResponse{Status: "not_found", Error: "There is no background process with the ID " + id + "."}, nil
		}
		id = job.ID
	}
	report := s.terminal.Jobs.Kill(ctx, nil, session, id, "process.kill", false)
	return &terminalv1.KillResponse{Status: report.Status, Error: report.Error}, nil
}

func (s *service) Stop(ctx context.Context, request *terminalv1.StopRequest) (*terminalv1.StopResponse, error) {
	return &terminalv1.StopResponse{Killed: int32(s.terminal.Jobs.KillAll(ctx, request.GetSessionId(), "process.stop", false))}, nil
}
