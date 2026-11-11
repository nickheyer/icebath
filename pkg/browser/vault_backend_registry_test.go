package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/providerregistry"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Plugin login backend double that must never be asked for a password
func exampleBackend(t *testing.T) *fakeLoginBackend {
	t.Helper()
	b := &fakeLoginBackend{name: "example", display: "Example", prefix: "example:"}
	b.Self = b
	b.listItems = func() ([]vault.VaultItemMeta, error) { return []vault.VaultItemMeta{}, nil }
	b.resolvePassword = func(string) (string, error) {
		t.Error("registry tests must never resolve passwords")
		return "", errors.New("registry tests must never resolve passwords")
	}
	return b
}

func loginRegistryFixture(t *testing.T) {
	t.Helper()
	ResetLoginBackendRegistryForTests()
	t.Cleanup(ResetLoginBackendRegistryForTests)
}

// Plugin services carrying only the login backend registry, restored when the test ends
func loginPluginServices(t *testing.T) {
	t.Helper()
	loginRegistryFixture(t)
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"login_backend": LoginBackendScopedRegistry()}})
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		plugins.ResetPluginManagersForTests()
	})
}

func isRegistryValueError(err error) bool {
	var verr *providerregistry.ValueError
	return errors.As(err, &verr)
}

func TestRegistrationRequiresExplicitProfileEnablement(t *testing.T) {
	ctx, _ := vaultProfile(t)
	loginRegistryFixture(t)
	pin(t, &isLoginBackendEnabledFn, func(context.Context, string) (bool, error) { return false, nil })
	cfg := map[string]any{}
	pin(t, &vaultCfgFn, func(context.Context) (map[string]any, error) { return cfg, nil })
	if err := RegisterLoginBackendProvider(ctx, exampleBackend(t), ""); err != nil {
		t.Fatal(err)
	}
	if backend, err := LoginBackendForHandle(ctx, "example:one"); err != nil || backend != nil {
		t.Fatalf("backend %v %v", backend, err)
	}
	cfg = map[string]any{"example": map[string]any{"enabled": true}}
	if backend, err := LoginBackendForHandle(ctx, "example:one"); err != nil || backend == nil || backend.Name() != "example" {
		t.Fatalf("backend %v %v", backend, err)
	}
}

func TestBuiltinNamesAndHandlePrefixesAreReserved(t *testing.T) {
	ctx, _ := vaultProfile(t)
	loginRegistryFixture(t)
	reserved := exampleBackend(t)
	reserved.name = "local"
	if err := RegisterLoginBackendProvider(ctx, reserved, ""); !isRegistryValueError(err) {
		t.Fatalf("reserved name err %v", err)
	}
	reservedPrefix := exampleBackend(t)
	reservedPrefix.prefix = "op:"
	if err := RegisterLoginBackendProvider(ctx, reservedPrefix, ""); !isRegistryValueError(err) {
		t.Fatalf("reserved prefix err %v", err)
	}
}

func TestReloadCannotChangePrefixAndLeaveAmbiguousRestore(t *testing.T) {
	loginPluginServices(t)
	scope := filepath.Join(t.TempDir(), "home")
	pctx := plugins.NewPluginContext(&plugins.Manifest{Name: "login-example", Source: "bundled"}, plugins.NewManager(scope))
	original := exampleBackend(t)
	first, err := pctx.RegisterLoginBackend(original)
	if err != nil || first == nil {
		t.Fatalf("first registration %v %v", first, err)
	}
	changed := exampleBackend(t)
	changed.prefix = "changed:"
	if handle, _ := pctx.RegisterLoginBackend(changed); handle != nil {
		t.Fatal("a changed prefix took the name over")
	}
	other := exampleBackend(t)
	other.name = "other"
	if handle, _ := pctx.RegisterLoginBackend(other); handle != nil {
		t.Fatal("a name whose prefix names another backend registered")
	}
	if got := loginBackendRegistry.GetProvider(context.Background(), "example", scope); got != LoginBackend(original) {
		t.Fatalf("provider %v", got)
	}
	if err := first.Dispose(); err != nil {
		t.Fatal(err)
	}
	if got := loginBackendRegistry.GetProvider(context.Background(), "example", scope); got != nil {
		t.Fatalf("provider after dispose %v", got)
	}
}

