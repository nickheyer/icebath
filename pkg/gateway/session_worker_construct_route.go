package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Python str of a value or empty when falsy, the text a route URL compares by
func routeText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return pyrepr.Str(value)
}

// RouteRuntime resolves a launch policy route on the frozen config and launch key, walking its own fallback chain when allowed
func RouteRuntime(ctx context.Context, policy *LocalSessionPolicy, frozen map[string]any, model, key *string, fallback bool) (*provider.Runtime, map[string]any, error) {
	modelBlock := map[string]any{}
	if block := frozen["model"]; pyrepr.Truthy(block) {
		mapping, ok := block.(map[string]any)
		if !ok {
			return nil, nil, &PyError{Kind: "AttributeError", Msg: "the worker route block is " + errchain.Kind(block) + ", expected a mapping"}
		}
		modelBlock = mapping
	}
	requested := ""
	if raw := modelBlock["provider"]; pyrepr.Truthy(raw) {
		text, ok := raw.(string)
		if !ok {
			return nil, nil, &PyError{Kind: "AttributeError", Msg: "a worker route value is " + errchain.Kind(raw) + ", expected text"}
		}
		requested = text
	}
	request, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return nil, nil, err
	}
	requestMap, ok := pyjson.Plain(request).(map[string]any)
	if !ok {
		return nil, nil, &PyError{Kind: "AttributeError", Msg: "the worker route request is " + errchain.Kind(request) + ", expected a mapping"}
	}
	launchURL := routeText(requestMap["base_url"])
	if launchURL != "" && provider.NormalizeRouteBaseURL(launchURL) != provider.NormalizeRouteBaseURL(routeText(modelBlock["base_url"])) {
		launchURL = ""
	}
	targetModel, launchKey := "", ""
	if model != nil {
		targetModel = *model
	}
	if key != nil {
		launchKey = *key
	}
	scoped := provider.WithFrozenRuntimeConfig(ctx, frozen)
	runtime, err := provider.ResolveNamedCustomRuntime(scoped, "", requested, launchKey, launchURL, targetModel)
	if err != nil {
		return nil, nil, err
	}
	if runtime != nil {
		return runtime, nil, nil
	}
	if !fallback {
		runtime, err := provider.ResolveRuntimeProvider(scoped, "", requested, targetModel, launchURL, launchKey)
		return runtime, nil, err
	}
	return provider.ResolveRuntimeWithFallback(scoped, "", requested, targetModel, launchURL, launchKey)
}
