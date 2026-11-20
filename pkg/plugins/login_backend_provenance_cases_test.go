package plugins_test

import (
	"errors"
	"maps"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Plugin services with the real login backend registry, the registry emptied around the test
func loginRegistryServices(t *testing.T) {
	t.Helper()
	browser.ResetLoginBackendRegistryForTests()
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.ProviderRegistries = maps.Clone(next.ProviderRegistries)
	if next.ProviderRegistries == nil {
		next.ProviderRegistries = map[string]plugins.ScopedRegistry{}
	}
	next.ProviderRegistries["login_backend"] = browser.LoginBackendScopedRegistry()
	plugins.InstallServices(&next)
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		browser.ResetLoginBackendRegistryForTests()
	})
}

// In process backend of the bundled source case, its password never resolved
type loginAcmeBackend struct{ browser.LoginBackendDefaults }

func (loginAcmeBackend) Name() string        { return "acme" }
func (loginAcmeBackend) Prefix() string      { return "acme:" }
func (loginAcmeBackend) DisplayName() string { return "Acme" }
func (loginAcmeBackend) ListItems() ([]vault.VaultItemMeta, error) {
	return []vault.VaultItemMeta{}, nil
}
func (loginAcmeBackend) GetMeta(string) (*vault.VaultItemMeta, error) { return nil, nil }
func (loginAcmeBackend) ResolvePassword(string) (string, error) {
	return "", errors.New("never resolved")
}

func TestBundledPluginsRegisterAndProjectOrPipPluginsDoNot(t *testing.T) {
	for _, tc := range []struct {
		source   string
		accepted bool
	}{{"bundled", true}, {"project", false}, {"entrypoint", false}} {
		t.Run(tc.source, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			loginRegistryServices(t)
			tmp := t.TempDir()
			scope := filepath.Join(tmp, "home")
			backend := &loginAcmeBackend{}
			backend.Self = backend
			manifest := &plugins.Manifest{Name: "acme-vault", Source: tc.source, Path: filepath.Join(tmp, "acme-vault")}
			handle, err := plugins.NewContext(manifest, plugins.NewManager(scope)).RegisterLoginBackend(backend)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if handle != nil {
					handle.Dispose()
				}
			}()
			if got := browser.LoginBackendProvider(t.Context(), "acme", scope) != nil; got != tc.accepted {
				t.Fatalf("source %s registered %v, want %v", tc.source, got, tc.accepted)
			}
		})
	}
}
