package provider

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigHooksReadTheRunHome(t *testing.T) {
	h, _ := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openrouter", "providers": map[string]any{}})
	hooks := tools.ToolsConfigHooks
	if hooks.CodexAuthStatus(context.Background(), h)["logged_in"] == true || hooks.XAIOAuthStatus(context.Background(), h)["logged_in"] == true {
		t.Fatal("empty store read as logged in")
	}
	if err := hooks.SaveCodexTokens(context.Background(), h, map[string]any{"access_token": "codex-a", "refresh_token": "codex-r"}, "2026-01-01T00:00:00Z", false); err != nil {
		t.Fatal(err)
	}
	auth := readAuthStore(t, h)
	providers, _ := auth["providers"].(map[string]any)
	state, _ := providers["openai-codex"].(map[string]any)
	tokens, _ := state["tokens"].(map[string]any)
	if auth["active_provider"] != "openrouter" || tokens["access_token"] != "codex-a" || state["last_refresh"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("auth store %v", auth)
	}
	t.Setenv("ICEBATH_NONINTERACTIVE", "yes")
	if !hooks.SetupIsNoninteractive() {
		t.Fatal("noninteractive env not read")
	}
	restore := WithSetupOutput(io.Discard)
	defer restore()
	prev := xaiOAuthDeviceCodeLoginFn
	t.Cleanup(func() { xaiOAuthDeviceCodeLoginFn = prev })
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		return map[string]any{"discovery": map[string]any{}}, nil
	}
	if hooks.XAIOAuthLoginFromSetup(context.Background(), h) {
		t.Fatal("login without tokens reported success")
	}
}
