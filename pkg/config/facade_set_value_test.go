package config

import (
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

func TestSecretRedactionInDisplay(t *testing.T) {
	t.Run("test_redact_config_value_masks_nested_api_key", func(t *testing.T) {
		secret := "cfut_SUPERSECRETTOKEN1234567890abcdef"
		out, _ := RedactConfigValue(map[string]any{"default": "@cf/foo", "provider": "custom", "api_key": secret}).(map[string]any)
		if out["api_key"] == secret || strings.Contains(PyStr(out), secret) || out["default"] != "@cf/foo" || out["provider"] != "custom" {
			t.Fatalf("nested %v", out)
		}
	})
	t.Run("test_redact_config_value_walks_lists", func(t *testing.T) {
		secret := "sk-deadbeefdeadbeefdeadbeef"
		walked, _ := RedactConfigValue(map[string]any{"custom_providers": []any{map[string]any{"name": "p", "api_key": secret}}}).(map[string]any)
		entries, _ := walked["custom_providers"].([]any)
		if strings.Contains(PyStr(walked), secret) || len(entries) != 1 || AsMap(entries[0])["name"] != "p" {
			t.Fatalf("list %v", walked)
		}
	})
	t.Run("test_redact_config_value_ignores_benign_keys", func(t *testing.T) {
		benign := map[string]any{"token_count": 1234, "secret_santa": "alice", "max_turns": 90}
		if got := RedactConfigValue(benign); !reflect.DeepEqual(got, benign) {
			t.Fatalf("benign %v", got)
		}
	})
}

func TestValidateConfigKey(t *testing.T) {
	src := pathCommandSources()
	t.Run("test_known_keys_pass", func(t *testing.T) {
		for _, key := range []string{"agent.max_turns", "discord.gateway_restart_notification", "mcp_servers.foo.command",
			"providers.openrouter.api_key", "gateway.platforms.my_platform.extra.token", "platform_toolsets.cli"} {
			if known, _ := validateConfigKey(key, src); !known {
				t.Fatalf("Expected %s to validate as known", PyRepr(key))
			}
		}
	})
	t.Run("test_unknown_keys_with_suggestion", func(t *testing.T) {
		for key, want := range map[string]string{"gateway.discord.gateway_restart_notification": "discord.gateway_restart_notification",
			"disco": "discord", "agent.max_turn": "agent.max_turns", "platform_toolset.cli": "platform_toolsets.cli"} {
			known, suggestion := validateConfigKey(key, src)
			if known || !strings.Contains(suggestion, want) {
				t.Fatalf("%s known %v suggestion %q", key, known, suggestion)
			}
		}
	})
	t.Run("test_underscore_only_first_segment_escapes", func(t *testing.T) {
		if known, _ := validateConfigKey("agent._max_turns", src); known {
			t.Fatal("Sub-key typo under a known top-level key must still be flagged")
		}
	})
}

func TestGatewayPlatformsPrefixRedirect(t *testing.T) {
	t.Run("test_nested_display_setting_still_reaches_display_platforms", func(t *testing.T) {
		src := pathCommandSources()
		src.DisplayOverrideableKeys = DisplayOverrideableKeys()
		if key, _ := redirectPlatformDisplayKey("gateway.platforms.telegram.streaming", src); key != "display.platforms.telegram.streaming" {
			t.Fatalf("key %s", key)
		}
	})
}
