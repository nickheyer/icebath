package config

import (
	"context"
	"slices"
	"testing"
)

func TestDisplayOverrideableKeysAreTheGlobalDisplayDefaults(t *testing.T) {
	keys := DisplayOverrideableKeys()
	if len(keys) != 13 || !keys["tool_progress"] || !keys["busy_steer_ack_enabled"] || !keys["live_status"] || keys["webhook"] {
		t.Fatal(keys)
	}
}

func TestPlatformDisplayDefaultsServeTheCoreTable(t *testing.T) {
	if got := CoreDisplayPlatforms(); !slices.Equal(got, []string{"api_server", "webhook"}) {
		t.Fatalf("core platforms %v", got)
	}
	if d := PlatformDisplayDefaults(context.Background(), "webhook"); d["tool_progress"] != "off" || d["tool_preview_length"] != 0 {
		t.Fatalf("webhook defaults %v", d)
	}
	if d := PlatformDisplayDefaults(context.Background(), "api_server"); d["tool_progress"] != "all" || d["tool_preview_length"] != 0 {
		t.Fatalf("api_server defaults %v", d)
	}
}
