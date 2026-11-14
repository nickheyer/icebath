package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Resolver over an empty home with an optional active secret scope on its context
func entryKeyResolver(t *testing.T, scope map[string]string) *resolver {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	if scope != nil {
		ctx = WithSecretScope(ctx, scope, "")
	}
	return newResolver(ctx, config.Tree{}, config.NewSecrets(h), h, nil)
}

func TestResolveEntryApiKeyInlineApiKeyWins(t *testing.T) {
	t.Setenv("FB_KEY", "env-key")
	if got := entryKeyResolver(t, nil).ResolveEntryAPIKey(map[string]any{"provider": "custom", "api_key": "inline-key", "key_env": "FB_KEY"}); got != "inline-key" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveEntryApiKeyNoKeyFieldsReturnsNone(t *testing.T) {
	if got := entryKeyResolver(t, nil).ResolveEntryAPIKey(map[string]any{"provider": "openrouter", "model": "glm"}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveEntryApiKeyWhitespaceInlineKeyFallsThroughToEnv(t *testing.T) {
	t.Setenv("FB_KEY", "env-key")
	if got := entryKeyResolver(t, nil).ResolveEntryAPIKey(map[string]any{"api_key": "   ", "key_env": "FB_KEY"}); got != "env-key" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveEntryApiKeyKeyEnvResolvesFromActiveSecretScopeNotRawEnv(t *testing.T) {
	t.Setenv("FB_KEY", "fake-other-profile-key")
	if got := entryKeyResolver(t, map[string]string{"FB_KEY": "fake-active-profile-key"}).ResolveEntryAPIKey(map[string]any{"key_env": "FB_KEY"}); got != "fake-active-profile-key" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveEntryApiKeyKeyEnvFallsBackToEnvWhenNoActiveScope(t *testing.T) {
	t.Setenv("FB_KEY", "env-key")
	if got := entryKeyResolver(t, nil).ResolveEntryAPIKey(map[string]any{"key_env": "FB_KEY"}); got != "env-key" {
		t.Fatalf("got %q", got)
	}
}

func TestEffectiveRuntimeProviderNamedCustomEntryKeepsConfiguredId(t *testing.T) {
	entry := map[string]any{"provider": "my-custom-provider", "model": "some-model"}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "custom", RequestedProvider: "my-custom-provider"}); got != "my-custom-provider" {
		t.Fatalf("got %q", got)
	}
}

func TestEffectiveRuntimeProviderRequestedProviderMissingFallsBackToEntry(t *testing.T) {
	entry := map[string]any{"provider": "my-custom-provider", "model": "some-model"}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "custom"}); got != "my-custom-provider" {
		t.Fatalf("got %q", got)
	}
}

func TestEffectiveRuntimeProviderBuiltinProviderUntouched(t *testing.T) {
	entry := map[string]any{"provider": "openrouter", "model": "glm"}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "openrouter", RequestedProvider: "openrouter"}); got != "openrouter" {
		t.Fatalf("got %q", got)
	}
}

func TestEffectiveRuntimeProviderGenuinelyBareCustomStaysCustom(t *testing.T) {
	entry := map[string]any{"provider": "custom", "model": "some-model"}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "custom", RequestedProvider: "custom"}); got != "custom" {
		t.Fatalf("got %q", got)
	}
}

func TestEffectiveRuntimeProviderNoneInputsAreSafe(t *testing.T) {
	if got := EffectiveRuntimeProvider(nil, nil); got != "" {
		t.Fatalf("got %q", got)
	}
}
