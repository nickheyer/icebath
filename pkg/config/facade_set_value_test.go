package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

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
		if out["api_key"] == secret || strings.Contains(anyutil.String(out), secret) || out["default"] != "@cf/foo" || out["provider"] != "custom" {
			t.Fatalf("nested %v", out)
		}
	})
	t.Run("test_redact_config_value_walks_lists", func(t *testing.T) {
		secret := "sk-deadbeefdeadbeefdeadbeef"
		walked, _ := RedactConfigValue(map[string]any{"fallback_providers": []any{map[string]any{"name": "p", "api_key": secret}}}).(map[string]any)
		entries, _ := walked["fallback_providers"].([]any)
		if strings.Contains(anyutil.String(walked), secret) || len(entries) != 1 || AsMap(entries[0])["name"] != "p" {
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
