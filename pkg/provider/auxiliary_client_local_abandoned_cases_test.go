package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const auxManagedLocalURL = "http://127.0.0.1:18434/v1"

// Supervisor state file declaring the managed endpoint with the netloc cache reset
func auxManagedState(t *testing.T) context.Context {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
	path := localRuntimeStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"base_url": "`+auxManagedLocalURL+`", "api_key": "k", "pid": 4242}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func auxLocalAbandonedMessages() []any {
	return []any{map[string]any{"role": "user", "content": "hi"}}
}

func TestManagedEndpointRequiresStream(t *testing.T) {
	ctx := auxManagedState(t)
	if !auxProviderRequiresStream(ctx, auxManagedLocalURL) {
		t.Fatal("managed endpoint must require streaming")
	}
}

func TestManagedDetectionMatchesNetlocNotSubstring(t *testing.T) {
	ctx := auxManagedState(t)
	if auxProviderRequiresStream(ctx, "http://127.0.0.1:8080/v1") {
		t.Fatal("same host on another port is not the managed endpoint")
	}
}

func TestRemoteProvidersUnaffected(t *testing.T) {
	ctx := auxManagedState(t)
	if auxProviderRequiresStream(ctx, "https://inference-api.nousresearch.com/v1/") {
		t.Fatal("remote provider must not require streaming")
	}
}

func TestExplicitMaxTokensForwardedToManagedLocal(t *testing.T) {
	ctx := auxManagedState(t)
	auxFakeCustomRuntime(t, auxManagedLocalURL, "k", "", "Qwen-Local")
	kwargs, err := auxBuildCallKwargs(ctx, "custom", "Qwen-Local", auxLocalAbandonedMessages(), auxCallKwargsOptions{MaxTokens: 64, Timeout: 30, Task: "title_generation"})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsCarryCap(kwargs, 64) {
		t.Fatalf("explicit caller cap dropped on the managed local endpoint — an EOS-less generation then runs to the full context window: %v", kwargs)
	}
}

func TestNoDefaultCapPolicyUnchangedForRemote(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
	kwargs, err := auxBuildCallKwargs(ctx, "openai", "some/model", auxLocalAbandonedMessages(), auxCallKwargsOptions{MaxTokens: 64, Timeout: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsOmitCap(kwargs) {
		t.Fatalf("kwargs %v", kwargs)
	}
}
