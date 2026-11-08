package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Codex singleton store resolver taking force refresh and refresh if expiring
var resolveCodexCredentials = func(env provider.PoolEnv, forceRefresh, refreshIfExpiring bool) (map[string]any, error) {
	return provider.ResolveCodexRuntimeCredentials(env, provider.CodexResolveOptions{ForceRefresh: forceRefresh, SkipExpiringRefresh: !refreshIfExpiring})
}

// xAI OAuth singleton store resolver taking force refresh and refresh if expiring with the proactive skew
var resolveXAIOAuthCredentials = func(env provider.PoolEnv, forceRefresh, refreshIfExpiring bool) (map[string]any, error) {
	return provider.ResolveXAIOAuthRuntimeCredentials(env, forceRefresh, refreshIfExpiring, 0)
}

var vertexConfig = provider.VertexGetConfig

// Adopts a freshly minted Codex or xAI OAuth singleton token, refusing a swap onto a different account
func (a *AIAgent) tryRefreshCodexClientCredentials(ctx context.Context, force bool) bool {
	if a.apiMode != "codex_responses" || (a.provider != "openai-codex" && a.provider != "xai-oauth") {
		return false
	}
	resolve := resolveCodexCredentials
	if a.provider != "openai-codex" {
		resolve = resolveXAIOAuthCredentials
	}
	env := agentPoolEnv(ctx)
	singletonNow, err := resolve(env, false, false)
	if err != nil {
		clientLifecycleLog().Debug(fmt.Sprintf("%s singleton read failed: %v", a.provider, err))
		return false
	}
	singletonKey := ""
	if v := singletonNow["api_key"]; pyrepr.Truthy(v) {
		singletonKey = pystr.Strip(pyStrValue(v))
	}
	oldKey := pystr.Strip(a.apiKey)
	if singletonKey != "" && oldKey != "" && singletonKey != oldKey {
		clientLifecycleLog().Debug(fmt.Sprintf("%s singleton tokens differ from the active api_key; skipping singleton force-refresh to avoid "+
			"silent account swap. Reactive credential rotation should go through the pool.", a.provider))
		return false
	}
	creds, err := resolve(env, force, true)
	if err != nil {
		clientLifecycleLog().Debug(fmt.Sprintf("%s credential refresh failed: %v", a.provider, err))
		return false
	}
	if !validCredentialPair(creds["api_key"], creds["base_url"]) {
		return false
	}
	apiKey, baseURL := creds["api_key"].(string), creds["base_url"].(string)
	if oldKey != "" && pystr.Strip(apiKey) == oldKey {
		clientLifecycleLog().Debug(fmt.Sprintf("%s credential refresh returned the same token; refresh likely failed silently", a.provider))
		return false
	}
	return a.adoptOpenAICredentials(apiKey, baseURL, a.provider+"_credential_refresh")
}

// Re-mints the Vertex OAuth token and rebuilds the shared client around it
func (a *AIAgent) tryRefreshVertexClientCredentials(ctx context.Context) bool {
	if a.apiMode != "chat_completions" || a.provider != "vertex" {
		return false
	}
	token, baseURL := vertexConfig(ctx)
	ok := validCredentialPair(token, baseURL) && a.adoptOpenAICredentials(token, baseURL, "vertex_credential_refresh")
	if ok {
		clientLifecycleLog().Info("Vertex AI OAuth token refreshed")
	}
	return ok
}
