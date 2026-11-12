package plugins_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/vault"
)

const loginProvenancePlugin = `
from agent.vault_backends.base import LoginBackend


class Backend(LoginBackend):
    name, prefix, display_name = "acme", "acme:", "Acme"

    def list_items(self):
        return []

    def get_meta(self, handle):
        return None

    def resolve_password(self, handle):
        raise AssertionError("provenance tests never resolve passwords")


def register(ctx):
    ctx.register_login_backend(Backend())
`

const loginProvenanceRule = "verified or official"

type loginLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *loginLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *loginLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Plugin log lines of the test, the previous logger restored when it ends
func loginCaptureLogs(t *testing.T) *loginLogBuffer {
	t.Helper()
	buf := &loginLogBuffer{}
	saved := plugins.Logger
	plugins.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { plugins.Logger = saved })
	return buf
}

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

// Login backend plugin world at tier verified with the real registry behind its plugin context
func loginProvenanceWorld(t *testing.T) *plugins.LoginWorld {
	t.Helper()
	world := plugins.NewLoginWorld(t, "acme-vault", loginProvenancePlugin)
	world.SetTier("verified")
	loginRegistryServices(t)
	return world
}

// Loads the profile with the plugin and its vault section enabled and returns the acme backend of its scope
func loginProvenanceLoad(t *testing.T, world *plugins.LoginWorld) browser.LoginBackend {
	t.Helper()
	world.Load(t, "plugins:\n  enabled: [acme-vault]\nvault:\n  acme: {enabled: true}\n")
	return browser.LoginBackendProvider(world.Ctx(), "acme", world.Home())
}

func TestCatalogInstallAtVerifiedOrOfficialTierRegisters(t *testing.T) {
	for _, tier := range []string{"verified", "official"} {
		t.Run(tier, func(t *testing.T) {
			world := loginProvenanceWorld(t)
			world.SetTier(tier)
			if err := world.InstallCatalogEntry("", false); err != nil {
				t.Fatal(err)
			}
			backend := loginProvenanceLoad(t, world)
			if backend == nil || backend.Name() != "acme" {
				t.Fatalf("tier %s registered %v", tier, backend)
			}
			enabled := browser.EnabledPluginLoginBackends(world.Ctx(), map[string]any{"acme": map[string]any{"enabled": true}})
			if len(enabled) != 1 || enabled[0].Name() != "acme" || enabled[0].DisplayName() != "acme (plugin)" {
				t.Fatalf("enabled plugin backends %v", enabled)
			}
			items, err := enabled[0].ListItems()
			if err != nil || len(items) != 0 {
				t.Fatalf("hosted list_items %v %v", items, err)
			}
			meta, err := enabled[0].GetMeta("acme:one")
			if err != nil || meta != nil {
				t.Fatalf("hosted get_meta %v %v", meta, err)
			}
			_, err = enabled[0].ResolvePassword("acme:one")
			var failed *browser.PluginBackendError
			if !errors.As(err, &failed) || err.Error() != "The acme login backend failed." {
				t.Fatalf("hosted resolve_password raising %v", err)
			}
		})
	}
}

func TestCommunityCatalogInstallIsRefused(t *testing.T) {
	world := loginProvenanceWorld(t)
	world.SetTier("community")
	if err := world.InstallCatalogEntry("", false); err != nil {
		t.Fatal(err)
	}
	logs := loginCaptureLogs(t)
	if backend := loginProvenanceLoad(t, world); backend != nil {
		t.Fatalf("community install registered %v", backend)
	}
	if text := logs.String(); !strings.Contains(text, loginProvenanceRule) || !strings.Contains(text, "tier community") {
		t.Fatalf("refusal log %s", text)
	}
}

func TestLocalDirectoryPluginIsRefused(t *testing.T) {
	world := loginProvenanceWorld(t)
	plugin := filepath.Join(world.PluginsDir(), "acme-vault")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	casesExternalWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: acme-vault\n")
	casesExternalWrite(t, filepath.Join(plugin, "__init__.py"), loginProvenancePlugin)
	logs := loginCaptureLogs(t)
	if backend := loginProvenanceLoad(t, world); backend != nil {
		t.Fatalf("local directory plugin registered %v", backend)
	}
	if text := logs.String(); !strings.Contains(text, loginProvenanceRule) || !strings.Contains(text, "not installed from the Icebath plugin catalog") {
		t.Fatalf("refusal log %s", text)
	}
}

func TestUrlInstallWithForgedInTreeCatalogFileIsRefused(t *testing.T) {
	world := loginProvenanceWorld(t)
	forgedRecord, err := json.Marshal(map[string]any{"catalog_name": "acme-vault", "repo": "file://" + filepath.ToSlash(world.Repo()),
		"sha": world.Sha1(), "tier": "verified"})
	if err != nil {
		t.Fatal(err)
	}
	casesExternalWrite(t, filepath.Join(world.Repo(), ".icebath-catalog.json"), string(forgedRecord))
	forged := world.Commit(t, "forge provenance")
	if err := world.InstallPluginCore("file://"+filepath.ToSlash(world.Repo()), forged); err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	raw, err := os.ReadFile(filepath.Join(world.PluginsDir(), "acme-vault", ".icebath-catalog.json"))
	if err != nil || json.Unmarshal(raw, &tree) != nil || tree["tier"] != "verified" {
		t.Fatalf("forged in tree record %s %v", raw, err)
	}
	logs := loginCaptureLogs(t)
	if backend := loginProvenanceLoad(t, world); backend != nil {
		t.Fatalf("forged url install registered %v", backend)
	}
	if text := logs.String(); !strings.Contains(text, loginProvenanceRule) {
		t.Fatalf("refusal log %s", text)
	}
}

