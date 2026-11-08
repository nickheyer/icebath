package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Adopts the entry as the live credential, false with nothing changed when its route cannot serve the model
func (a *AIAgent) swapCredential(ctx context.Context, entry provider.PooledCredential) (bool, error) {
	runtimeKey := entry.RuntimeAPIKey()
	if runtimeKey == "" {
		runtimeKey = entry.AccessToken
	}
	runtimeBase := entry.RuntimeBaseURL()
	if runtimeBase == "" {
		runtimeBase = entry.BaseURL
	}
	if runtimeBase == "" {
		runtimeBase = a.baseURL
	}
	actualRoute := provider.IsActualRoute(a.provider, runtimeBase)
	if actualRoute {
		runtimeBase = provider.NormalizeActualBaseURL(runtimeBase)
	}
	strippedBase := strings.TrimRight(runtimeBase, "/")
	if !provider.RouteCanServeModel(a.provider, strippedBase, a.model) {
		id := entry.ID
		if id == "" {
			id = "?"
		}
		clientLifecycleLog().Info(fmt.Sprintf("Credential %s skipped: its route cannot serve model %s", id, a.model))
		return false, nil
	}
	if actualRoute {
		a.apiMode = "chat_completions"
		clear(a.transportCache)
	}
	a.credentialPoolEntryID = entry.ID
	routeChanged := provider.NormalizeRouteBaseURL(a.baseURL) != provider.NormalizeRouteBaseURL(runtimeBase)
	if a.apiMode == "anthropic_messages" {
		closeClientQuietly(a.anthropicClient)
		a.anthropicAPIKey, a.anthropicBaseURL = runtimeKey, strippedBase
		client, err := a.buildDirectAnthropicClient(ctx, runtimeKey, a.anthropicBaseURL)
		if err != nil {
			return false, err
		}
		a.anthropicClient = client
		a.isAnthropicOAuth = a.anthropicOAuthFlag(ctx, runtimeKey)
		a.apiKey, a.baseURL = runtimeKey, strippedBase
		return true, nil
	}
	a.apiKey, a.baseURL = runtimeKey, strippedBase
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	a.clientKwargs["api_key"] = a.apiKey
	a.clientKwargs["base_url"] = a.baseURL
	if err := a.reapplyRouteClientConfig(ctx, routeChanged); err != nil {
		return false, err
	}
	a.replacePrimaryOpenAIClient("credential_rotation")
	return true, nil
}
