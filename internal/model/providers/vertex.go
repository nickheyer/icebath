package providers

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth/gcp"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/gemini"
)

type vertexProfile struct {
	base
	d      Deps
	vertex homeBound[*gcp.Vertex]
}

type VertexOwner interface {
	Vertex(ctx context.Context) (*gcp.Vertex, error)
}

func newVertex(d Deps) Profile {
	return &vertexProfile{d: d, base: base{spec: Spec{
		Name: "vertex", Aliases: []string{"google-vertex", "vertex-ai", "gcp-vertex", "vertexai"}, Mode: wire.ModeChat,
		BaseURL: "https://aiplatform.googleapis.com", AuthType: authTypeVertex, DefaultAuxModel: "google/gemini-3.6-flash",
		SkipModelListing: true,
	}}}
}

func (p *vertexProfile) Vertex(ctx context.Context) (*gcp.Vertex, error) {
	return p.vertex.get(ctx, p.d, func(env func(name string) string, key homeKey) (*gcp.Vertex, error) {
		return gcp.NewVertex(gcp.Options{Secrets: env, Multiplex: key.multiplex, Config: p.config, HTTP: p.d.HTTP})
	})
}

func (p *vertexProfile) config(ctx context.Context) config.VertexConfig {
	if p.d.Config == nil {
		return config.VertexConfig{}
	}
	cfg, err := p.d.Config.LoadReadonly(ctx)
	if err != nil {
		return config.VertexConfig{}
	}
	return cfg.Vertex
}

func (p *vertexProfile) ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error) {
	raw := gemini.ThinkingConfig(f.Model, f.Reasoning)
	if len(raw) == 0 {
		return nil, nil
	}
	snake := gemini.SnakeThinkingConfig(raw)
	if len(snake) == 0 {
		return nil, nil
	}
	return googleThinkingBody(snake), nil
}