func TestVerifiedEntryInstalledOffTheReviewedPinIsRefused(t *testing.T) {
	world := loginProvenanceWorld(t)
	if err := world.InstallCatalogEntry(world.Sha2(), false); err != nil {
		t.Fatal(err)
	}
	logs := loginCaptureLogs(t)
	if backend := loginProvenanceLoad(t, world); backend != nil {
		t.Fatalf("off pin install registered %v", backend)
	}
	if text := logs.String(); !strings.Contains(text, loginProvenanceRule) || !strings.Contains(text, "reviewed pin") {
		t.Fatalf("refusal log %s", text)
	}
}

func TestVerifiedEntryInstalledWithAllowRemovedIsRefused(t *testing.T) {
	world := loginProvenanceWorld(t)
	if err := world.InstallCatalogEntry("", true); err != nil {
		t.Fatal(err)
	}
	logs := loginCaptureLogs(t)
	if backend := loginProvenanceLoad(t, world); backend != nil {
		t.Fatalf("allow removed install registered %v", backend)
	}
	if text := logs.String(); !strings.Contains(text, loginProvenanceRule) || !strings.Contains(text, "--allow-removed") {
		t.Fatalf("refusal log %s", text)
	}
}

const hostedLoginPlugin = `
import contextlib
from agent.vault_backends.base import LoginBackend
from agent.vault_store import VaultItemMeta

FILLS = FILLS_PATH


class Secret(str):
    pass


class Backend(LoginBackend):
    name, prefix, display_name = "acme", "acme:", "Acme"

    def list_items(self):
        return [VaultItemMeta(id="acme:one", kind="login", label="One", origin="https://a.example", created_at="t",
                              identifier_type="email", identifier="me@a.example", has_otp=True,
                              allowed_origins=("https://a.example", "https://b.example")),
                VaultItemMeta(id="other:two", kind="login", label="Two", origin=None, created_at="t")]

    def get_meta(self, handle):
        return VaultItemMeta(id=handle, kind="login", label="L", origin=None, created_at="t",
                             allowed_origins=["https://a.example"])

    def resolve_password(self, handle):
        return Secret("hunter2")

    def resolve_otp(self, handle):
        return 123456

    def resolve_secret(self, handle):
        return {"password": "p", "card_number": "4111"}

    @contextlib.contextmanager
    def fill_context(self):
        with open(FILLS, "a") as f:
            f.write("enter\n")
        yield
        with open(FILLS, "a") as f:
            f.write("exit\n")


def register(ctx):
    ctx.register_login_backend(Backend())
`

func TestHostedLoginBackendCopiesTypeChecksAndFills(t *testing.T) {
	fills := filepath.Join(t.TempDir(), "fills.log")
	world := plugins.NewLoginWorld(t, "acme-vault", strings.ReplaceAll(hostedLoginPlugin, "FILLS_PATH", strconv.Quote(fills)))
	world.SetTier("official")
	loginRegistryServices(t)
	if err := world.InstallCatalogEntry("", false); err != nil {
		t.Fatal(err)
	}
	if loginProvenanceLoad(t, world) == nil {
		t.Fatal("official install registered no backend")
	}
	enabled := browser.EnabledPluginLoginBackends(world.Ctx(), map[string]any{"acme": map[string]any{"enabled": true}})
	if len(enabled) != 1 {
		t.Fatalf("enabled plugin backends %v", enabled)
	}
	backend := enabled[0]
	items, err := backend.ListItems()
	if err != nil || len(items) != 1 || items[0].ID != "acme:one" || !items[0].HasOTP || items[0].Identifier != "me@a.example" ||
		items[0].IdentifierType != "email" || len(items[0].AllowedOrigins) != 2 || items[0].AllowedOrigins[1] != "https://b.example" {
		t.Fatalf("hosted list_items %+v %v", items, err)
	}
	invalid := "The acme login backend returned an invalid value."
	if meta, err := backend.GetMeta("acme:one"); err == nil || err.Error() != invalid {
		t.Fatalf("list allowed_origins %v %v", meta, err)
	}
	if password, err := backend.ResolvePassword("acme:one"); err != nil || password != "hunter2" {
		t.Fatalf("str subclass password %q %v", password, err)
	}
	if _, _, err := backend.ResolveOTP("acme:one"); err == nil || err.Error() != invalid {
		t.Fatalf("int otp %v", err)
	}
	if secret, err := backend.ResolveSecret("acme:one"); err != nil || secret["card_number"] != "4111" || secret["password"] != "p" {
		t.Fatalf("secret %v %v", secret, err)
	}
	if _, err := backend.ResolvePassword("other:one"); err == nil || !strings.Contains(err.Error(), "outside the acme login backend's namespace") {
		t.Fatalf("foreign handle %v", err)
	}
	leave, err := backend.FillContext()
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(fills); string(raw) != "enter\n" {
		t.Fatalf("fill entered %q", raw)
	}
	if err := leave(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(fills); string(raw) != "enter\nexit\n" {
		t.Fatalf("fill left %q", raw)
	}
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
