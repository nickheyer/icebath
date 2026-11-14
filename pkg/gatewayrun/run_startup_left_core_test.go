package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Root home using Home Assistant with a named launch profile under it, lazy installs off
func installLeftCoreHomes(t *testing.T, launchEnv string) (root, launch string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "root")
	launch = filepath.Join(root, "profiles", "work")
	for dir, env := range map[string]string{root: "HASS_TOKEN=abc\n", launch: launchEnv} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("security:\n  allow_lazy_installs: false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HASS_TOKEN", "")
	os.Unsetenv("HASS_TOKEN")
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "1")
	t.Setenv("ICEBATH_HOME", launch)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	return root, launch
}

func TestGatewayStartRecoversLeftCoreInTheDefaultRootOfANamedLaunchProfile(t *testing.T) {
	root, launch := installLeftCoreHomes(t, "")
	ctx := context.Background()
	if err := recoverLeftCoreAtGatewayStart(ctx); err != nil {
		t.Fatal(err)
	}
	kept := provider.ClaimKeptNotices(gateway.ProfileRuntimeScope(ctx, root, false))
	if len(kept) != 1 || !strings.Contains(kept[0], "Home Assistant moved out of core") || !strings.Contains(kept[0], "`icebath plugins install homeassistant`") {
		t.Fatalf("root kept %v", kept)
	}
	if kept := provider.ClaimKeptNotices(gateway.ProfileRuntimeScope(ctx, launch, false)); len(kept) != 0 {
		t.Fatalf("launch home kept %v", kept)
	}
}

func TestGatewayStartKeepsTheLaunchHomeNoticeForItsNextTurn(t *testing.T) {
	_, launch := installLeftCoreHomes(t, "HASS_TOKEN=abc\n")
	ctx := context.Background()
	if err := recoverLeftCoreAtGatewayStart(ctx); err != nil {
		t.Fatal(err)
	}
	kept := provider.ClaimKeptNotices(gateway.ProfileRuntimeScope(ctx, launch, false))
	if len(kept) != 1 || !strings.Contains(kept[0], "`icebath -p work plugins install homeassistant`") {
		t.Fatalf("launch home kept %v", kept)
	}
}
