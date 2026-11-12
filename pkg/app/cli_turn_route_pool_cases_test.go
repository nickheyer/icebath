package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestCliTurnRoutePool(t *testing.T) {
	t.Run("test_resolve_turn_includes_pool", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		pool := provider.NewPool(provider.PoolEnv{}, "openai-codex", nil)
		c := &CLI{ctx: context.Background(), model: "gpt-5.4", apiKey: "sk-test", provider: "openai-codex", requestedProvider: "my-named-provider",
			apiMode: "codex_responses", credentialPool: pool}
		route := c.resolveTurnAgentConfig("test message")
		if route.runtime.CredentialPool != pool || route.runtime.RequestedProvider != "my-named-provider" || !strings.Contains(route.signature, "my-named-provider") {
			t.Fatalf("route %+v runtime %+v", route, route.runtime)
		}
		c.requestedProvider = "other-named-provider"
		if other := c.resolveTurnAgentConfig("test message"); other.signature == route.signature {
			t.Fatalf("signature unchanged %q", other.signature)
		}
	})
}
