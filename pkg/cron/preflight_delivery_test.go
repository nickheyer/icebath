package cron

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Replaces the known platform lookup for one test
func patchKnownDeliveryPlatform(t *testing.T, fn func(context.Context, string) bool) {
	t.Helper()
	prev := isKnownDeliveryPlatform
	isKnownDeliveryPlatform = fn
	t.Cleanup(func() { isKnownDeliveryPlatform = prev })
}

func TestPreflightIgnoresBotChatTargets(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, deliver := range []string{"bot-chat", "bot-chat:research"} {
		if err := s.preflightCheckDelivery(ctx, &Job{ID: "j1", Deliver: strPtr(deliver)}); err != nil {
			t.Fatalf("%s blocked: %s", deliver, *err)
		}
	}
}

func TestPreflightStillBlocksUnknownPlatforms(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	patchKnownDeliveryPlatform(t, func(context.Context, string) bool { return false })
	err := s.preflightCheckDelivery(context.Background(), &Job{ID: "j1", Deliver: strPtr("nonexistent-platform")})
	if err == nil || !strings.Contains(*err, "not a known") {
		t.Fatalf("err %v", err)
	}
}

func TestPreflightAndDashboardLanesPreflightBlocksUnknownFailurePlatform(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	patchKnownDeliveryPlatform(t, func(context.Context, string) bool { return false })
	err := s.preflightCheckDelivery(context.Background(), &Job{ID: "p1", Deliver: strPtr("local"), FailureDeliver: strPtr("nonexistent-platform:C1")})
	if err == nil || !strings.Contains(*err, "not a known") {
		t.Fatalf("err %v", err)
	}
}

func TestPreflightAndDashboardLanesPreflightFailureDeliverLocalAddsNoPlatforms(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	if err := s.preflightCheckDelivery(context.Background(), &Job{ID: "p2", Deliver: strPtr("local"), FailureDeliver: strPtr("local")}); err != nil {
		t.Fatalf("blocked: %s", *err)
	}
}

func TestPreflightAndDashboardLanesPreflightDuplicateLaneNotCheckedTwice(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	var seen []string
	patchKnownDeliveryPlatform(t, func(_ context.Context, p string) bool {
		seen = append(seen, p)
		return false
	})
	s.preflightCheckDelivery(context.Background(), &Job{ID: "p3", Deliver: strPtr("ghost:C1"), FailureDeliver: strPtr("ghost:C1")})
	if !reflect.DeepEqual(seen, []string{"ghost"}) {
		t.Fatalf("seen %v", seen)
	}
}

// Gateway config whose connected platforms are exactly the given ones
func patchConnectedGatewayConfig(t *testing.T, connected ...string) {
	t.Helper()
	registerHomePlatforms(t, "slack", "discord")
	cfg := &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
	for _, name := range connected {
		cfg.Platforms[gateway.Platform(name)] = &gateway.PlatformConfig{Enabled: true, Token: "test-token"}
	}
	prev := loadGatewayConfig
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) { return cfg, nil }
	t.Cleanup(func() { loadGatewayConfig = prev })
}

func TestPreflightRelayFrontedRelayFrontedSlackAccepted(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "slack")
	patchConnectedGatewayConfig(t, "relay")
	if err := s.preflightCheckDelivery(context.Background(), &Job{Deliver: strPtr("slack:D0BJTDCSR7C")}); err != nil {
		t.Fatalf("relay fronted slack blocked: %s", *err)
	}
}

func TestPreflightRelayFrontedUnfrontedPlatformStillRejected(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "slack")
	patchConnectedGatewayConfig(t, "relay")
	err := s.preflightCheckDelivery(context.Background(), &Job{Deliver: strPtr("discord:12345")})
	if err == nil || !strings.Contains(*err, "discord") {
		t.Fatalf("err %v", err)
	}
}

func TestPreflightRelayFrontedNativeStrictnessWithoutRelay(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "")
	os.Unsetenv("GATEWAY_RELAY_PLATFORMS")
	patchConnectedGatewayConfig(t, "telegram")
	err := s.preflightCheckDelivery(context.Background(), &Job{Deliver: strPtr("slack:D0BJTDCSR7C")})
	if err == nil || !strings.Contains(*err, "slack") {
		t.Fatalf("err %v", err)
	}
}
