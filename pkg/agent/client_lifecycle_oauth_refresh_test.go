package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// xAI OAuth codex responses agent over a fake OpenAI constructor recording each rebuilt client
func xaiOAuthRefreshAgent(t *testing.T) (*AIAgent, *[]*fakeOpenAI) {
	t.Helper()
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "xai-oauth", apiMode: "codex_responses", model: "grok-4.3", apiKey: "xai-oauth-token"}
	a.setBaseURL("https://api.x.ai/v1")
	a.clientKwargs = map[string]any{}
	return a, constructed
}

func installSingletonResolver(t *testing.T, target *func(provider.PoolEnv, bool, bool) (map[string]any, error), fn func(provider.PoolEnv, bool, bool) (map[string]any, error)) {
	t.Helper()
	prev := *target
	*target = fn
	t.Cleanup(func() { *target = prev })
}

func TestTryRefreshCodexClientCredentialsHandlesXaiOauth(t *testing.T) {
	a, constructed := xaiOAuthRefreshAgent(t)
	installSingletonResolver(t, &resolveXAIOAuthCredentials, func(_ provider.PoolEnv, force, _ bool) (map[string]any, error) {
		key := a.apiKey
		if force {
			key = "fresh-xai-token"
		}
		return map[string]any{"api_key": key, "base_url": "https://api.x.ai/v1"}, nil
	})
	existing := &fakeOpenAI{}
	a.client = existing
	var retired any
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(c any) int { retired = c; return 0 }
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	if !a.tryRefreshCodexClientCredentials(context.Background(), true) {
		t.Fatal("refresh must succeed")
	}
	if existing.closed || retired != existing {
		t.Fatalf("old client must be retired, not closed, closed=%v retired=%v", existing.closed, retired)
	}
	if len(*constructed) != 1 {
		t.Fatalf("constructed %d", len(*constructed))
	}
	rebuilt := (*constructed)[0]
	if rebuilt.kwargs["api_key"] != "fresh-xai-token" || rebuilt.kwargs["base_url"] != "https://api.x.ai/v1" {
		t.Fatalf("kwargs %v", rebuilt.kwargs)
	}
	if a.client != rebuilt || a.apiKey != "fresh-xai-token" {
		t.Fatalf("client %v key %q", a.client, a.apiKey)
	}
}

func TestTryRefreshCodexClientCredentialsSkipsXaiOauthWhenSingletonDiffers(t *testing.T) {
	a, _ := xaiOAuthRefreshAgent(t)
	refreshCalls := 0
	installSingletonResolver(t, &resolveXAIOAuthCredentials, func(_ provider.PoolEnv, force, _ bool) (map[string]any, error) {
		if force {
			refreshCalls++
		}
		return map[string]any{"api_key": "singleton-account-token", "base_url": "https://api.x.ai/v1"}, nil
	})
	pre := a.apiKey
	if a.tryRefreshCodexClientCredentials(context.Background(), true) {
		t.Fatal("must not refresh when the active credential is not the singleton")
	}
	if refreshCalls != 0 || a.apiKey != pre {
		t.Fatalf("force refresh ran %d times, key %q", refreshCalls, a.apiKey)
	}
}

func TestCodexRefreshReadsSingletonWithoutRefreshThenForcesAndRejectsSameToken(t *testing.T) {
	a, constructed := xaiOAuthRefreshAgent(t)
	a.provider, a.apiKey = "openai-codex", "codex-token"
	a.setBaseURL("https://chatgpt.com/backend-api/codex")
	var calls [][2]bool
	installSingletonResolver(t, &resolveXAIOAuthCredentials, func(provider.PoolEnv, bool, bool) (map[string]any, error) {
		t.Fatal("openai-codex must use the Codex resolver")
		return nil, nil
	})
	installSingletonResolver(t, &resolveCodexCredentials, func(_ provider.PoolEnv, force, ifExpiring bool) (map[string]any, error) {
		calls = append(calls, [2]bool{force, ifExpiring})
		return map[string]any{"api_key": " codex-token ", "base_url": "https://chatgpt.com/backend-api/codex"}, nil
	})
	if a.tryRefreshCodexClientCredentials(context.Background(), true) || len(*constructed) != 0 {
		t.Fatal("the same token back must not be adopted")
	}
	if len(calls) != 2 || calls[0] != [2]bool{false, false} || calls[1] != [2]bool{true, true} {
		t.Fatalf("calls %v", calls)
	}
	a.apiMode = "chat_completions"
	if a.tryRefreshCodexClientCredentials(context.Background(), true) || len(calls) != 2 {
		t.Fatal("only codex_responses agents refresh")
	}
}

func TestVertexRefreshAdoptsMintedTokenAndRejectsMissingConfig(t *testing.T) {
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "vertex", apiMode: "chat_completions", apiKey: "old"}
	a.setBaseURL("https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi")
	a.clientKwargs = map[string]any{}
	token, base := "", ""
	prev := vertexConfig
	vertexConfig = func(context.Context) (string, string) { return token, base }
	t.Cleanup(func() { vertexConfig = prev })
	if a.tryRefreshVertexClientCredentials(context.Background()) || len(*constructed) != 0 {
		t.Fatal("unresolvable Vertex config must not refresh")
	}
	token, base = "fresh-vertex", "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi/"
	if !a.tryRefreshVertexClientCredentials(context.Background()) || len(*constructed) != 1 {
		t.Fatal("minted token must be adopted")
	}
	if a.apiKey != "fresh-vertex" || a.baseURL != "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi" {
		t.Fatalf("key %q base %q", a.apiKey, a.baseURL)
	}
	a.provider = "openrouter"
	if a.tryRefreshVertexClientCredentials(context.Background()) || len(*constructed) != 1 {
		t.Fatal("only the vertex provider refreshes")
	}
}
