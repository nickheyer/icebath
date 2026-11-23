package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Home with a default profile allowing default-admin on discord and a routed profile b with its own config.yaml, under an active multiplexer
func multiplexAllowlistWorld(t *testing.T, profileConfig string) (root, prof string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), ".icebath")
	prof = filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prof, "config.yaml"), []byte(profileConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("discord:\n  allow_from: [default-admin]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
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

// Discord allow list a home's config.yaml holds
func storedDiscordAllowlist(t *testing.T, h string) []string {
	t.Helper()
	doc, err := config.ReadDocument(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := config.CfgGet(doc, "discord", "allow_from")
	return config.AsStrings(value)
}

func TestRoutedPairingGrantMirrorStaysInProfileScope(t *testing.T) {
	root, prof := multiplexAllowlistWorld(t, "discord:\n  allow_from: [b-admin]\n")
	scoped := ProfileRuntimeScope(context.Background(), prof, true)
	if err := syncAllowlistAdd(scoped, "discord", "111"); err != nil {
		t.Fatal(err)
	}
	if value, err := readAllowlistEnv(scoped, "DISCORD_ALLOWED_USERS"); err != nil || value != "b-admin,111" {
		t.Fatalf("scoped allowlist = %q %v", value, err)
	}
	if got := storedDiscordAllowlist(t, prof); !reflect.DeepEqual(got, []string{"b-admin", "111"}) {
		t.Fatalf("profile allow list = %v", got)
	}
	if got := storedDiscordAllowlist(t, root); !reflect.DeepEqual(got, []string{"default-admin"}) {
		t.Fatalf("default allow list = %v", got)
	}
	provider.SetMultiplexActive(false)
	if err := syncAllowlistAdd(context.Background(), "discord", "222"); err != nil {
		t.Fatal(err)
	}
	if got := storedDiscordAllowlist(t, root); !reflect.DeepEqual(got, []string{"default-admin", "222"}) {
		t.Fatalf("default allow list after a single profile grant = %v", got)
	}
	if got := storedDiscordAllowlist(t, prof); !reflect.DeepEqual(got, []string{"b-admin", "111"}) {
		t.Fatalf("profile allow list after the default profile's grant = %v", got)
	}
}

func TestAllowlistUnsetInTheProfileConfiguresNothing(t *testing.T) {
	root, prof := multiplexAllowlistWorld(t, "display:\n  personality: kawaii\n")
	scoped := ProfileRuntimeScope(context.Background(), prof, true)
	if value, err := readAllowlistEnv(scoped, "DISCORD_ALLOWED_USERS"); err != nil || value != "" {
		t.Fatalf("scoped read = %q %v", value, err)
	}
	if err := syncAllowlistAdd(scoped, "discord", "111"); err != nil {
		t.Fatal(err)
	}
	if got := storedDiscordAllowlist(t, prof); len(got) != 0 {
		t.Fatalf("profile allow list = %v", got)
	}
	if got := storedDiscordAllowlist(t, root); !reflect.DeepEqual(got, []string{"default-admin"}) {
		t.Fatalf("default allow list = %v", got)
	}
}

func TestPairingAllowlistReadsTheProfileOfTheScope(t *testing.T) {
	root, prof := multiplexAllowlistWorld(t, "discord:\n  allow_from: [b-admin]\n")
	if value, err := readAllowlistEnv(ProfileRuntimeScope(context.Background(), prof, true), "DISCORD_ALLOWED_USERS"); err != nil || value != "b-admin" {
		t.Fatalf("scoped read = %q %v", value, err)
	}
	if value, err := readAllowlistEnv(ProfileRuntimeScope(context.Background(), root, true), "DISCORD_ALLOWED_USERS"); err != nil || value != "default-admin" {
		t.Fatalf("default profile read = %q %v", value, err)
	}
	if value, err := readAllowlistEnv(context.Background(), "DISCORD_ALLOWED_USERS"); err != nil || value != "default-admin" {
		t.Fatalf("unscoped read = %q %v", value, err)
	}
}
