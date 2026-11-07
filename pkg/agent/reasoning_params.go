package agent

import (
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Replay route decision with the api mode, route, echo opt in and rejected carriers it was computed for
type reasoningRouteCacheEntry struct {
	apiMode, provider, model, baseURL string
	echoOptIn                         bool
	rejected                          string
	route                             provider.ReasoningReplayRoute
}

// Whether model.reasoning_echo marks the current provider must echo
func (a *AIAgent) reasoningEchoOptIn() bool {
	return pyTruthy(a.reasoningEchoFlag)
}

// Reasoning replay decision for the active route, recomputed only when its route inputs or rejections change
func (a *AIAgent) reasoningReplayRoute() provider.ReasoningReplayRoute {
	rejected, err := a.rejectedReasoningCarriers()
	if err != nil {
		panic(err)
	}
	keys := slices.Sorted(maps.Keys(rejected))
	baseURL := a.baseURLLower
	if baseURL == "" {
		baseURL = a.baseURL
	}
	entry := reasoningRouteCacheEntry{apiMode: a.apiMode, provider: a.provider, model: a.model, baseURL: baseURL,
		echoOptIn: a.reasoningEchoOptIn(), rejected: strings.Join(keys, "\x00")}
	if c := a.reasoningRouteCache; c != nil && c.apiMode == entry.apiMode && c.provider == entry.provider &&
		c.model == entry.model && c.baseURL == entry.baseURL && c.echoOptIn == entry.echoOptIn && c.rejected == entry.rejected {
		return c.route
	}
	entry.route = provider.ReasoningReplayRouteFor(entry.apiMode, a.provider, a.model, a.baseURL, entry.echoOptIn, rejected)
	a.reasoningRouteCache = &entry
	return entry.route
}

// Forwards the source message's reasoning fields onto its API replay copy under the active route policy
func (a *AIAgent) copyReasoningContentForAPI(src *state.Message, apiMsg map[string]any) {
	route := a.reasoningReplayRoute()
	applyReasoningContentPolicy(state.MessageToDict(src), apiMsg, route.Pad, route.Carriers)
}

// True for every api mode except Codex Responses, whose tool call fields strict chat APIs reject
func (a *AIAgent) shouldSanitizeToolCalls() bool {
	return a.apiMode != "codex_responses"
}

// Drops Codex Responses tool call fields, keeping extra_content only where the route consumes thought signatures
func sanitizeToolCallsForStrictAPI(apiMsg map[string]any, model, baseURL, providerName string) {
	toolCalls, ok := apiMsg["tool_calls"].([]any)
	if !ok {
		if typed, isMaps := apiMsg["tool_calls"].([]map[string]any); isMaps {
			toolCalls = make([]any, len(typed))
			for i, tc := range typed {
				toolCalls[i] = tc
			}
		} else {
			return
		}
	}
	strip := map[string]bool{"call_id": true, "response_item_id": true}
	if !provider.ModelConsumesThoughtSignature(model, baseURL, providerName) {
		strip["extra_content"] = true
	}
	out := make([]any, len(toolCalls))
	for i, tc := range toolCalls {
		m, isMap := tc.(map[string]any)
		if !isMap {
			out[i] = tc
			continue
		}
		kept := make(map[string]any, len(m))
		for k, v := range m {
			if !strip[k] {
				kept[k] = v
			}
		}
		out[i] = kept
	}
	apiMsg["tool_calls"] = out
}
