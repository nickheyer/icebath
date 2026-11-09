package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestParseConfigSecretEnvResolvesFromProfileScopeUnderMultiplex(t *testing.T) {
	t.Setenv("MY_HOOK_SECRET", "from-default-profile-env")
	previous := plugins.InstalledServices()
	t.Cleanup(func() { plugins.InstallServices(previous) })
	bound := provider.WithSecretScope(context.Background(), map[string]string{"MY_HOOK_SECRET": "from-secondary-scope"}, "")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	install := func(ctx context.Context) {
		plugins.InstallServices(&plugins.Services{GetSecret: func(_ context.Context, name string) (string, bool, error) { return provider.GetSecret(ctx, name) }})
	}
	raw := map[string]any{"hooks": map[string]any{"outbound": []any{map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "secret_env": "MY_HOOK_SECRET"}}}}
	install(bound)
	targets, err := plugins.IterConfiguredOutboundWebhooks(raw)
	if err != nil || len(targets) != 1 || targets[0].Secret != "from-secondary-scope" {
		t.Fatalf("targets %v %v", targets, err)
	}
	install(provider.WithSecretScope(context.Background(), map[string]string{}, ""))
	targets, err = plugins.IterConfiguredOutboundWebhooks(raw)
	if err != nil || len(targets) != 1 || targets[0].Secret != "" {
		t.Fatalf("targets %v %v", targets, err)
	}
}
