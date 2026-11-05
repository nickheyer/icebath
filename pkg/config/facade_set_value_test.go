package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestValidateConfigKeyKnownAndSuggested(t *testing.T) {
	src := testCommandSources()
	src.RuntimeKeyOwners = RuntimeKeyOwners{
		IsPresetFieldPath:   func([]string) bool { return false },
		SelectionConfigKeys: func() map[string]bool { return nil },
		ActiveManifests:     func() []PluginConfigKeys { return nil },
	}
	for _, key := range []string{"agent.max_turns", "discord.gateway_restart_notification", "mcp_servers.foo.command",
		"providers.openrouter.api_key", "gateway.platforms.my_platform.extra.token", "platform_toolsets.cli"} {
		if known, _ := validateConfigKey(key, src); !known {
			t.Fatalf("%s validated as unknown", key)
		}
	}
	for key, want := range map[string]string{
		"gateway.discord.gateway_restart_notification": "discord.gateway_restart_notification",
		"disco":                "discord",
		"agent.max_turn":       "agent.max_turns",
		"platform_toolset.cli": "platform_toolsets.cli",
	} {
		if known, suggestion := validateConfigKey(key, src); known || !strings.Contains(suggestion, want) {
			t.Fatalf("%s known %v suggestion %q", key, known, suggestion)
		}
	}
	if known, _ := validateConfigKey("agent._max_turns", src); known {
		t.Fatal("a sub key typo under a known section passed")
	}
}

func TestSplitKeyPathEscapedDot(t *testing.T) {
	for key, want := range map[string][]string{
		`providers.qwen3\.5-397b.api_key`: {"providers", "qwen3.5-397b", "api_key"},
		`qwen3\.5`:                        {"qwen3.5"},
		`a\.b\.c`:                         {"a.b.c"},
		"terminal.backend":                {"terminal", "backend"},
		"model":                           {"model"},
		`win\path.key`:                    {`win\path`, "key"},
	} {
		if got := SplitPath(key); !slices.Equal(got, want) {
			t.Fatalf("%s split %q", key, got)
		}
	}
}

func TestRedactConfigValueNestedListsAndBenignKeys(t *testing.T) {
	secret := "cfut_SUPERSECRETTOKEN1234567890abcdef"
	out, _ := RedactConfigValue(map[string]any{"default": "@cf/foo", "provider": "custom", "api_key": secret}).(map[string]any)
	if out["api_key"] == secret || strings.Contains(fmt.Sprint(out), secret) || out["default"] != "@cf/foo" || out["provider"] != "custom" {
		t.Fatalf("nested %v", out)
	}
	listed := "sk-deadbeefdeadbeefdeadbeef"
	walked := RedactConfigValue(map[string]any{"custom_providers": []any{map[string]any{"name": "p", "api_key": listed}}})
	if strings.Contains(fmt.Sprint(walked), listed) || !strings.Contains(fmt.Sprint(walked), "name:p") {
		t.Fatalf("list %v", walked)
	}
	benign := map[string]any{"token_count": 1234, "secret_santa": "alice", "max_turns": 90}
	if got := RedactConfigValue(benign); !reflect.DeepEqual(got, benign) {
		t.Fatalf("benign %v", got)
	}
}
