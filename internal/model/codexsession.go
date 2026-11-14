package model

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/codex"
)

func (s *Session) Codex(ctx context.Context, o codex.Options) (*codex.Session, error) {
	ctx = s.r.bind(ctx)
	rt := s.Route()
	if strings.TrimSpace(o.Bin) == "" {
		cfg, err := s.r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		o.Bin = strings.TrimSpace(cfg.Model.CodexBin)
		if o.Bin == "" {
			o.Bin = "codex"
		}
	}
	o.ModelProvider = s.codexModelProvider(ctx, rt)
	o.Model = s.codexWireModel(ctx, rt, o.ModelProvider)
	o.Version = s.r.c.o.Version
	return codex.Start(ctx, o)
}

func (s *Session) CodexModelProvider(ctx context.Context) string {
	ctx = s.r.bind(ctx)
	return s.codexModelProvider(ctx, s.Route())
}

func (s *Session) CodexTurn(ctx context.Context, msg wire.Message) (codex.TurnInput, error) {
	ctx = s.r.bind(ctx)
	rt := s.Route()
	model := s.codexWireModel(ctx, rt, s.codexModelProvider(ctx, rt))
	tier, err := s.CodexServiceTier(ctx)
	if err != nil {
		return codex.TurnInput{}, err
	}
	return codex.TurnInput{Message: msg, Model: model, Effort: s.codexTurnEffort(ctx, rt, model), ServiceTier: tier}, nil
}

func (s *Session) codexModelProvider(ctx context.Context, rt *Route) string {
	if strings.ToLower(strings.TrimSpace(rt.Provider)) != "custom" {
		return ""
	}
	if id, ok := s.r.CodexProviderID(ctx, rt.Requested); ok {
		return id
	}
	return ""
}

func (s *Session) codexWireModel(ctx context.Context, rt *Route, modelProvider string) string {
	model := stripContextVariant(rt.Model)
	if model != "" && modelProvider == "" {
		model = s.r.c.o.Catalog.NormalizeModelForProvider(ctx, model, "openai-codex")
	}
	return model
}

func (s *Session) codexTurnEffort(ctx context.Context, rt *Route, model string) string {
	r := s.o.Reasoning
	if r == nil || (r.Enabled && r.Effort == "") {
		return ""
	}
	s.mu.Lock()
	backend := s.responsesBackend()
	s.mu.Unlock()
	var declared []string
	if backend != wire.ResponsesCodex && backend != wire.ResponsesOpenAI {
		if p, ok := s.r.c.o.Providers.Get(ctx, rt.Provider); ok {
			if d, ok := p.(providers.EffortDeclarer); ok {
				if efforts, ok := d.Efforts(ctx, model); ok {
					declared = efforts
				}
			}
		}
	}
	effort, _ := s.r.c.responses.ResolveEffort(model, r, backend, declared)
	if effort == "max" && r.Effort == "ultra" && slices.Contains(wire.RouteEfforts(rt.Provider, model, wire.ModeCodexAppServer), "ultra") {
		return "ultra"
	}
	return effort
}

func (s *Session) CodexServiceTier(ctx context.Context) (string, error) {
	ctx = s.r.bind(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	overrides, err := s.effectiveOverrides(ctx)
	if err != nil {
		return "", err
	}
	word, ok := CodexTierWord(overrideText(overrides, "service_tier"))
	if !ok {
		return "", nil
	}
	return word, nil
}
