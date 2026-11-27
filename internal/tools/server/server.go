package server

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
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
	disabled := s.config.GetStringSlice(toolsconfig.Disabled)
	defs := s.tools.Tools()
	// The tools of one heading together, headings and the tools under each by the names a person reads
	groups := tools.Groups(defs)
	slices.SortFunc(defs, func(a, b tools.Def) int {
		return cmp.Or(cmp.Compare(strings.ToLower(groups[a.Toolset]), strings.ToLower(groups[b.Toolset])),
			cmp.Compare(strings.ToLower(a.Label()), strings.ToLower(b.Label())), cmp.Compare(a.Name, b.Name))
	})
	response := &toolsv1.ListResponse{}
	for _, def := range defs {
		description, _ := def.Schema["description"].(string)
		response.Tools = append(response.Tools, &toolsv1.Tool{
			Name:        def.Name,
			Toolset:     def.Toolset,
			Description: description,
			Available:   def.Schema != nil && def.Handler != nil && (def.Check == nil || def.Check(env)),
			Title:       def.Label(),
			Icon:        def.Icon,
			Subject:     def.Subject,
			Disabled:    slices.Contains(disabled, def.Name),
			About:       def.About,
			Group:       groups[def.Toolset],
		})
	}
	return response, nil
}
