package provider

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Provider runs as a local external process rather than an HTTP endpoint
func IsExternalProcessProvider(ctx context.Context, provider string) bool {
	return isExternalProcessProvider(ctx, provider)
}

// Endpoint binds replayed Responses item ids to a connection so replay must drop them
func DropsReplayItemIDs(baseURL string) bool {
	return dropsReplayItemIDs(baseURL)
}

// Max output tokens of an Anthropic model by its longest matching family
func AnthropicMaxOutput(model string) int {
	return anthropicMaxOutput(model)
}

// Responses transport preflight of outgoing kwargs, an AttributeError for every transport lacking it
func PreflightKwargs(t Transport, apiKwargs map[string]any, allowStream, dropReplayItemIDs, sanitizeHarmony bool) (map[string]any, error) {
	if rt, ok := t.(*responsesAPITransport); ok && rt.codex != nil {
		return rt.codex.preflightKwargs(apiKwargs, allowStream, dropReplayItemIDs, sanitizeHarmony)
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'preflight_kwargs'", transportClassName(t))
}

// Python class name of a transport for the AttributeError text
func transportClassName(t Transport) string {
	switch t.(type) {
	case *chatCompletionsTransport:
		return "ChatCompletionsTransport"
	case *anthropicMessagesTransport:
		return "AnthropicTransport"
	case *bedrockConverseTransport:
		return "BedrockTransport"
	case *responsesAPITransport:
		return "ResponsesApiTransport"
	case nil:
		return "NoneType"
	}
	return fmt.Sprintf("%T", t)
}

// Hop count that scales the non streaming stale budget of one api payload
func LongDecodeStaleMultiplier(apiPayload map[string]any) int {
	return longDecodeStaleMultiplier(apiPayload)
}

// Model whose chat completions requests must send max_completion_tokens instead of max_tokens
func ModelForcesMaxCompletionTokens(model string) bool {
	return modelForcesMaxCompletionTokens(model)
}

// Route is the ChatGPT codex backend by exact provider name or chatgpt.com codex path
func ResponsesRouteIsCodexBackend(providerName, baseURL string) bool {
	return classifyResponsesRoute(providerName, baseURL).isCodexBackend
}

// Per model wire of a profile, false when it answers none and the declared mode when its hook raises
func ProfileModelAPIMode(ctx context.Context, profile Profile, model string) (string, bool) {
	hook := profile.APIModeForModelHook
	if hook == nil {
		perModelHooksMu.RLock()
		hook = perModelHooks[profile.Name]
		perModelHooksMu.RUnlock()
	}
	if hook == nil && profile.PerModelMode != nil {
		hook = func(m, _ string) APIMode { return profile.PerModelMode(m) }
	}
	if hook == nil {
		return "", false
	}
	declared := profile.Mode
	if declared == "" {
		declared = ModeChatCompletions
	}
	mode := modelAPIMode(ctx, &perModelHookProfile{name: profile.Name, mode: declared, hook: hook}, model, "")
	return string(mode), mode != ""
}

// Gateway display settings a platform entry may override, the keys of the global display defaults
func DisplayOverrideableKeys() map[string]bool {
	keys := map[string]bool{}
	for _, k := range []string{"tool_progress", "tool_progress_grouping", "show_reasoning", "reasoning_style", "tool_preview_length",
		"streaming", "interim_assistant_messages", "suppress_warning_notifications", "long_running_notifications", "busy_ack_detail",
		"busy_steer_ack_enabled", "cleanup_progress", "live_status"} {
		keys[k] = true
	}
	return keys
}

// First usable Anthropic credential over the registry env vars, the home dotenv beating a stale shell export
func GetAnthropicKey(ctx context.Context) string {
	row, err := providerConfig(ctx, "anthropic")
	if err != nil {
		panic(err)
	}
	h := home.Dir(ctx)
	for _, name := range row.KeyEnvNames() {
		v, err := config.GetEnvValuePreferDotenv(ctx, h, name)
		if err != nil {
			panic(err)
		}
		if v != "" {
			return v
		}
	}
	return ""
}
