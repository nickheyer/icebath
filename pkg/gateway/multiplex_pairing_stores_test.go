package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Home with a default profile and a routed profile b, each with its own .env, under an active multiplexer
func multiplexAllowlistWorld(t *testing.T, profileEnv string) (root, prof string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), ".icebath")
	prof = filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prof, ".env"), []byte(profileEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("DISCORD_ALLOWED_USERS", "default-admin")
	bundled := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundled, "platforms", "discord"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundled, "platforms", "discord", "plugin.yaml"), []byte("name: discord\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetPlatformMembers(t, bundled)
	swapBundledEnv(t, "discord", "", BundledEnvRows{Authz: map[string]any{"allowed_users_env": "DISCORD_ALLOWED_USERS"}})
	wasActive := provider.MultiplexProcess()
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(wasActive) })
	return root, prof
}

// Registers declarations of a bundled platform directory for one case and puts back what was registered before
func swapBundledEnv(t *testing.T, directory, prefix string, rows BundledEnvRows) {
	t.Helper()
	bundledEnvModules.Lock()
	previous, had := bundledEnvModules.byDir[directory][prefix]
	bundledEnvModules.Unlock()
	RegisterBundledEnv(directory, prefix, rows)
	t.Cleanup(func() {
		bundledEnvModules.Lock()
		defer bundledEnvModules.Unlock()
		if had {
			bundledEnvModules.byDir[directory][prefix] = previous
			return
		}
		delete(bundledEnvModules.byDir[directory], prefix)
		if len(bundledEnvModules.byDir[directory]) == 0 {
			delete(bundledEnvModules.byDir, directory)
		}
	})
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func TestRoutedPairingGrantMirrorStaysInProfileScope(t *testing.T) {
	root, prof := multiplexAllowlistWorld(t, "DISCORD_ALLOWED_USERS=b-admin\n")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DISCORD_ALLOWED_USERS=default-admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scoped := ProfileRuntimeScope(context.Background(), prof, true)
	if err := syncAllowlistAdd(scoped, "discord", "111"); err != nil {
		t.Fatal(err)
	}
	if value, _, err := provider.GetSecret(scoped, "DISCORD_ALLOWED_USERS"); err != nil || value != "b-admin,111" {
		t.Fatalf("scoped allowlist = %q %v", value, err)
	}
	if got := readTrimmed(t, filepath.Join(prof, ".env")); got != "DISCORD_ALLOWED_USERS=b-admin,111" {
		t.Fatalf("profile .env = %q", got)
	}
	if got := readTrimmed(t, filepath.Join(root, ".env")); got != "DISCORD_ALLOWED_USERS=default-admin" {
		t.Fatalf("default .env = %q", got)
	}
	if got := os.Getenv("DISCORD_ALLOWED_USERS"); got != "default-admin" {
		t.Fatalf("process env = %q", got)
	}
	provider.SetMultiplexActive(false)
	if err := config.SaveEnvValueContext(context.Background(), root, "DISCORD_ALLOWED_USERS", "default-admin,222"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DISCORD_ALLOWED_USERS"); got != "default-admin,222" {
		t.Fatalf("process env after a single profile save = %q", got)
	}
}

func TestAllowlistScopedMissConfiguresNothing(t *testing.T) {
	_, prof := multiplexAllowlistWorld(t, "OTHER_KEY=x\n")
	scoped := ProfileRuntimeScope(context.Background(), prof, true)
	if value, err := readAllowlistEnv(scoped, "DISCORD_ALLOWED_USERS"); err != nil || value != "" {
		t.Fatalf("scoped read = %q %v", value, err)
	}
	if err := syncAllowlistAdd(scoped, "discord", "111"); err != nil {
		t.Fatal(err)
	}
	if got := readTrimmed(t, filepath.Join(prof, ".env")); got != "OTHER_KEY=x" {
		t.Fatalf("profile .env = %q", got)
	}
	if got := os.Getenv("DISCORD_ALLOWED_USERS"); got != "default-admin" {
		t.Fatalf("process env = %q", got)
	}
}

func TestPairingAllowlistRead(t *testing.T) {
	multiplexed := func(t *testing.T, envValue string) context.Context {
		t.Helper()
		t.Setenv("ICEBATH_HOME", t.TempDir())
		t.Setenv("TELEGRAM_ALLOWED_USERS", envValue)
		wasActive := provider.MultiplexProcess()
		provider.SetMultiplexActive(true)
		t.Cleanup(func() { provider.SetMultiplexActive(wasActive) })
		return context.Background()
	}
	t.Run("test_scoped_value_wins", func(t *testing.T) {
		ctx := provider.WithSecretScope(multiplexed(t, "111"), map[string]string{"TELEGRAM_ALLOWED_USERS": "222"}, "")
		if value, err := readAllowlistEnv(ctx, "TELEGRAM_ALLOWED_USERS"); err != nil || value != "222" {
			t.Fatalf("scoped read = %q %v", value, err)
		}
	})
	t.Run("test_scoped_miss_no_borrow", func(t *testing.T) {
		ctx := provider.WithSecretScope(multiplexed(t, "other-profile"), map[string]string{"UNRELATED": "x"}, "")
		if value, err := readAllowlistEnv(ctx, "TELEGRAM_ALLOWED_USERS"); err != nil || value != "" {
			t.Fatalf("scoped miss = %q %v", value, err)
		}
	})
	t.Run("test_unscoped_multiplex_falls_back_to_env", func(t *testing.T) {
		if value, err := readAllowlistEnv(multiplexed(t, "own-env"), "TELEGRAM_ALLOWED_USERS"); err != nil || value != "own-env" {
			t.Fatalf("unscoped read = %q %v", value, err)
		}
	})
}
