package config

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The per platform display settings gateway display_config resolves, its OVERRIDEABLE_KEYS
var gatewayOverrideableKeys = setOf("tool_progress", "tool_progress_grouping", "show_reasoning", "reasoning_style",
	"tool_preview_length", "streaming", "interim_assistant_messages", "suppress_warning_notifications",
	"long_running_notifications", "busy_ack_detail", "busy_steer_ack_enabled", "cleanup_progress", "live_status")

func redirectSources() ConfigCommandSources {
	src := pathCommandSources()
	src.DisplayOverrideableKeys = gatewayOverrideableKeys
	return src
}

const redirectConfig = `{"model": {"default": "test-model", "provider": "openrouter"}, "platforms": {"telegram": {"token": "secret-bot-token"}},
 "display": {"skin": "default", "platforms": {"telegram": {"show_reasoning": true}}}}`

// Home seeded with the redirect fixture or the given text, stdout captured
func redirectHome(t *testing.T, text string) (string, string, func() string) {
	t.Helper()
	if text == "" {
		text = redirectConfig
	}
	h, path := facadeHome(t, text)
	out, _ := captureFacade(t)
	return h, path, func() string { s := out.String(); out.Reset(); return s }
}

func redirectSet(t *testing.T, h, key, value string) {
	t.Helper()
	if err := SetConfigValue(context.Background(), h, key, value, false, redirectSources()); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

func TestStreamingRedirectsToDisplayPlatforms(t *testing.T) {
	h, path, _ := redirectHome(t, "")
	redirectSet(t, h, "platforms.telegram.streaming", "false")
	if v, _ := readSaved(t, path, "display", "platforms", "telegram", "streaming"); v != false {
		t.Fatalf("display streaming %v", v)
	}
	if _, ok := readSaved(t, path, "platforms", "telegram", "streaming"); ok {
		t.Fatal("streaming landed on the connection block")
	}
	if v, _ := readSaved(t, path, "platforms", "telegram", "token"); v != "secret-bot-token" {
		t.Fatalf("token %v", v)
	}
}

func TestConnectionKeyNotRedirected(t *testing.T) {
	h, path, _ := redirectHome(t, "")
	redirectSet(t, h, "platforms.telegram.token", "new-token")
	if v, _ := readSaved(t, path, "platforms", "telegram", "token"); v != "new-token" {
		t.Fatalf("token %v", v)
	}
	if _, ok := readSaved(t, path, "display", "platforms", "telegram", "token"); ok {
		t.Fatal("token redirected to display")
	}
}

func TestNoTopLevelPlatformsCreatedWhenMissing(t *testing.T) {
	h, path, _ := redirectHome(t, "model:\n  default: m\n")
	redirectSet(t, h, "platforms.telegram.streaming", "true")
	if v, _ := readSaved(t, path, "display", "platforms", "telegram", "streaming"); v != true {
		t.Fatalf("display streaming %v", v)
	}
	if _, ok := readSaved(t, path, "platforms"); ok {
		t.Fatal("a stray top level platforms block was written")
	}
}

func TestGetMirrorsTheRedirectedLeafAfterSet(t *testing.T) {
	h, _, stdout := redirectHome(t, "")
	redirectSet(t, h, "platforms.telegram.streaming", "false")
	stdout()
	if err := GetConfigValue(context.Background(), h, "platforms.telegram.streaming", false, false, redirectSources()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout()); got != "false" {
		t.Fatalf("get printed %q", got)
	}
}

func TestUnsetRemovesTheRedirectedLeaf(t *testing.T) {
	h, path, _ := redirectHome(t, "")
	redirectSet(t, h, "platforms.telegram.streaming", "false")
	if err := UnsetConfigValue(context.Background(), h, "platforms.telegram.streaming", redirectSources()); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSaved(t, path, "display", "platforms", "telegram", "streaming"); ok {
		t.Fatal("redirected leaf survived unset")
	}
	if v, _ := readSaved(t, path, "display", "platforms", "telegram", "show_reasoning"); v != true {
		t.Fatalf("show_reasoning %v", v)
	}
	if v, _ := readSaved(t, path, "platforms", "telegram"); !reflect.DeepEqual(v, map[string]any{"token": "secret-bot-token"}) {
		t.Fatalf("connection block %v", v)
	}
}

func TestUnsetMissingRedirectedLeafExitsNonzero(t *testing.T) {
	h, _, _ := redirectHome(t, "")
	if err := UnsetConfigValue(context.Background(), h, "platforms.telegram.streaming", redirectSources()); exitCode(err) != 1 {
		t.Fatalf("unset of a missing leaf returned %v", err)
	}
}

func TestRedirectHelperOnlyTouchesKnownDisplayKeys(t *testing.T) {
	src := redirectSources()
	for setting := range gatewayOverrideableKeys {
		canonical, note := redirectPlatformDisplayKey("platforms.discord."+setting, src)
		if canonical != "display.platforms.discord."+setting || note == "" {
			t.Fatalf("%s redirected to %q note %q", setting, canonical, note)
		}
	}
	for _, key := range []string{"platforms.telegram.token", "platforms.telegram.reply_to_mode", "platforms.telegram.extra.foo",
		"platforms.telegram", "display.platforms.telegram.streaming", "streaming.enabled"} {
		if canonical, note := redirectPlatformDisplayKey(key, src); canonical != key || note != "" {
			t.Fatalf("%s redirected to %q note %q", key, canonical, note)
		}
	}
}
