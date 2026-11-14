package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestBundledPlatformNamesListsShippedPlatformPlugins(t *testing.T) {
	root := t.TempDir()
	platforms := filepath.Join(root, "plugins", "platforms")
	for _, d := range []string{"Telegram", "discord", "no-manifest"} {
		os.MkdirAll(filepath.Join(platforms, d), 0o755)
	}
	os.WriteFile(filepath.Join(platforms, "Telegram", "plugin.yaml"), []byte("name: telegram\n"), 0o644)
	os.WriteFile(filepath.Join(platforms, "discord", "plugin.yaml"), []byte("name: discord\n"), 0o644)
	os.Mkdir(filepath.Join(platforms, "no-manifest", "plugin.yaml"), 0o755)
	os.WriteFile(filepath.Join(platforms, "README.md"), nil, 0o644)
	patch(t, &bundledPlatformsRepoRoot, func() string { return root })
	names, err := bundledPlatformNames()
	if err != nil || strings.Join(names, ",") != "discord,telegram" {
		t.Fatalf("%v %v", names, err)
	}
	patch(t, &bundledPlatformsRepoRoot, func() string { return filepath.Join(root, "missing") })
	if names, err := bundledPlatformNames(); err != nil || len(names) != 0 {
		t.Fatalf("%v %v", names, err)
	}
}

func TestConnectedPlatformsHookCountsTheGatewayConfigsConnectedPlatforms(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if observability.ConnectedPlatformsHook == nil {
		t.Fatal("hook not installed")
	}
	ctx := context.Background()
	if got, err := observability.ConnectedPlatformsHook(ctx); err != nil || got != 0 {
		t.Fatalf("empty home %d %v", got, err)
	}
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms:\n  telegram:\n    enabled: true\n    token: 123456789:AAHq8xZ3vRealisticBotTokenValue01\n  discord:\n    enabled: false\n    token: def\n"), 0o600)
	if got, err := observability.ConnectedPlatformsHook(ctx); err != nil || got != 1 {
		t.Fatalf("one connected %d %v", got, err)
	}
}
