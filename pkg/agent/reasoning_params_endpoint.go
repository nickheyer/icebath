package agent

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Static OpenRouter reasoning families used while the live capability cache is cold
var openrouterReasoningPrefixes = []string{
	"deepseek/", "anthropic/", "openai/", "x-ai/", "google/gemini-2", "google/gemma-4",
	"qwen/qwen3", "tencent/hy", "xiaomi/",
}

// Hosts whose reasoning gate core owns even when a plugin profile claims them
var coreReasoningGatedHosts = []string{"nousresearch.com", "ai-gateway.vercel.sh", "ollama.com"}

const reasoningProbeTTL = 60 * time.Second

type reasoningProbeEntry struct {
	value any
	at    time.Time
}

// Clock of the reasoning probe cache, a seam tests move past the unknown answer TTL
var reasoningProbeNow = time.Now

// Probe answer per model and base url, definitive answers kept and unknown ones rechecked after a minute
func (a *AIAgent) cachedReasoningProbe(cache *map[[2]string]reasoningProbeEntry, probe func(model, baseURL, apiKey string) any, unknown any, definitive func(any) bool) any {
	key := [2]string{a.model, a.baseURL}
	a.reasoningProbeMu.Lock()
	if *cache == nil {
		*cache = map[[2]string]reasoningProbeEntry{}
	}
	cached, ok := (*cache)[key]
	a.reasoningProbeMu.Unlock()
	if ok && (definitive(cached.value) || reasoningProbeNow().Sub(cached.at) < reasoningProbeTTL) {
		return cached.value
	}
	value := func() (v any) {
		defer func() {
			if recover() != nil {
				v = unknown
			}
		}()
		return probe(a.model, a.baseURL, a.apiKey)
	}()
	a.reasoningProbeMu.Lock()
	(*cache)[key] = reasoningProbeEntry{value: value, at: reasoningProbeNow()}
	a.reasoningProbeMu.Unlock()
	return value
}

// LM Studio published reasoning options of the model, gating and clamping toggle only models
func (a *AIAgent) lmstudioReasoningOptionsCached(ctx context.Context) []string {
	v := a.cachedReasoningProbe(&a.lmReasoningOptsCache, func(model, baseURL, apiKey string) any {
		return provider.LMStudioModelReasoningOptions(ctx, model, baseURL, apiKey, 0)
	}, []string{}, func(v any) bool {
		opts, _ := v.([]string)
		return len(opts) > 0
	})
	opts, _ := v.([]string)
	return opts
}

// Profile claiming this endpoint that gates its reasoning, nil for endpoints core gates itself
func (a *AIAgent) reasoningEndpointProfile(ctx context.Context) *provider.Profile {
	url := a.baseURLLower
	if pystr.Lower(pystr.Strip(a.provider)) == "lmstudio" || a.isOpenRouterURL() {
		return nil
	}
	for _, host := range coreReasoningGatedHosts {
		if hostMatches(url, host) {
			return nil
		}
	}
	if p, ok := provider.ProfileForHost(ctx, a.baseURL, false); ok {
		return &p
	}
	return nil
}

// Reasoning payload of the endpoint gating profile for the model and reasoning config, nil when no profile gates
func (a *AIAgent) endpointReasoningPayload(ctx context.Context) any {
	profile := a.reasoningEndpointProfile(ctx)
	if profile == nil {
		return nil
	}
	return provider.AskProfile(ctx, profile, "reasoning_payload", nil, a,
		[]any{a.model, provider.ReasoningOfConfig(a.reasoningConfig)}, map[string]any{"base_url": a.baseURL})
}

// True when reasoning extra_body is safe on this route, gated to known reasoning families and direct Nous Portal
func (a *AIAgent) supportsReasoningExtraBody(ctx context.Context) bool {
	url := a.baseURLLower
	if hostMatches(url, "nousresearch.com") || hostMatches(url, "ai-gateway.vercel.sh") {
		return true
	}
	if profile := a.reasoningEndpointProfile(ctx); profile != nil {
		return provider.AskProfile(ctx, profile, "reasoning_payload", nil, a,
			[]any{a.model, nil}, map[string]any{"base_url": a.baseURL}) != nil
	}
	if pystr.Lower(pystr.Strip(a.provider)) == "lmstudio" {
		for _, opt := range a.lmstudioReasoningOptionsCached(ctx) {
			if opt != "" && opt != "off" {
				return true
			}
		}
		return false
	}
	if hostMatches(url, "ollama.com") {
		return a.OllamaSupportsThinkingCached()
	}
	if !a.isOpenRouterURL() || hostMatches(url, "api.mistral.ai") {
		return false
	}
	caps, known := func() (caps provider.ReasoningCapabilities, known bool) {
		defer func() {
			if recover() != nil {
				known = false
			}
		}()
		caps, known = provider.OpenRouterModelReasoningCapabilities(ctx, a.model, 0, false)
		if !known {
			provider.WarmOpenRouterReasoningCapsAsync(ctx)
		}
		return caps, known
	}()
	if known {
		return caps.SupportsReasoning
	}
	model := pystr.Lower(a.model)
	for _, prefix := range openrouterReasoningPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// Must echo tier of the active reasoning replay route, never true on a route that ignores the field
func (a *AIAgent) needsThinkingReasoningPad() bool { return a.reasoningReplayRoute().Pad }

// Echo family checks with the raw provider and no model for Kimi as the rule table expects
func (a *AIAgent) needsKimiToolReasoning() bool {
	return reasoningEchoFamily("kimi", a.provider, "", a.baseURL)
}

func (a *AIAgent) needsDeepseekToolReasoning() bool {
	return reasoningEchoFamily("deepseek", pystr.Lower(a.provider), a.model, a.baseURL)
}

func (a *AIAgent) needsMimoToolReasoning() bool {
	return reasoningEchoFamily("mimo", pystr.Lower(a.provider), a.model, a.baseURL)
}

// Echo family match raising like Python on an unknown family
func reasoningEchoFamily(family, providerName, model, baseURL string) bool {
	ok, err := provider.MatchesReasoningEchoFamily(family, providerName, model, baseURL)
	if err != nil {
		panic(err)
	}
	return ok
}
