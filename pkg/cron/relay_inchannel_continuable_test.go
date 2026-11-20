package cron

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Platform config carrying only the extras
func relaySurfaceConfig(extra map[string]any) *gateway.PlatformConfig {
	cfg := gateway.NewPlatformConfig()
	cfg.Extra = extra
	return cfg
}

func TestSurfaceKnobResolutionNativeFlatKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := resolveCronSurfaceMode(relaySurfaceConfig(map[string]any{"cron_continuable_surface": "in_channel"}), "slack"); got != "in_channel" {
		t.Fatalf("mode %q", got)
	}
}

func TestSurfaceKnobResolutionRelaySlackSubblock(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := relaySurfaceConfig(map[string]any{"slack": map[string]any{"cron_continuable_surface": "in_channel"}})
	if got := resolveCronSurfaceMode(cfg, "slack"); got != "in_channel" {
		t.Fatalf("mode %q", got)
	}
}

func TestSurfaceKnobResolutionSubblockIsPerLogicalPlatform(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := relaySurfaceConfig(map[string]any{"slack": map[string]any{"cron_continuable_surface": "in_channel"}})
	if got := resolveCronSurfaceMode(cfg, "discord"); got != "thread" {
		t.Fatalf("mode %q", got)
	}
}

func TestSurfaceKnobResolutionDefaultThread(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := resolveCronSurfaceMode(relaySurfaceConfig(map[string]any{}), "slack"); got != "thread" {
		t.Fatalf("mode %q", got)
	}
}

func TestSurfaceKnobResolutionSubblockWinsOverFlatKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := relaySurfaceConfig(map[string]any{"cron_continuable_surface": "thread", "slack": map[string]any{"cron_continuable_surface": "in_channel"}})
	if got := resolveCronSurfaceMode(cfg, "slack"); got != "in_channel" {
		t.Fatalf("mode %q", got)
	}
}

func TestSurfaceKnobResolutionNonePconfigDefaultsThread(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := resolveCronSurfaceMode(nil, "slack"); got != "thread" {
		t.Fatalf("mode %q", got)
	}
}
