package providers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type customProfile struct {
	base
	d Deps
}

func newCustom(d Deps) Profile {
	return &customProfile{d: d, base: base{spec: Spec{
		Name: ProviderCustom, Aliases: []string{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"},
	}}}
}

func looksLikeOllamaEndpoint(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	u := parseBaseURL(raw)
	if u == nil {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return false
		}
		if n == 11434 {
			return true
		}
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	return host != "" && (host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") || slices.Contains(strings.Split(host, "."), "ollama"))
}

func (p *customProfile) Efforts(ctx context.Context, model string) ([]string, bool) {
	return wire.EffortsOpenAICompat.Efforts().Levels, true
}

func (p *customProfile) DefaultReasoning(model string) *wire.Reasoning {
	return &wire.Reasoning{Enabled: true, Effort: "medium"}
}

func (p *customProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	out := Extras{Body: map[string]json.RawMessage{}, TopLevel: map[string]json.RawMessage{}}
	if f.OllamaNumCtx > 0 {
		out.Body["options"] = (&wire.Dict{}).SetInt("num_ctx", f.OllamaNumCtx).JSON()
	}
	if r := f.Reasoning; r != nil {
		effort := reasoningEffortRaw(r)
		switch {
		case effort == "none" || !r.Enabled:
			out.TopLevel["reasoning_effort"] = wire.Quote("none")
			if looksLikeOllamaEndpoint(f.BaseURL) {
				out.Body["think"] = wire.Bool(false)
			}
		case effort != "" && HostMatches(f.BaseURL, "api.groq.com"):
			out.TopLevel["reasoning_effort"] = wire.Quote("default")
		case effort != "":
			out.TopLevel["reasoning_effort"] = wire.Quote(wire.ClampEffort(effort, wire.EffortsOpenAICompat.Efforts().Levels, nil))
		}
	}
	return out, nil
}

func (p *customProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	if in.BaseURL == "" && p.spec.BaseURL == "" {
		return nil, false
	}
	return FetchModels(ctx, p.d.Net, &p.spec, in)
}
