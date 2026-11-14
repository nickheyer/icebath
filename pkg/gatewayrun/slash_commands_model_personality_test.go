package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Home whose config.yaml holds data, both the gateway read and the personality write rooted there
func personalityHome(t *testing.T, data map[string]any) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	raw, err := yaml.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(h, "config.yaml")
}

// Saved config.yaml as a plain mapping
func savedPersonalityConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]any{}
	if err := yaml.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

// Personality command event carrying args
func personalityEvent(args string) *gateway.MessageEvent {
	event := gateway.NewMessageEvent("/personality " + args)
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c", ChatType: "dm"}
	return event
}

// test_personality_none.py TestGatewayPersonalityNone.test_default_clears_ephemeral_prompt
func TestGatewayPersonalityNoneDefaultClearsEphemeralPrompt(t *testing.T) {
	path := personalityHome(t, map[string]any{
		"agent":   map[string]any{"system_prompt": "manual forever", "personalities": map[string]any{"helpful": "You are helpful."}},
		"display": map[string]any{"personality": "helpful"},
	})
	r := allocGatewayRunner()
	ctx := context.Background()
	if _, err := r.handlePersonalityCommand(ctx, personalityEvent("default")); err != nil {
		t.Fatal(err)
	}
	saved := savedPersonalityConfig(t, path)
	agentCfg, _ := saved["agent"].(map[string]any)
	if agentCfg["system_prompt"] != "manual forever" {
		t.Fatalf("system_prompt = %#v", agentCfg["system_prompt"])
	}
	display, _ := saved["display"].(map[string]any)
	if display["personality"] != "" {
		t.Fatalf("display.personality = %#v", display["personality"])
	}
	config.ResetCache()
	if got := r.getSystemPromptForChannel(ctx, "", "c", nil, nil); got != "manual forever" {
		t.Fatalf("channel prompt = %q", got)
	}
}

// test_personality_none.py TestGatewayPersonalityNone.test_set_persists_display_personality_not_system_prompt
func TestGatewayPersonalityNoneSetPersistsDisplayPersonalityNotSystemPrompt(t *testing.T) {
	path := personalityHome(t, map[string]any{
		"agent": map[string]any{"system_prompt": "manual forever", "personalities": map[string]any{"helpful": "You are helpful."}},
	})
	r := allocGatewayRunner()
	ctx := context.Background()
	result, err := r.handlePersonalityCommand(ctx, personalityEvent("helpful"))
	if err != nil {
		t.Fatal(err)
	}
	saved := savedPersonalityConfig(t, path)
	agentCfg, _ := saved["agent"].(map[string]any)
	if agentCfg["system_prompt"] != "manual forever" {
		t.Fatalf("system_prompt = %#v", agentCfg["system_prompt"])
	}
	display, _ := saved["display"].(map[string]any)
	if display["personality"] != "helpful" {
		t.Fatalf("display.personality = %#v", display["personality"])
	}
	config.ResetCache()
	if got := r.getSystemPromptForChannel(ctx, "", "c", nil, nil); got != "You are helpful." {
		t.Fatalf("channel prompt = %q", got)
	}
	if !strings.Contains(strings.ToLower(result), "helpful") {
		t.Fatalf("result = %q", result)
	}
}
