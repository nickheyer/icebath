package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
)

func (s *service) Backends(_ context.Context, request *terminalv1.BackendsRequest) (*terminalv1.BackendsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &terminalv1.BackendsResponse{Profile: cfg.Profile(), Variable: s.terminal.Holder(cfg)}
	for _, place := range s.terminal.Places(cfg) {
		backend := &terminalv1.Backend{
			Name:     place.Backend.Name,
			Title:    place.Backend.Title,
			About:    place.Backend.About,
			Selected: place.Selected,
			Prepares: place.Backend.Prepare != nil,
			State:    place.State,
			Ready:    place.State == terminal.Ready,
			Missing:  place.Missing,
			Why:      place.Why,
			Readies:  place.Backend.Readies,
		}
		for _, check := range place.Checks {
			backend.Checks = append(backend.Checks, &terminalv1.Check{Ok: check.OK, Label: check.Label, State: check.State, Detail: check.Detail,
				Fix: check.Fix, Setting: check.Setting})
		}
		if section, ok := cfg.Section(terminalconfig.Key + "." + place.Backend.Name); ok && len(section.Settings) > 0 {
			backend.Section = section.Key
		}
		response.Backends = append(response.Backends, backend)
	}
	return response, nil
}

func (s *service) Use(_ context.Context, request *terminalv1.UseRequest) (*terminalv1.UseResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	note, held, err := s.terminal.Use(cfg, request.GetBackend())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &terminalv1.UseResponse{Note: note, Held: held}, nil
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
