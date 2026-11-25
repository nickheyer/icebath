package provider

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/opencodezen/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func TestProfilesRouteEachModelByItsFamily(t *testing.T) {
	cfg := config.New(t.TempDir())
	opencodezenconfig.Register(cfg)

	listed := profiles{config: cfg}.Profiles()
	if len(listed) != 2 || listed[0].Name != opencodezenconfig.ZenName || listed[1].Name != opencodezenconfig.GoName {
		t.Fatalf("Profiles() = %d profiles, want Zen then Go", len(listed))
	}
	zen, sub := listed[0], listed[1]
	if zen.PerModelMode == nil || sub.PerModelMode == nil {
		t.Fatal("Profiles() left a per model mode unset")
	}
	for _, c := range []struct {
		model    string
		zen, sub provider.APIMode
	}{
		{"claude-opus-5", provider.ModeAnthropicMessages, provider.ModeAnthropicMessages},
		{"union-alpha", provider.ModeAnthropicMessages, provider.ModeAnthropicMessages},
		{"gpt-5.6-luna", provider.ModeCodexResponses, provider.ModeCodexResponses},
		{"grok-4.5", provider.ModeCodexResponses, provider.ModeCodexResponses},
		{"muse-spark-1.2", provider.ModeCodexResponses, provider.ModeCodexResponses},
		{"qwen3.6-plus", provider.ModeAnthropicMessages, provider.ModeAnthropicMessages},
		{"minimax-m3", provider.ModeChatCompletions, provider.ModeAnthropicMessages},
		{"glm-5", provider.ModeChatCompletions, provider.ModeChatCompletions},
		{"", provider.ModeChatCompletions, provider.ModeChatCompletions},
		{" OpenCode-Zen/Claude-Sonnet-5 ", provider.ModeAnthropicMessages, provider.ModeChatCompletions},
		{"opencode-go/minimax-m3", provider.ModeChatCompletions, provider.ModeAnthropicMessages},
		{"opencode-zen/", provider.ModeChatCompletions, provider.ModeChatCompletions},
	} {
		if got := zen.PerModelMode(c.model); got != c.zen {
			t.Errorf("Zen PerModelMode(%q) = %q, want %q", c.model, got, c.zen)
		}
		if got := sub.PerModelMode(c.model); got != c.sub {
			t.Errorf("Go PerModelMode(%q) = %q, want %q", c.model, got, c.sub)
		}
	}
}
