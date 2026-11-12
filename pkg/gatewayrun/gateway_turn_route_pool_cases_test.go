package gatewayrun

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestGatewayTurnRoutePool(t *testing.T) {
	t.Run("test_resolve_turn_includes_pool", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		pool := provider.NewPool(provider.PoolEnv{}, "openai-codex", nil)
		r := &GatewayRunner{}
		route := r.resolveTurnAgentConfig("gpt-5.4", map[string]any{"api_key": "***", "base_url": nil, "provider": "openai-codex",
			"requested_provider": "openai-codex", "api_mode": "codex_responses", "command": nil, "args": []any{}, "credential_pool": pool}, tierFromRunner)
		if route.runtime["credential_pool"] != pool || route.runtime["requested_provider"] != "openai-codex" {
			t.Fatalf("runtime %v", route.runtime)
		}
	})
}
