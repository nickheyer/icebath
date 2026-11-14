package plugins_test

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
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
