package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Creates a --clone profile of the default home
func cloneConfig(t *testing.T, name string, syncImports bool) string {
	t.Helper()
	dir, err := CreateProfile(context.Background(), name, CreateOptions{CloneConfig: true, SyncImports: syncImports})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCloneConfigCopiesOnlyTheActiveMemoryProvidersConfig(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "memory:\n  provider: hindsight\n")
	payload := `{"mode": "local_embedded", "bank_id": "hermes", "apiKey": "hs-secret"}`
	writeFile(t, filepath.Join(def, "hindsight", "config.json"), payload)
	writeFile(t, filepath.Join(def, "mem0.json"), `{"agent_id": "hermes"}`)
	dir := cloneConfig(t, "coder", false)
	cloned := filepath.Join(dir, "hindsight", "config.json")
	if readText(t, cloned) != payload {
		t.Fatal("active provider config not cloned")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(cloned); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("cloned mode %v %v", st.Mode().Perm(), err)
		}
	}
	if exists(filepath.Join(dir, "mem0.json")) {
		t.Fatal("inactive provider config cloned")
	}
}

func TestCloneConfigCopiesSourcePlugins(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	writeFile(t, filepath.Join(tmp, ".icebath", "plugins", "example-plugin", "plugin.yaml"), "name: example-plugin\n")
	dir := cloneConfig(t, "coder", false)
	if readText(t, filepath.Join(dir, "plugins", "example-plugin", "plugin.yaml")) != "name: example-plugin\n" {
		t.Fatal("plugin not cloned")
	}
}

func TestCloneConfigKeepsPluginProvenanceAndSkipsInstallStaging(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	plugins := filepath.Join(tmp, ".icebath", "plugins")
	writeFile(t, filepath.Join(plugins, "acme-memory", "plugin.yaml"), "name: acme-memory\nkind: memory\n")
	for _, d := range []string{filepath.Join(plugins, "acme-memory", "__pycache__"), filepath.Join(plugins, ".install-abc123", "plugin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	record := `{"acme-memory": {"pinned": true, "revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source": "https://example.invalid/a.git"}}`
	writeFile(t, filepath.Join(plugins, ".install-metadata.json"), record)
	writeFile(t, filepath.Join(tmp, ".icebath", "config.yaml"), "memory:\n  provider: acme-memory\n")
	cloned := filepath.Join(cloneConfig(t, "coder", false), "plugins")
	if readText(t, filepath.Join(cloned, ".install-metadata.json")) != record {
		t.Fatal("install record not cloned")
	}
	if st, err := os.Stat(filepath.Join(cloned, "acme-memory", "plugin.yaml")); err != nil || !st.Mode().IsRegular() {
		t.Fatal("plugin.yaml not cloned")
	}
	if exists(filepath.Join(cloned, "acme-memory", "__pycache__")) || exists(filepath.Join(cloned, ".install-abc123")) {
		t.Fatal("pycache or install staging cloned")
	}
}

func TestCloneConfigIgnoresUnsafeMemoryProviderNames(t *testing.T) {
	for _, provider := range []string{"../outside", "a/b", "..", "hind sight"} {
		t.Run(provider, func(t *testing.T) {
			tmp := manageEnv(t)
			captureOut(t)
			def := filepath.Join(tmp, ".icebath")
			writeFile(t, filepath.Join(def, "config.yaml"), "memory:\n  provider: '"+provider+"'\n")
			writeFile(t, filepath.Join(tmp, "outside", "config.json"), "{}")
			writeFile(t, filepath.Join(def, "a", "b", "config.json"), "{}")
			dir := cloneConfig(t, "coder", false)
			if exists(filepath.Join(dir, "a")) || exists(filepath.Join(filepath.Dir(dir), "outside")) || exists(filepath.Join(dir, "hind sight")) {
				t.Fatal("copy escaped the provider name check")
			}
		})
	}
}

func TestCloneSyncImportsCarriesManifestButNeverLinksProfiles(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	manifest := map[string]any{"version": float64(1), "agents": map[string]any{"claude-code": map[string]any{
		"source": filepath.Join(tmp, ".claude"), "digest": "d", "overwrite": false,
		"last_import": float64(1), "imported_skills": []any{"s1"}}}}
	data, _ := json.Marshal(manifest)
	writeFile(t, filepath.Join(def, "import-sync.json"), string(data))
	plain := cloneConfig(t, "plain", false)
	if exists(filepath.Join(plain, "import-sync.json")) {
		t.Fatal("manifest carried without --sync-imports")
	}
	synced := cloneConfig(t, "synced", true)
	var got map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(synced, "import-sync.json"))), &got); err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatalf("manifest %v %v", got, err)
	}
	writeFile(t, filepath.Join(def, "config.yaml"), "model: changed")
	parsed, err := config.LoadYAMLFileReadonly(filepath.Join(synced, "config.yaml"))
	cfg, _ := parsed.(map[string]any)
	if err != nil || cfg["model"] != "test" {
		t.Fatalf("clone config %v %v", cfg, err)
	}
}
