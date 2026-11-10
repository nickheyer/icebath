package provider

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRunXAIOAuthLoginFromSetupDoesNotHijackActiveProvider(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openrouter", "providers": map[string]any{}})
	configPath := filepath.Join(h, "config.yaml")
	raw, err := yaml.Marshal(map[string]any{
		"model": map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4", "base_url": "https://openrouter.ai/api/v1"},
		"tts":   map[string]any{"provider": "edge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prevLogin, prevGetenv := xaiOAuthDeviceCodeLoginFn, oauthGetenv
	t.Cleanup(func() { xaiOAuthDeviceCodeLoginFn, oauthGetenv = prevLogin, prevGetenv })
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		return map[string]any{
			"tokens":    map[string]any{"access_token": "tts-xai-access", "refresh_token": "tts-xai-refresh", "id_token": "", "token_type": "Bearer"},
			"discovery": map[string]any{"token_endpoint": "https://auth.x.ai/oauth2/token"}, "redirect_uri": "",
			"base_url": "https://api.x.ai/v1", "last_refresh": "2026-07-25T12:00:00Z",
		}, nil
	}
	oauthGetenv = func(name string) string {
		if name == "SSH_CLIENT" {
			return "remote"
		}
		return ""
	}
	if err := env.Store.SuppressCredentialSource("xai-oauth", "device_code"); err != nil {
		t.Fatal(err)
	}
	if !env.Store.IsSourceSuppressed("xai-oauth", "device_code") {
		t.Fatal("device_code not suppressed before login")
	}
	restore := WithSetupOutput(io.Discard)
	defer restore()
	if !RunXAIOAuthLoginFromSetup(env) {
		t.Fatal("login from setup reported failure")
	}
	if env.Store.IsSourceSuppressed("xai-oauth", "device_code") {
		t.Fatal("device_code still suppressed after login")
	}
	auth := readAuthStore(t, h)
	if auth["active_provider"] != "openrouter" {
		t.Fatalf("active_provider %v", auth["active_provider"])
	}
	providers, _ := auth["providers"].(map[string]any)
	xaiState, _ := providers["xai-oauth"].(map[string]any)
	tokens, _ := xaiState["tokens"].(map[string]any)
	if tokens["access_token"] != "tts-xai-access" || tokens["refresh_token"] != "tts-xai-refresh" {
		t.Fatalf("xai tokens %v", tokens)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	model, _ := cfg["model"].(map[string]any)
	if model["provider"] != "openrouter" || model["base_url"] != "https://openrouter.ai/api/v1" || model["default"] != "anthropic/claude-sonnet-4" {
		t.Fatalf("model section %v", model)
	}
}
