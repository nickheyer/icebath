package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Channel override entry of a chat, its model and provider
type channelModelOverride struct {
	Model    string
	Provider string
}

// Runner resolving a messaging chat's next turn route without building an agent
type messagingRouteRunner interface {
	ChannelOverrideFor(ctx context.Context, source *SessionSource) *channelModelOverride
	SessionModelOverride(route string) *pyjson.Object
	ResolveSessionReasoningConfig(ctx context.Context, source *SessionSource, sessionKey, model string) (map[string]any, error)
	ResolveSessionServiceTier(ctx context.Context, source *SessionSource, sessionKey string) (string, error)
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
	if channel := resolver.ChannelOverrideFor(ctx, live.Source); channel != nil {
		model, providerName = pyrepr.Or(channel.Model, model), pyrepr.Or(channel.Provider, providerName)
	}
	override := resolver.SessionModelOverride(live.Route)
	if override == nil || override.Len() == 0 {
		override = runner.SessionStore().GetModelOverride(ctx, live.Route)
	}
	if override == nil {
		override = pyjson.NewObject()
	}
	get := func(key string) any {
		value, _ := override.Get(key)
		return value
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
	info := pyjson.ObjectOf("model", model)
	mergeObject(info, effortFields(reasoningValue, tier, model, providerName, baseURL, apiMode))
	return info, nil
}