func TestLeaseReloadAndUnloadDoNotCrossProfiles(t *testing.T) {
	loginPluginServices(t)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	manifest := &plugins.Manifest{Name: "test-vault", Source: "bundled"}
	firstCtx := plugins.NewPluginContext(manifest, plugins.NewManager(first))
	secondCtx := plugins.NewPluginContext(manifest, plugins.NewManager(second))
	original, replacement, sibling := exampleBackend(t), exampleBackend(t), exampleBackend(t)
	a, err := firstCtx.RegisterLoginBackend(original)
	if err != nil || a == nil {
		t.Fatalf("first registration %v %v", a, err)
	}
	if handle, _ := firstCtx.RegisterLoginBackend(replacement); handle != nil {
		t.Fatal("a held name was taken over")
	}
	c, err := secondCtx.RegisterLoginBackend(sibling)
	if err != nil || c == nil {
		t.Fatalf("sibling registration %v %v", c, err)
	}
	get := func(scope string) LoginBackend {
		return loginBackendRegistry.GetProvider(context.Background(), "example", scope)
	}
	if get(first) != LoginBackend(original) || get(second) != LoginBackend(sibling) {
		t.Fatalf("providers %v %v", get(first), get(second))
	}
	if err := a.Dispose(); err != nil {
		t.Fatal(err)
	}
	if get(first) != nil || get(second) != LoginBackend(sibling) {
		t.Fatalf("providers after dispose %v %v", get(first), get(second))
	}
	if err := c.Dispose(); err != nil {
		t.Fatal(err)
	}
}

// Compiled in plugin whose registration hands a fresh backend to the profile's login backend registry
type loginExamplePlugin struct {
	t   *testing.T
	key string
	new func() LoginBackend
}

func (p loginExamplePlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: p.key, Key: p.key}
}

func (p loginExamplePlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterLoginBackend(p.new())
	return err
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRealPluginDiscoveryReloadAndDisable(t *testing.T) {
	ctx, env := vaultProfile(t)
	loginPluginServices(t)
	h := env.Home
	t.Cleanup(plugins.SwapBundled(loginExamplePlugin{t: t, key: "login_example", new: func() LoginBackend { return exampleBackend(t) }}))
	config := filepath.Join(h, "config.yaml")
	writeFile(t, config, "plugins:\n  enabled: [login_example]\nvault:\n  example:\n    enabled: true\n")
	manager := plugins.NewManager(h)
	defer manager.Unload(nil)
	manager.DiscoverAndLoad(ctx, false)
	first, err := LoginBackendForHandle(ctx, "example:one")
	if err != nil || first == nil {
		t.Fatalf("backend after discovery %v %v", first, err)
	}
	manager.DiscoverAndLoad(ctx, true)
	second, err := LoginBackendForHandle(ctx, "example:one")
	if err != nil || second == nil || second.(*PluginLoginBackend).plugin == first.(*PluginLoginBackend).plugin {
		t.Fatalf("backend after reload %v %v", second, err)
	}
	writeFile(t, config, "plugins:\n  enabled: []\nvault:\n  example:\n    enabled: true\n")
	manager.DiscoverAndLoad(ctx, true)
	if backend, err := LoginBackendForHandle(ctx, "example:one"); err != nil || backend != nil {
		t.Fatalf("backend after disable %v %v", backend, err)
	}
}

func TestHostPluginRefusesServerLocalFillContext(t *testing.T) {
	reason, refused := plugins.HostUnsupportedCtxMethods["register_login_backend"]
	if !refused || !strings.Contains(reason, "server-local") {
		t.Fatalf("the plugin host does not refuse register_login_backend as server-local: %q", reason)
	}
}
