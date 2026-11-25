package server

import (
	"cmp"
	"context"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/tools"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
)

type service struct {
	config *config.Config
	tools  *tools.Tools
}

func Register(host *server.Server, cfg *config.Config, t *tools.Tools) {
	host.Register(&service{config: cfg, tools: t})
}

func (s *service) Mount(services *connect.Server) {
	toolsv1connect.RegisterToolServiceHandler(services, s)
}

func (s *service) List(context.Context, *toolsv1.ListRequest) (*toolsv1.ListResponse, error) {
	env := &tools.Env{Config: s.config}
	defs := s.tools.Tools()
	slices.SortFunc(defs, func(a, b tools.Def) int {
		return cmp.Or(cmp.Compare(a.Toolset, b.Toolset), cmp.Compare(a.Name, b.Name))
	})
	response := &toolsv1.ListResponse{}
	for _, def := range defs {
		description, _ := def.Schema["description"].(string)
		response.Tools = append(response.Tools, &toolsv1.Tool{
			Name:        def.Name,
			Toolset:     def.Toolset,
			Description: description,
			Available:   def.Schema != nil && def.Handler != nil && (def.Check == nil || def.Check(env)),
		})
	}
	return response, nil
}
