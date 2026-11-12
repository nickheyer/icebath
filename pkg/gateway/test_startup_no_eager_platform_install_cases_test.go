package gateway

import (
	"context"
	"testing"
)

// test_startup_no_eager_platform_install.py test_unconfigured_platform_is_not_probed_for_install
func TestUnconfiguredPlatformIsNotProbedForInstall(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	prev := Registry
	Registry = NewPlatformRegistry()
	t.Cleanup(func() { Registry = prev })
	checks := 0
	entry := NewPlatformEntry("discord", "Discord", func(context.Context, *PlatformConfig) (any, error) { return &registryCaseAdapter{}, nil },
		func() bool { checks++; return true })
	entry.IsConnected = func(context.Context, *PlatformConfig) (bool, error) { return false, nil }
	entry.Source = "plugin"
	if err := Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultGatewayConfig(ctx)
	applyEnvOverrides(ctx, cfg, "")
	if checks != 0 {
		t.Fatalf("check_fn called %d times", checks)
	}
	if pc := cfg.Platforms[PlatformDiscord]; pc != nil && pc.Enabled {
		t.Fatal("discord enabled without configuration")
	}
}
