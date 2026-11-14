package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var anthropicReasoningOptional = []string{"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1",
	"claude-sonnet-4-0", "claude-sonnet-4.0", "claude-opus-4-2025", "claude-sonnet-4-2025", "claude-opus-4-5", "claude-opus-4.5",
	"claude-sonnet-4-5", "claude-sonnet-4.5", "claude-haiku-4-5", "claude-haiku-4.5"}

var grokAggregatorPrefixes = []string{"x-ai/grok-", "xai/grok-"}

type endpointPin struct {
	base string
	tags []string
}

var OpenRouterEndpointPins = func() map[string]endpointPin {
	pins := map[string]endpointPin{}
	tiers := map[string][]string{"": {"openai", "azure", "azure/us"}, "-fast": {"openai/fast"}, "-flex": {"openai/flex"}}
	for _, b := range []string{"openai/gpt-6-astra", "openai/gpt-6-astra-pro"} {
		for suffix, tags := range tiers {
			pins[b+suffix] = endpointPin{base: b, tags: tags}
		}
	}
	return pins
}()

type openRouterProfile struct {
	base
	d     Deps
	mu    sync.Mutex
	cache []string
}

func newOpenRouter(d Deps) Profile {
	return &openRouterProfile{d: d, base: base{spec: Spec{
		Name: "openrouter", Aliases: []string{"or"}, EnvVars: []string{"OPENROUTER_API_KEY"}, DisplayName: "OpenRouter",
		Description: "OpenRouter — unified API for 200+ models", SignupURL: "https://openrouter.ai/keys",
		BaseURL: "https://openrouter.ai/api/v1", ModelsURL: "https://openrouter.ai/api/v1/models",
		FallbackModels: []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "deepseek/deepseek-chat", "google/gemini-3.8-flash",
			"google/gemini-3.7-flash", "qwen/qwen3-plus"},
	}}}
}

const openRouterKeyEnv = "OPENROUTER_API_KEY"

func OpenRouterKeyPresent(ctx context.Context, secrets *secret.Scope) (bool, error) {
	value, _, err := secrets.Lookup(ctx, openRouterKeyEnv)
	var unscoped *secret.UnscopedError
	if errors.Is(err, secret.ErrUnscoped) || errors.As(err, &unscoped) {
		return os.Getenv(openRouterKeyEnv) != "", nil
	}
	if err != nil {
		return false, err
	}
	return value != "", nil
}

func anthropicReasoningMandatory(model string) bool {
	m := strings.ToLower(model)
	if !strings.HasPrefix(m, "anthropic/") && !strings.HasPrefix(m, "claude") && !strings.Contains(m, "claude") {
		return false
	}
	return !slices.ContainsFunc(anthropicReasoningOptional, func(s string) bool { return strings.Contains(m, s) })
}

func (p *openRouterProfile) clampToCatalog(r *wire.Reasoning, model string, caps *ReasoningCaps) (*wire.Reasoning, bool) {
	effort := r.Effort
	disabled := !r.Enabled || effort == "none"
	if effort == "" && !disabled {
		return r, true
	}
	if caps == nil || !caps.Supports {
		return r, true
	}
	if disabled {
		noDisable := strings.HasPrefix(model, "openai/") && !slices.Contains(wire.CodexEfforts(model).Efforts().Levels, "none")
		if caps.Mandatory || noDisable {
			return nil, false
		}
		return r, true
	}
	if !caps.EffortsDeclared {
		return r, true
	}
	clamped := wire.ClampEffort(effort, caps.Efforts, nil)
	if clamped != "" && clamped != effort {
		out := *r
		out.Effort = clamped
		out.Payload = nil
		if v, err := wire.ParseValue(r.Payload); err == nil && v.IsObject() {
			out.Payload = wire.DictOf(v).SetString("effort", clamped).JSON()
		}
		return &out, true
	}
	return r, true
}

func (p *openRouterProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	p.mu.Lock()
	cached := p.cache
	p.mu.Unlock()
	if cached != nil {
		return slices.Clone(cached), true
	}
	in.APIKey = auth.Secret{}
	models, ok := FetchModels(ctx, p.d.Net, &p.spec, in)
	if ok {
		p.mu.Lock()
		p.cache = slices.Clone(models)
		p.mu.Unlock()
	}
	return models, ok
}

func (p *openRouterProfile) ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error) {
	body := map[string]json.RawMessage{}
	if sticky := AffinityKey(ctx, f.SessionID); sticky != "" {
		body["session_id"] = wire.Quote(sticky)
	}
	prefs := f.ProviderPrefs
	if pin, ok := OpenRouterEndpointPins[f.Model]; ok {
		userOnly := false
		if v, err := wire.ParseValue(prefs); err == nil {
			userOnly = v.Get("only").Truthy()
		}
		if !(pin.base == f.Model && userOnly) {
			d := &wire.Dict{}
			if v, err := wire.ParseValue(prefs); err == nil && v.IsObject() {
				d = wire.DictOf(v)
			}
			prefs = d.Set("only", wire.Strings(pin.tags)).JSON()
		}
	}
	if v, err := wire.ParseValue(prefs); err == nil && v.Truthy() {
		body["provider"] = prefs
	}
	if f.Model == "openrouter/pareto-code" && len(f.MinCodingScore) > 0 {
		if v, err := wire.ParseValue(f.MinCodingScore); err == nil && v.Kind() != wire.KindNull && v.Display() != "" {
			score, ok := v.Float()
			if !ok {
				score, err = strconv.ParseFloat(strings.TrimSpace(v.Display()), 64)
				ok = err == nil
			}
			if ok && score >= 0 && score <= 1 {
				plugin := (&wire.Dict{}).SetString("id", "pareto-router").SetFloat("min_coding_score", score).JSON()
				body["plugins"] = wire.Array(plugin)
			}
		}
	}
	return body, nil
}

func (p *openRouterProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	out := Extras{Body: map[string]json.RawMessage{}, TopLevel: map[string]json.RawMessage{}}
	if pin, ok := OpenRouterEndpointPins[f.Model]; ok && pin.base != f.Model {
		out.Model = pin.base
	}
	if f.SupportsReasoning {
		switch {
		case anthropicReasoningMandatory(f.Model):
			if r := f.Reasoning; r != nil && r.Enabled && r.Effort != "" && r.Effort != "none" {
				out.TopLevel["verbosity"] = wire.Quote(r.Effort)
			}
		case f.Reasoning != nil:
			if clamped, ok := p.clampToCatalog(f.Reasoning, f.Model, f.Caps); ok {
				out.Body["reasoning"] = reasoningObject(clamped)
			}
		default:
			out.Body["reasoning"] = json.RawMessage(`{"enabled":true,"effort":"medium"}`)
		}
	}
	conv := AffinityKey(ctx, f.SessionID)
	if strings.Contains(f.CacheScope, "::") {
		conv = f.CacheScope
	}
	if conv != "" && f.Model != "" && slices.ContainsFunc(grokAggregatorPrefixes, func(prefix string) bool { return strings.HasPrefix(f.Model, prefix) }) {
		out.Headers = map[string]string{"x-grok-conv-id": conv}
	}
	return out, nil
}
