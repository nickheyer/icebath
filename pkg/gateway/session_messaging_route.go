package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Runner resolving a messaging chat's next turn route without building an agent
type messagingRouteRunner interface {
	sessionTierRunner
	ChannelOverrideFor(source *SessionSource) *ChannelOverride
	SessionModelOverride(sessionKey string) map[string]any
	ResolveSessionReasoningConfig(ctx context.Context, source *SessionSource, sessionKey, model string) (map[string]any, error)
}

// Model and effort fields a messaging chat's next turn resolves, session override over channel over config
func messagingRouteInfo(ctx context.Context, runner AuthorityRunner, live *LiveSession) (*pyjson.Object, error) {
	cfg := loadGatewayConfig(ctx, "")
	block, _ := cfg["model"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	model, providerName, baseURL := any(resolveGatewayModel(ctx, cfg)), block["provider"], block["base_url"]
	resolver, ok := runner.(messagingRouteRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_channel_override_for'", runner)}
	}
	if channel := resolver.ChannelOverrideFor(live.Source); channel != nil {
		model, providerName = pyrepr.Or(channel.Model, model), pyrepr.Or(channel.Provider, providerName)
	}
	get := func(string) any { return nil }
	if override := resolver.SessionModelOverride(live.Route); len(override) > 0 {
		get = func(key string) any { return override[key] }
	} else if stored := runner.SessionStore().GetModelOverride(ctx, live.Route); stored != nil && stored.Len() > 0 {
		get = func(key string) any {
			value, _ := stored.Get(key)
			return value
		}
	}
	model = pyrepr.Or(get("model"), model)
	providerName, baseURL = pyrepr.Or(get("provider"), providerName), pyrepr.Or(get("base_url"), baseURL)
	apiMode := get("api_mode")
	if !pyTruthy(apiMode) {
		apiMode = nil
		if pyrepr.Equal(providerName, block["provider"]) {
			apiMode = block["api_mode"]
		}
	}
	modelText := anyText(model)
	reasoning, err := resolver.ResolveSessionReasoningConfig(ctx, live.Source, live.Route, modelText)
	if err != nil {
		return nil, err
	}
	tier, err := resolver.ResolveSessionServiceTier(ctx, live.Source, live.Route)
	if err != nil {
		return nil, err
	}
	var reasoningValue any
	if reasoning != nil {
		reasoningValue = reasoning
	}
	tierText := ""
	if tier != nil {
		tierText = *tier
	}
	info := pyjson.ObjectOf("model", model)
	mergeObject(info, effortFields(reasoningValue, tierText, model, providerName, baseURL, apiMode))
	return info, nil
}
