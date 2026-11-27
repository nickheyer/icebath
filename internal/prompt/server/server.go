package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/server"
	promptv1 "github.com/nickheyer/icebath/pkg/gen/icebath/prompt/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/prompt/v1/promptv1connect"
)

type service struct {
	prompt *prompt.Prompt
}

func Register(host *server.Server, p *prompt.Prompt) {
	host.Register(&service{prompt: p})
}

func (s *service) Mount(services *connect.Server) {
	promptv1connect.RegisterPromptServiceHandler(services, s)
}

func (s *service) Size(ctx context.Context, request *promptv1.SizeRequest) (*promptv1.SizeResponse, error) {
	env, toolsets, err := s.prompt.Preview(ctx, request.GetDirectory(), request.GetPlatform())
	if err != nil {
		return nil, failure(err)
	}
	sections, err := s.prompt.Sections(ctx, env)
	if err != nil {
		return nil, err
	}
	whole := prompt.Join(sections)
	response := &promptv1.SizeResponse{
		Platform: env.Platform,
		Model:    env.Model,
		Chars:    int64(utf8.RuneCountInString(whole)),
		Bytes:    int64(len(whole)),
	}
	for _, tier := range prompt.Tiers {
		var in []prompt.Section
		for _, section := range sections {
			if section.Tier == tier {
				in = append(in, section)
				if text := strings.TrimSpace(section.Text); section.Name != "" && text != "" {
					response.Sections = append(response.Sections, part(section.Name, tier, text))
				}
			}
		}
		response.Tiers = append(response.Tiers, part(tier.String(), tier, prompt.Join(in)))
	}
	all, err := encoded(env.Definitions)
	if err != nil {
		return nil, err
	}
	response.Tools, response.ToolBytes = int64(len(env.Definitions)), all
	sets := map[string]*promptv1.Toolset{}
	for _, definition := range env.Definitions {
		function, _ := definition["function"].(map[string]any)
		name, _ := function["name"].(string)
		toolset := cmp.Or(toolsets[name], "(unknown)")
		set := sets[toolset]
		if set == nil {
			set = &promptv1.Toolset{Name: toolset}
			sets[toolset] = set
			response.Toolsets = append(response.Toolsets, set)
		}
		schema, err := encoded(definition)
		if err != nil {
			return nil, err
		}
		set.Tools++
		set.Bytes += schema
	}
	slices.SortFunc(response.Toolsets, func(a, b *promptv1.Toolset) int {
		return cmp.Or(cmp.Compare(b.GetBytes(), a.GetBytes()), cmp.Compare(a.GetName(), b.GetName()))
	})
	return response, nil
}

func (s *service) Refresh(_ context.Context, request *promptv1.RefreshRequest) (*promptv1.RefreshResponse, error) {
	session, count, err := s.prompt.Refresh(request.GetSessionId())
	if err != nil {
		return nil, failure(err)
	}
	return &promptv1.RefreshResponse{SessionId: session, Sessions: int64(count)}, nil
}

func part(name string, tier prompt.Tier, text string) *promptv1.Part {
	return &promptv1.Part{Name: name, Tier: tier.String(), Chars: int64(utf8.RuneCountInString(text)), Bytes: int64(len(text))}
}

// Bytes of the JSON a provider is sent for the value
func encoded(value any) (int64, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0, err
	}
	return int64(out.Len() - 1), nil
}

// Error as the caller reads it, nothing to build prompts with being a precondition
func failure(err error) error {
	if errors.Is(err, prompt.ErrNoKeeper) {
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return err
}
