package gatewayapi

import (
	"context"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Value of key in admission settings held as a JSON object or a plain map, nil when absent
func settingValue(settings any, key string) any {
	switch s := settings.(type) {
	case *pyjson.Object:
		if s == nil {
			return nil
		}
		v, _ := s.Get(key)
		return v
	case map[string]any:
		return s[key]
	}
	return nil
}

// Route of admission settings as text fields, nil when the settings carry none
func settingRoute(value any) map[string]string {
	var fields map[string]any
	switch r := value.(type) {
	case *pyjson.Object:
		if r == nil {
			return nil
		}
		fields = r.Vals
	case map[string]any:
		fields = r
	case map[string]string:
		return maps.Clone(r)
	default:
		return nil
	}
	route := make(map[string]string, len(fields))
	for k, v := range fields {
		if s, ok := v.(string); ok {
			route[k] = s
		}
	}
	return route
}

// Model a gateway turn runs for an API admission, the settings route selecting over the resolved runtime kwargs
func (a *APIServerAdapter) PrepareTurnRuntime(ctx context.Context, model string, runtimeKwargs map[string]any, settings any) (string, error) {
	route := settingRoute(settingValue(settings, "route"))
	requestedModel, _ := settingValue(settings, "requested_model").(string)
	if configured := a.resolveRoute(settingValue(settings, "requested_model")); len(configured) > 0 {
		withoutKey := maps.Clone(configured)
		delete(withoutKey, "api_key")
		if maps.Equal(withoutKey, route) {
			route = configured
		}
	}
	requestedProvider, _ := settingValue(settings, "requested_provider").(string)
	sessionModel, _ := settingValue(settings, "session_model").(string)
	selection, err := a.selectAgentRuntime(ctx, runtimeKwargs, model, agentOptions{requestedModel: requestedModel,
		requestedProvider: requestedProvider, route: route, sessionModel: sessionModel,
		confirmedRuntimeLock: pyrepr.Truthy(settingValue(settings, "confirmed_runtime_lock"))})
	if err != nil {
		return "", err
	}
	return selection.model, nil
}

// Served runtime of a finished gateway turn under a confirmed model lock, an error when it differs from the lock
func (a *APIServerAdapter) TurnRuntimeMetadata(ag *agent.AIAgent, settings any, confirmedRuntimeLock bool) (*pyjson.Object, error) {
	var requested *pyjson.Object
	switch r := settingValue(settings, "requested_runtime").(type) {
	case *pyjson.Object:
		requested = r
	case map[string]any:
		requested = pyjson.NewObject()
		for _, k := range slices.Sorted(maps.Keys(r)) {
			requested.Set(k, r[k])
		}
	}
	routeSource, _ := settingValue(settings, "route_source").(string)
	return a.turnRuntimeMetadata(ag, settingRoute(settingValue(settings, "route")), requested, routeSource, confirmedRuntimeLock)
}
