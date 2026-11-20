package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestAgentSwitchModelStripsV1ForAnthropicMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{model: "glm-5", provider: "opencode-go", apiMode: "chat_completions", apiKey: "sk-opencode-fake", quietMode: true, clientKwargs: map[string]any{}}
	a.setBaseURL("https://opencode.ai/zen/go/v1")
	captured := ""
	sentinel := errors.New("strip verified")
	savedBuilder, savedToken := anthropicClientBuilder, resolveAnthropicToken
	t.Cleanup(func() { anthropicClientBuilder, resolveAnthropicToken = savedBuilder, savedToken })
	anthropicClientBuilder = func(_ context.Context, _ any, baseURL string, _ float64, _ bool) (any, error) {
		captured = baseURL
		return nil, sentinel
	}
	resolveAnthropicToken = func(provider.PoolEnv, string) string { return "" }
	err := a.switchModel(context.Background(), "minimax-m2.7", "opencode-go", "sk-opencode-fake", "https://opencode.ai/zen/go/v1", "anthropic_messages", nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("switch returned %v", err)
	}
	if captured != "https://opencode.ai/zen/go" {
		t.Fatalf("agent.switch_model did not strip /v1; passed %q to build_anthropic_client", captured)
	}
}
