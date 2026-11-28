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

// The groups of the profile's tools, each as the one function that says how a group stands answers, and their tools under them by the names
// a person reads
func (s *service) List(_ context.Context, request *toolsv1.ListRequest) (*toolsv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	env := &tools.Env{Config: cfg}
	disabled := cfg.GetStringSlice(toolsconfig.Disabled)
	response := &toolsv1.ListResponse{}
	for _, standing := range s.tools.Standings(cfg) {
		group := standing.Group
		message := &toolsv1.Group{Title: group.Title, State: standing.State, Note: standing.Note, Sections: group.Sections, Area: group.Area,
			Opens: group.Opens, Elsewhere: group.Elsewhere, Total: int32(standing.Total), On: int32(standing.On)}
		for _, blocked := range standing.Blocked {
			message.Blocked = append(message.Blocked, &toolsv1.Blocked{Tool: blocked.Tool.Label(), Why: blocked.Why})
		}
		response.Groups = append(response.Groups, message)
	}
	// Every tool, those of other profiles too, so a chat of any profile can name the tools it calls
	defs := s.tools.Tools()
	groups := tools.Groups(defs)
	slices.SortFunc(defs, func(a, b tools.Def) int {
		return cmp.Or(cmp.Compare(strings.ToLower(groups[a.Toolset]), strings.ToLower(groups[b.Toolset])),
			cmp.Compare(strings.ToLower(a.Label()), strings.ToLower(b.Label())), cmp.Compare(a.Name, b.Name))
	})
	for _, def := range defs {
		description, _ := def.Schema["description"].(string)
		lacks := tools.Lacking(def, env)
		response.Tools = append(response.Tools, &toolsv1.Tool{Name: def.Name, Toolset: def.Toolset, Description: description, Available: lacks == "",
			Title: def.Label(), Icon: def.Icon, Subject: def.Subject, Disabled: slices.Contains(disabled, def.Name), About: def.About,
			Group: groups[def.Toolset], Lacks: lacks})
	}
	return response, nil
}

func (s *service) Turn(_ context.Context, request *toolsv1.TurnRequest) (*toolsv1.TurnResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	turned, err := s.tools.Turn(cfg, request.GetWords(), request.GetOn())
	if err != nil {
		return nil, err
	}
	response := &toolsv1.TurnResponse{Said: turned.Said, Unknown: turned.Unknown}
	for _, def := range turned.Tools {
		response.Turned = append(response.Turned, def.Label())
	}
	return response, nil
}
