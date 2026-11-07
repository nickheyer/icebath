package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type ssbOrchestrator struct {
	sources              []SecretSourceInfo
	reset, load, refresh int
}

func (o *ssbOrchestrator) ListPluginSources() []SecretSourceInfo { return o.sources }
func (o *ssbOrchestrator) ResetCache(string) error               { o.reset++; return nil }
func (o *ssbOrchestrator) LoadDotenv(string) error               { o.load++; return nil }
func (o *ssbOrchestrator) RefreshInstalledScope(string) error    { o.refresh++; return nil }

// Source whose activation follows the base enabled key
func ssbStub(name string) SecretSourceInfo {
	return SecretSourceInfo{Name: name, IsEnabled: func(cfg map[string]any) (bool, error) {
		on, _ := cfg["enabled"].(bool)
		return on, nil
	}}
}

// Source that ignores enabled and activates when a vault_id is present
func ssbCustom(name string) SecretSourceInfo {
	return SecretSourceInfo{Name: name, IsEnabled: func(cfg map[string]any) (bool, error) {
		id, _ := cfg["vault_id"].(string)
		return id != "", nil
	}}
}

// Runs one post discovery refresh over a home whose config holds the secrets section
func ssbRefresh(t *testing.T, secrets map[string]any, sources ...SecretSourceInfo) *ssbOrchestrator {
	t.Helper()
	dir := t.TempDir()
	if secrets != nil {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(dumpYAML(t, map[string]any{"secrets": secrets})), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o := &ssbOrchestrator{sources: sources}
	previous := installedServices()
	InstallServices(&Services{SecretSources: o})
	t.Cleanup(func() { InstallServices(previous) })
	NewManager(dir).refreshSecretSourcesAfterDiscovery()
	return o
}

func TestRefreshSecretSourcesNoopWithoutPluginSources(t *testing.T) {
	if o := ssbRefresh(t, nil); o.reset != 0 || o.load != 0 {
		t.Fatalf("reset %d load %d", o.reset, o.load)
	}
}

// Bundled sources never reach the plugin source list so an enabled builtin never re-pulls
func TestRefreshSecretSourcesNoopWhenOnlyBuiltins(t *testing.T) {
	if o := ssbRefresh(t, map[string]any{"bitwarden": map[string]any{"enabled": true}}); o.reset != 0 || o.load != 0 {
		t.Fatalf("reset %d load %d", o.reset, o.load)
	}
}

func TestRefreshSecretSourcesRepullsWhenPluginEnabled(t *testing.T) {
	if o := ssbRefresh(t, map[string]any{"myvault": map[string]any{"enabled": true}}, ssbStub("myvault")); o.reset != 1 || o.load != 1 {
		t.Fatalf("reset %d load %d", o.reset, o.load)
	}
}

func TestRefreshRespectsCustomIsEnabled(t *testing.T) {
	if o := ssbRefresh(t, map[string]any{"myvault": map[string]any{"vault_id": "abc123"}}, ssbCustom("myvault")); o.reset != 1 || o.load != 1 {
		t.Fatalf("reset %d load %d", o.reset, o.load)
	}
}

func TestRefreshSkipsCustomSourceWhenNotActivated(t *testing.T) {
	if o := ssbRefresh(t, map[string]any{"myvault": map[string]any{"enabled": true}}, ssbCustom("myvault")); o.reset != 0 || o.load != 0 {
		t.Fatalf("reset %d load %d", o.reset, o.load)
	}
}

func TestRefreshSkipsSourceWhoseIsEnabledRaises(t *testing.T) {
	boom := SecretSourceInfo{Name: "myvault", IsEnabled: func(map[string]any) (bool, error) { return false, errors.New("boom") }}
	panics := SecretSourceInfo{Name: "myvault", IsEnabled: func(map[string]any) (bool, error) { panic("boom") }}
	for _, src := range []SecretSourceInfo{boom, panics} {
		if o := ssbRefresh(t, map[string]any{"myvault": map[string]any{"enabled": true}}, src); o.reset != 0 || o.load != 0 {
			t.Fatalf("reset %d load %d", o.reset, o.load)
		}
	}
}
