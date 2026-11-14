package providers

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type actualProfile struct {
	base
	d Deps
}

func newActual(d Deps) Profile {
	return &actualProfile{d: d, base: base{spec: Spec{
		Name: "actual", Aliases: []string{"actual-computer", "actualcomputer", "aci"}, DisplayName: "Actual Computer",
		Description: "Actual Computer - hosted inference via api.actual.inc, or local offline inference via model.base_url in config.yaml",
		SignupURL:   "https://actual.inc", EnvVars: []string{"ACTUAL_API_KEY"}, BaseURL: auth.DefaultActualBaseURL,
		AuthType: authTypeAPIKey, Mode: wire.ModeChat,
	}}}
}

func (p *actualProfile) Efforts(ctx context.Context, model string) ([]string, bool) {
	return wire.EffortsActualRelay.Efforts().Levels, true
}

func (p *actualProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	r := f.Reasoning
	if r == nil {
		return Extras{}, nil
	}
	if r.Enabled && reasoningEffortRaw(r) == "none" {
		return Extras{Body: map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"disabled"}`)},
			TopLevel: map[string]json.RawMessage{"reasoning_effort": wire.Quote("none")}}, nil
	}
	supported, _ := p.Efforts(ctx, f.Model)
	body, top := wire.ThinkingToggleExtras(r, wire.Efforts{Levels: supported}, true)
	return Extras{Body: body, TopLevel: top}, nil
}

func (p *actualProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	baseURL := in.BaseURL
	if strings.TrimSpace(baseURL) == "" {
		creds, err := p.d.Store.ResolveAPIKey(ctx, "actual")
		if err != nil {
			return nil, false
		}
		baseURL = creds.BaseURL
	}
	in.BaseURL = auth.NormalizeActualBaseURL(baseURL)
	return FetchModels(ctx, p.d.Net, &p.spec, in)
}
