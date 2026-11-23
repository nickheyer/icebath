package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/settings"
)

func TestRegisteredPlatformDisplayTierAndOverridesReachTheResolver(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	entry := NewPlatformEntry("tierchat", "Tier Chat", nil, nil)
	entry.DisplayTier = "MEDIUM"
	entry.DisplayOverrides = map[string]any{"tool_progress": "off", "busy_ack_detail": false}
	Registry.register(entry, CurrentScopeKey(ctx))
	t.Cleanup(func() { _, _ = Registry.Unregister(ctx, "tierchat", CurrentScopeKey(ctx)) })
	chat := settings.WithPlatform(ctx, "tierchat")
	if got := settings.ToolProgress.Get(chat); got != "off" {
		t.Fatalf("tool_progress = %q", got)
	}
	if settings.DisplayBusyAckDetail.Get(chat) {
		t.Fatal("busy_ack_detail override lost")
	}
	if got := settings.DisplayToolPreviewLength.Get(chat); got != 40 {
		t.Fatalf("tool_preview_length = %d", got)
	}
	if got := settings.DisplayLongRunningNotifications.Get(chat); got != "on" {
		t.Fatalf("long_running_notifications = %q", got)
	}
	if got := settings.DisplayLiveStatus.Get(chat); got != "full" {
		t.Fatalf("live_status = %q", got)
	}
	if got := settings.DisplayToolPreviewLength.Get(settings.WithPlatform(ctx, "unregistered")); got != 0 {
		t.Fatalf("unregistered platform tier leaked %d", got)
	}
}
