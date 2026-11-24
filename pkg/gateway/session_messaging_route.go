package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Runner resolving a messaging chat's next turn route without building an agent
type messagingRouteRunner interface {
	sessionTierRunner
	ChannelOverrideFor(source *SessionSource) *ChannelOverride
	SessionModelOverride(sessionKey string) map[string]any
	ResolveSessionReasoningConfig(ctx context.Context, source *SessionSource, sessionKey, model string) (map[string]any, error)
}

// Model and effort fields a messaging chat's next turn resolves, session override over channel over config
func messagingRouteInfo(ctx context.Context, runner AuthorityRunner, live *LiveSession) (*jsonutil.Object, error) {
	configured := settings.ModelProvider.Get(ctx)
	model, providerName, baseURL := any(settings.Model.Get(ctx)), any(configured), any(settings.ModelBaseURL.Get(ctx))
	resolver, ok := runner.(messagingRouteRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("the gateway runner (%T) cannot look up a channel override", runner)}
	}
	if channel := resolver.ChannelOverrideFor(live.Source); channel != nil {
		model, providerName = anyutil.Or(channel.Model, model), anyutil.Or(channel.Provider, providerName)
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
	model = anyutil.Or(get("model"), model)
	providerName, baseURL = anyutil.Or(get("provider"), providerName), anyutil.Or(get("base_url"), baseURL)
	apiMode := get("api_mode")
	if !pyTruthy(apiMode) {
		apiMode = nil
		if anyutil.Equal(providerName, configured) && settings.ModelAPIMode.Get(ctx) != "" {
			apiMode = settings.ModelAPIMode.Get(ctx)
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
	info := jsonutil.ObjectOf("model", model)
	mergeObject(info, effortFields(reasoningValue, tierText, model, providerName, baseURL, apiMode))
	return info, nil
}
