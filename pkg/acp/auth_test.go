package acp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func withRuntime(t *testing.T, rt *provider.Runtime) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := resolveRuntimeProvider
	resolveRuntimeProvider = func(context.Context) (*provider.Runtime, error) { return rt, nil }
	t.Cleanup(func() { resolveRuntimeProvider = prev })
}

func TestDetectProviderPresenceHasProviderFalseWithoutCredentials(t *testing.T) {
	withRuntime(t, &provider.Runtime{Provider: "openrouter", APIKey: ""})
	if got := DetectProvider(context.Background()); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectProviderDetectOpenrouter(t *testing.T) {
	withRuntime(t, &provider.Runtime{Provider: "openrouter", APIKey: "sk-or-test"})
	if got := DetectProvider(context.Background()); got != "openrouter" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectProviderEntraCallableAPIKeyIsAValidCredential(t *testing.T) {
	withRuntime(t, &provider.Runtime{
		Provider: "azure-foundry", APIMode: "chat_completions", AuthMode: "entra_id",
		BaseURL:     "https://r.openai.azure.com/openai/v1",
		TokenSource: func(context.Context) (string, error) { return "jwt-fresh", nil },
	})
	if got := DetectProvider(context.Background()); got != "azure-foundry" {
		t.Fatalf("got %q", got)
	}
}
