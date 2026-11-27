package server

import (
	"context"

	"github.com/nickheyer/icebath/internal/server"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
)

func (s *service) Backends(_ context.Context, request *terminalv1.BackendsRequest) (*terminalv1.BackendsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &terminalv1.BackendsResponse{Profile: cfg.Profile()}
	for _, report := range s.terminal.Survey(cfg) {
		backend := &terminalv1.Backend{
			Name:        report.Backend.Name,
			Title:       report.Backend.Title,
			About:       report.Backend.About,
			Selected:    report.Selected,
			Unavailable: report.Unavailable,
			Setup:       report.Setup,
			Prepares:    report.Backend.Prepare != nil,
		}
		for _, check := range report.Checks {
			backend.Checks = append(backend.Checks, &terminalv1.Check{Ok: check.OK, Label: check.Label, Detail: check.Detail})
		}
		if section, ok := cfg.Section(terminalconfig.Key + "." + report.Backend.Name); ok && len(section.Settings) > 0 {
			backend.Section = section.Key
		}
		response.Backends = append(response.Backends, backend)
	}
	return response, nil
}

func (s *service) Prepare(ctx context.Context, request *terminalv1.PrepareRequest, stream terminalv1connect.BackendServicePrepareServerStream) error {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return err
	}
	var sent error
	say := func(line string) {
		if sent == nil {
			sent = stream.Send(&terminalv1.PrepareResponse{Line: line})
		}
	}
	done := &terminalv1.PrepareResponse{Done: true}
	if err := s.terminal.Prepare(ctx, cfg, request.GetBackend(), say); err != nil {
		done.Error = err.Error()
	}
	if sent != nil {
		return sent
	}
	return stream.Send(done)
}
