package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

const primaryRouteYAML = "gateway:\n  multiplex_profiles: true\n  profile_routes:\n" +
	"  - name: grant-topic\n    platform: telegram\n    chat_id: '-1004306455751'\n    thread_id: '14'\n    profile: grant\n"

// Primary root whose config routes telegram to grant, the ctx served as the grant home
func multiplexHomes(t *testing.T, primaryYAML string) (string, context.Context) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	grant := filepath.Join(root, "profiles", "grant")
	if err := os.MkdirAll(grant, 0o700); err != nil {
		t.Fatal(err)
	}
	if primaryYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(primaryYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	return root, home.WithOverride(context.Background(), grant)
}

// Bundled telegram and discord platforms with a gateway config connecting neither, for one test
func patchNoConnectedPlatforms(t *testing.T) {
	t.Helper()
	registerHomePlatforms(t, "telegram", "discord")
	prev := loadGatewayConfig
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) {
		return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}, nil
	}
	t.Cleanup(func() { loadGatewayConfig = prev })
}

func TestRoutedSatellitePreflightRoutedPlatformPassesPreflight(t *testing.T) {
	_, ctx := multiplexHomes(t, primaryRouteYAML)
	patchNoConnectedPlatforms(t)
	s := openCronStore(home.Dir(ctx), nil)
	if reason := s.preflightCheckDelivery(ctx, &Job{Deliver: strPtr("telegram:-1004306455751:14")}); reason != nil {
		t.Fatalf("routed telegram blocked: %s", *reason)
	}
}

func TestRoutedSatellitePreflightUnroutedPlatformStillBlocked(t *testing.T) {
	_, ctx := multiplexHomes(t, primaryRouteYAML)
	patchNoConnectedPlatforms(t)
	s := openCronStore(home.Dir(ctx), nil)
	reason := s.preflightCheckDelivery(ctx, &Job{Deliver: strPtr("discord:12345")})
	if reason == nil || !strings.Contains(*reason, "discord") {
		t.Fatalf("reason = %v", reason)
	}
}

func TestRoutedSatellitePreflightRouteForAnotherProfileDoesNotRescue(t *testing.T) {
	root, _ := multiplexHomes(t, primaryRouteYAML)
	other := filepath.Join(root, "profiles", "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := home.WithOverride(context.Background(), other)
	if openCronStore(other, nil).deliveryPlatformRoutedFromPrimaryGateway(ctx, "telegram") {
		t.Fatal("a route naming another profile rescued this home")
	}
}

func TestRoutedSatellitePreflightPrimaryHomeItselfSkipsRouteLookup(t *testing.T) {
	root, _ := multiplexHomes(t, primaryRouteYAML)
	ctx := home.WithOverride(context.Background(), root)
	if openCronStore(root, nil).deliveryPlatformRoutedFromPrimaryGateway(ctx, "telegram") {
		t.Fatal("the primary home consulted its own routes")
	}
}

func TestRoutedSatellitePreflightMissingPrimaryConfigFailsClosed(t *testing.T) {
	_, ctx := multiplexHomes(t, "")
	patchNoConnectedPlatforms(t)
	s := openCronStore(home.Dir(ctx), nil)
	reason := s.preflightCheckDelivery(ctx, &Job{Deliver: strPtr("telegram:-1004306455751:14")})
	if reason == nil || !strings.Contains(*reason, "telegram") {
		t.Fatalf("reason = %v", reason)
	}
}

func TestRoutedSatellitePreflightDisabledRouteDoesNotRescue(t *testing.T) {
	_, ctx := multiplexHomes(t, primaryRouteYAML+"    enabled: false\n")
	if openCronStore(home.Dir(ctx), nil).deliveryPlatformRoutedFromPrimaryGateway(ctx, "telegram") {
		t.Fatal("a disabled route rescued the platform")
	}
}
