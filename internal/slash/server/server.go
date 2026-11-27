package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/slash"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1/slashv1connect"
)

type service struct {
	slash *slash.Slash
}

func Register(host *server.Server, s *slash.Slash) {
	host.Register(&service{slash: s})
}

func (s *service) Mount(services *connect.Server) {
	slashv1connect.RegisterSlashServiceHandler(services, s)
}

func (s *service) List(context.Context, *slashv1.ListRequest) (*slashv1.ListResponse, error) {
	response := &slashv1.ListResponse{}
	for _, command := range s.slash.Commands() {
		response.Commands = append(response.Commands, &slashv1.Command{
			Name:    command.Name,
			Aliases: command.Aliases,
			Usage:   command.Usage,
			Help:    command.Help,
			Skill:   command.Skill,
		})
	}
	return response, nil
}
