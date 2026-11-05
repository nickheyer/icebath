package tools

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// User platform plugin manifest declaring one password secret, under plugins/platforms or flat under plugins
func writeUserPlatformPlugin(t *testing.T, root, name, secret string) string {
	t.Helper()
	dir := filepath.Join(root, "plugins", "platforms", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin.yaml")
	text := "name: " + name + "\nkind: platform\nrequires_env:\n  - name: " + secret + "\n    password: true\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func homeSecrets(t *testing.T, h string) map[string]bool {
	t.Helper()
	names, err := HomeAdapterSecretEnv(h)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestBundledPlatformSecretEnvsIsTheConfigBundledManifestScan(t *testing.T) {
	want, err := config.PlatformManifestSecretEnvs(home.Process(), config.PlatformSourceBundled, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := bundledPlatformManifestSecretEnvs(); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestUserPlatformPluginSecretsBelongToTheirOwnProfile(t *testing.T) {
	installTestEnvHooks(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "profiles", "a"), filepath.Join(root, "profiles", "b")
	manifest := writeUserPlatformPlugin(t, a, "chatx", "CHATX_SIGNING_SECRET")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if !homeSecrets(t, a)["CHATX_SIGNING_SECRET"] || homeSecrets(t, b)["CHATX_SIGNING_SECRET"] {
		t.Fatalf("a %v b %v", homeSecrets(t, a), homeSecrets(t, b))
	}
	text, _ := os.ReadFile(manifest)
	st, _ := os.Stat(manifest)
	if err := os.WriteFile(manifest, append(text, "  - name: CHATX_WEBHOOK_KEY\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(manifest, time.Now(), st.ModTime().Add(time.Second))
	if !homeSecrets(t, a)["CHATX_WEBHOOK_KEY"] {
		t.Fatalf("in place edit missed %v", homeSecrets(t, a))
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if homeSecrets(t, a)["CHATX_WEBHOOK_KEY"] {
		t.Fatal("removed plugin still declares its names")
	}
}

func TestAFailedRescanKeepsTheDenialsItAlreadyKnew(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	installTestEnvHooks(t)
	for _, layout := range []string{"platforms", "flat"} {
		h := filepath.Join(t.TempDir(), "profiles", "a")
		manifest := writeUserPlatformPlugin(t, h, "chatx", "CHATX_SIGNING_SECRET")
		locked, mode := filepath.Dir(manifest), os.FileMode(0o755)
		if layout == "flat" {
			flat := filepath.Join(h, "plugins", "chatx")
			if err := os.Rename(filepath.Dir(manifest), flat); err != nil {
				t.Fatal(err)
			}
			locked, mode = filepath.Join(flat, "plugin.yaml"), 0o644
		}
		if !homeSecrets(t, h)["CHATX_SIGNING_SECRET"] {
			t.Fatalf("%s healthy scan %v", layout, homeSecrets(t, h))
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		first, second := homeSecrets(t, h), homeSecrets(t, h)
		_ = os.Chmod(locked, mode)
		if !first["CHATX_SIGNING_SECRET"] || !second["CHATX_SIGNING_SECRET"] || !homeSecrets(t, h)["CHATX_SIGNING_SECRET"] {
			t.Fatalf("%s partial scans released a known denial %v %v", layout, first, second)
		}
		key := home.Key(h)
		homeAdapterSecretMu.Lock()
		complete := homeAdapterSecretCache[key].complete
		homeAdapterSecretMu.Unlock()
		if !complete {
			t.Fatalf("%s recovered scan was not cached", layout)
		}
	}
}

func TestASymlinkedHomeAliasSharesItsProfilesPluginDeclarations(t *testing.T) {
	installTestEnvHooks(t)
	root := t.TempDir()
	h := filepath.Join(root, "profiles", "a")
	writeUserPlatformPlugin(t, h, "chatx", "CHATX_SIGNING_SECRET")
	alias := filepath.Join(root, "a-alias")
	if err := os.Symlink(h, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	var sizes []int
	for _, bound := range []string{h, alias} {
		if !homeSecrets(t, bound)["CHATX_SIGNING_SECRET"] {
			t.Fatalf("%s misses the secret", bound)
		}
		homeAdapterSecretMu.Lock()
		sizes = append(sizes, len(homeAdapterSecretCache))
		homeAdapterSecretMu.Unlock()
	}
	if sizes[0] != sizes[1] {
		t.Fatalf("alias added a cache entry %v", sizes)
	}
}
