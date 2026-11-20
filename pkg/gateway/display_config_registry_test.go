package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestRegisteredPlatformDisplayTierAndOverridesReachTheResolver(t *testing.T) {
	ctx := context.Background()
	entry := NewPlatformEntry("tierchat", "Tier Chat", nil, nil)
	entry.DisplayTier = "MEDIUM"
	entry.DisplayOverrides = map[string]any{"tool_progress": "off", "busy_ack_detail": false}
	Registry.register(entry, CurrentScopeKey(ctx))
	t.Cleanup(func() { _, _ = Registry.Unregister(ctx, "tierchat", CurrentScopeKey(ctx)) })
	cases := map[string]any{"tool_progress": "off", "busy_ack_detail": false, "tool_preview_length": 40, "long_running_notifications": true, "live_status": "full"}
	for key, want := range cases {
		if got := config.ResolveDisplaySetting(ctx, map[string]any{}, "tierchat", key, nil); got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
	if mode, explicit := config.ResolveToolProgress(ctx, map[string]any{}, "tierchat", ""); mode != "off" || explicit {
		t.Fatalf("tool progress = %v %v", mode, explicit)
	}
	if got := config.ResolveDisplaySetting(ctx, map[string]any{}, "unregistered", "tool_preview_length", nil); got != 0 {
		t.Fatalf("unregistered platform tier leaked %#v", got)
	}
}
