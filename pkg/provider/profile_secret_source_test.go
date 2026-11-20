package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestProfileSecretSourceReadsProfileAndBoundScopes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_API_KEY=sk-profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := ProfileSecretSource()
	if scope := src.BuildProfileSecretScope(dir); scope["ANTHROPIC_API_KEY"] != "sk-profile" {
		t.Fatalf("profile scope %v", scope)
	}
	ctx := BindProfileSecretScope(context.Background(), dir)
	if scope := src.CurrentSecretScope(ctx); scope["ANTHROPIC_API_KEY"] != "sk-profile" {
		t.Fatalf("bound scope %v", scope)
	}
	if got := CurrentSecretScopeHome(ctx); got != dir {
		t.Fatalf("bound home %q", got)
	}
	if !reflect.DeepEqual(src.ManagedDotenvKeys(), ManagedDotenvKeys()) || !reflect.DeepEqual(src.SourceSuppliedNames(), SourceSuppliedNames()) {
		t.Fatal("the source must read the env loader tables")
	}
}

func TestManagedDotenvKeysListInSortedOrder(t *testing.T) {
	secretSourceCacheMu.Lock()
	prev := managedDotenvKeys
	managedDotenvKeys = map[string]bool{"ZETA_KEY": true, "ALPHA_KEY": true, "MID_KEY": true, "BETA_KEY": true}
	secretSourceCacheMu.Unlock()
	t.Cleanup(func() {
		secretSourceCacheMu.Lock()
		managedDotenvKeys = prev
		secretSourceCacheMu.Unlock()
	})
	for range 20 {
		if got := ManagedDotenvKeys(); !reflect.DeepEqual(got, []string{"ALPHA_KEY", "BETA_KEY", "MID_KEY", "ZETA_KEY"}) {
			t.Fatalf("keys %v", got)
		}
	}
}

func TestToolRuntimeCredentialsResolveTheRequestedRoute(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("MY_RELAY_KEY", "relay-test-key")
	writeConfig(t, h, map[string]any{
		"providers": map[string]any{"my-relay": map[string]any{"base_url": "http://127.0.0.1:1/v1", "key_env": "MY_RELAY_KEY", "transport": "codex_responses"}},
	})
	base, key, err := ToolRuntimeCredentials(context.Background(), &tools.Env{Home: h}, "custom:my-relay", "")
	if err != nil {
		t.Fatal(err)
	}
	if base != "http://127.0.0.1:1/v1" || key != "relay-test-key" {
		t.Fatalf("got %q %q", base, key)
	}
}
