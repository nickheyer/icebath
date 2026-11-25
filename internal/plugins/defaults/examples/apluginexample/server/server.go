package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	apluginexampleconfig "github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample/config"
	"github.com/nickheyer/icebath/internal/server"
	apluginexamplev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/apluginexample/v1/apluginexamplev1connect"
)

type service struct {
	config *config.Config
}

func Register(registry *server.Registry, cfg *config.Config) {
	registry.Register(&service{config: cfg})
}

func (s *service) Mount(services *connect.Server) {
	apluginexamplev1connect.RegisterExampleServiceHandler(services, s)
}

func (s *service) Greet(context.Context, *apluginexamplev1.GreetRequest) (*apluginexamplev1.GreetResponse, error) {
	return &apluginexamplev1.GreetResponse{Greeting: s.config.GetString(apluginexampleconfig.Greeting)}, nil
}
