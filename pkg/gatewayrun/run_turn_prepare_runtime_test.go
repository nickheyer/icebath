package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Profile home holding the config text, every provider key cleared and only the given keys exported
func runtimeTestHome(t *testing.T, configYAML string, keys map[string]string) string {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	t.Setenv("HOME", filepath.Join(root, "userhome"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "missing-codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	for _, p := range provider.Profiles() {
		for _, name := range provider.ProfileEnvVars(p) {
			t.Setenv(name, "")
		}
	}
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ICEBATH_INFERENCE_PROVIDER"} {
		t.Setenv(name, "")
	}
	for name, value := range keys {
		t.Setenv(name, value)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return h
}

func TestGatewayAuthFallbackUsesFallbackModelFromConfig(t *testing.T) {
	runtimeTestHome(t, "model:\n  default: gpt-5.5\n  provider: openai-codex\nfallback_providers:\n  - provider: openrouter\n    model: minimax/minimax-m2.7\n",
		map[string]string{"OPENROUTER_API_KEY": "sk-openrouter"})
	r := allocGatewayRunner()
	model, kwargs, err := r.resolveSessionAgentRuntime(context.Background(), nil, "agent:main:telegram:group:-1003715515980:63",
		map[string]any{"model": map[string]any{"default": "gpt-5.5", "provider": "openai-codex"},
			"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "minimax/minimax-m2.7"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if model != "minimax/minimax-m2.7" {
		t.Fatalf("model %q", model)
	}
	if kwargs["provider"] != "openrouter" || kwargs["api_key"] != "sk-openrouter" {
		t.Fatalf("kwargs %v", kwargs)
	}
	if r.pendingPreAgentFallbackNotice() == "" {
		t.Fatal("the fallback switch left no notice")
	}
}

func TestEmptyModelRecoversSessionLastGood(t *testing.T) {
	runtimeTestHome(t, "model:\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n", nil)
	r := allocGatewayRunner()
	sk := "agent:main:discord:dm:123"
	ctx := context.Background()
	if _, _, err := r.resolveSessionAgentRuntime(ctx, nil, sk, map[string]any{"model": map[string]any{"default": "deepseek/deepseek-v4-flash"}}, true); err != nil {
		t.Fatal(err)
	}
	model, _, err := r.resolveSessionAgentRuntime(ctx, nil, sk, map[string]any{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("recovery turn must reuse last-known-good, not build model=%q", model)
	}
}

func TestChannelOverrideBeatsGlobal(t *testing.T) {
	runtimeTestHome(t, "model:\n  default: global/model\n  provider: anthropic\n",
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-k", "OPENROUTER_API_KEY": "k2"})
	r := allocGatewayRunner()
	discord := gateway.NewPlatformConfig()
	discord.Enabled = true
	discord.ChannelOverrides["chan_1"] = &gateway.ChannelOverride{Model: "channel/model", Provider: "openrouter"}
	r.config = &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformDiscord: discord}}
	userID := "u1"
	source := &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatID: "chan_1", UserID: &userID}
	model, runtime, err := r.resolveSessionAgentRuntime(context.Background(), source, "", map[string]any{"model": map[string]any{"default": "global/model"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if model != "channel/model" || runtime["provider"] != "openrouter" {
		t.Fatalf("model %q runtime %v", model, runtime)
	}
}
