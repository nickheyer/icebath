package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Re-mints the live credential through the provider profile refresh hook and rebuilds the shared client
func (a *AIAgent) tryRefreshProviderRuntimeCredentials(ctx context.Context, reason string) (bool, error) {
	profile := credentialRefreshProfile(ctx, a)
	if profile == nil {
		return false, nil
	}
	refreshed, _ := provider.AskOnErrorPath(ctx, profile, "refresh_runtime_credentials", nil, map[string]any{"reason": reason}).(map[string]any)
	token, _ := refreshed["api_key"].(string)
	if token == "" {
		return false, nil
	}
	a.apiKey = token
	if baseURL, _ := refreshed["base_url"].(string); baseURL != "" {
		a.setBaseURL(pystr.RstripChars(baseURL, "/"))
	}
	a.syncClientKwargsCredentials()
	if err := a.applyClientHeadersForBaseURL(ctx, a.baseURL, true); err != nil {
		return false, err
	}
	return a.replacePrimaryOpenAIClient(fmt.Sprintf("%s_credential_%s", profile.Name, reason)), nil
}
