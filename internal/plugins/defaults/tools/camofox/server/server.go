// Connect service of the camofox tools, through which `icebath camofox` reaches the server the profile drives
package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	camofoxtools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/tools"
	"github.com/nickheyer/icebath/internal/server"
	camofoxv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/camofox/v1/camofoxv1connect"
)

type service struct {
	config  *config.Config
	camofox *camofoxtools.Camofox
}

func Register(registry *server.Registry, cfg *config.Config, c *camofoxtools.Camofox) {
	registry.Register(&service{config: cfg, camofox: c})
}

func (s *service) Mount(services *connect.Server) {
	camofoxv1connect.RegisterCamofoxServiceHandler(services, s)
}

func status(st camofoxtools.Status) *camofoxv1.StatusResponse {
	return &camofoxv1.StatusResponse{Url: st.URL, Identity: st.Identity, Reachable: st.Reachable, Vnc: st.VNC}
}

func (s *service) Status(ctx context.Context, request *camofoxv1.StatusRequest) (*camofoxv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return status(s.camofox.Status(ctx, cfg)), nil
}

func (s *service) Setup(ctx context.Context, request *camofoxv1.SetupRequest) (*camofoxv1.SetupResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.camofox.Setup(cfg, request.GetUrl(), request.GetApiKey()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &camofoxv1.SetupResponse{Status: status(s.camofox.Status(ctx, cfg))}, nil
}
