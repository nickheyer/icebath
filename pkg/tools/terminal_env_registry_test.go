package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// The testbox provider of tests/agent/test_terminal_env_registry.py
type registryTestProvider struct {
	TerminalEnvironmentProviderDefaults
	name          string
	available     bool
	cacheBase     func() string
	stripEnvKeys  func() []string
	sessionIsolat bool
}

func newRegistryTestProvider(name string) *registryTestProvider {
	p := &registryTestProvider{name: name, available: true, sessionIsolat: true,
		cacheBase:    func() string { return "~/.icebath" },
		stripEnvKeys: func() []string { return []string{"TESTBOX_TOKEN", "TESTBOX_SECRET"} }}
	p.Self = p
	return p
}

func (p *registryTestProvider) Name() string                           { return p.name }
func (p *registryTestProvider) DisplayName() string                    { return "TestBox" }
func (p *registryTestProvider) IsAvailable() bool                      { return p.available }
func (p *registryTestProvider) CachePathBase() string                  { return p.cacheBase() }
func (p *registryTestProvider) StripEnvKeys() []string                 { return p.stripEnvKeys() }
func (p *registryTestProvider) SessionIsolatedWhenNonpersistent() bool { return p.sessionIsolat }
func (p *registryTestProvider) CreateEnvironment(context.Context, *Env, CreateOptions) (Environment, error) {
	return nil, nil
}

// Every case of tests/agent/test_terminal_env_registry.py over the plugin terminal backend registry
func TestTerminalEnvRegistry(t *testing.T) {
	ctx := context.Background()
	clean := func(t *testing.T) {
		ResetTerminalEnvRegistryForTests()
		t.Cleanup(ResetTerminalEnvRegistryForTests)
	}
	register := func(t *testing.T, p BackendProvider, scope string) {
		if err := RegisterTerminalBackendScoped(p, scope); err != nil {
			t.Fatal(err)
		}
	}
	get := func(name string) BackendProvider { return terminalEnvRegistry.GetProvider(ctx, name, "") }

	t.Run("test_register_and_get", func(t *testing.T) {
		clean(t)
		p := newRegistryTestProvider("testbox")
		register(t, p, "")
		if get("testbox") != p || get("TESTBOX") != p {
			t.Fatal("lookup is case insensitive and returns the same instance")
		}
		if names := PluginBackendNames(ctx, ""); !reflect.DeepEqual(names, []string{"testbox"}) {
			t.Fatalf("names %v", names)
		}
	})
	t.Run("test_rejects_non_provider", func(t *testing.T) {
		clean(t)
		_, err := TerminalEnvRegistryScoped().Register(struct{}{}, "")
		var typeErr *providerregistry.ProviderTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("test_rejects_empty_name", func(t *testing.T) {
		clean(t)
		var valueErr *providerregistry.ValueError
		if err := RegisterTerminalBackend(newRegistryTestProvider("  ")); !errors.As(err, &valueErr) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("test_rejects_builtin_names", func(t *testing.T) {
		clean(t)
		for _, reserved := range BuiltinTerminalBackends() {
			var valueErr *providerregistry.ValueError
			if err := RegisterTerminalBackend(newRegistryTestProvider(reserved)); !errors.As(err, &valueErr) {
				t.Fatalf("%s: err %v", reserved, err)
			}
			if get(reserved) != nil {
				t.Fatalf("%s registered as a plugin backend", reserved)
			}
		}
	})
	t.Run("test_reregistration_overwrites", func(t *testing.T) {
		clean(t)
		p1, p2 := newRegistryTestProvider("testbox"), newRegistryTestProvider("testbox")
		register(t, p1, "")
		register(t, p2, "")
		if get("testbox") != p2 {
			t.Fatal("second registration wins")
		}
	})
	t.Run("test_scoped_registration_isolated", func(t *testing.T) {
		clean(t)
		pGlobal, pScoped := newRegistryTestProvider("testbox"), newRegistryTestProvider("testbox")
		register(t, pGlobal, "")
		register(t, pScoped, "profile-a")
		if terminalEnvRegistry.GetProvider(ctx, "testbox", "profile-a") != pScoped {
			t.Fatal("scope a reads its own registration")
		}
		if terminalEnvRegistry.GetProvider(ctx, "testbox", "profile-b") != pGlobal {
			t.Fatal("another scope falls back to global")
		}
	})
	t.Run("test_provider_flag_reads_attributes", func(t *testing.T) {
		clean(t)
		register(t, newRegistryTestProvider("testbox"), "")
		for attr, want := range map[string]any{"is_remote": true, "is_container": true, "session_isolated_when_nonpersistent": true,
			"cache_path_base": "~/.icebath", "skip_container_guards": true} {
			if got := TerminalProviderFlag(ctx, "testbox", attr, nil, ""); got != want {
				t.Fatalf("%s = %v want %v", attr, got, want)
			}
		}
	})
	t.Run("test_provider_flag_unknown_backend_returns_default", func(t *testing.T) {
		clean(t)
		if TerminalProviderFlag(ctx, "missing", "is_remote", false, "") != false {
			t.Fatal("bool default")
		}
		if TerminalProviderFlag(ctx, "missing", "cache_path_base", nil, "") != nil {
			t.Fatal("nil default")
		}
	})
	t.Run("test_provider_flag_fail_soft_on_raising_property", func(t *testing.T) {
		clean(t)
		broken := newRegistryTestProvider("broken")
		broken.cacheBase = func() string { panic("boom") }
		register(t, broken, "")
		if TerminalProviderFlag(ctx, "broken", "cache_path_base", nil, "") != nil {
			t.Fatal("a raising property reads as the default")
		}
	})
	t.Run("test_plugin_strip_env_keys_union", func(t *testing.T) {
		clean(t)
		other := newRegistryTestProvider("other")
		other.stripEnvKeys = func() []string { return []string{"OTHER_KEY"} }
		register(t, newRegistryTestProvider("testbox"), "")
		register(t, other, "")
		want := map[string]bool{"TESTBOX_TOKEN": true, "TESTBOX_SECRET": true, "OTHER_KEY": true}
		if got := PluginStripEnvKeys(); !reflect.DeepEqual(got, want) {
			t.Fatalf("keys %v", got)
		}
	})
	t.Run("test_plugin_strip_env_keys_fail_soft", func(t *testing.T) {
		clean(t)
		broken := newRegistryTestProvider("broken")
		broken.stripEnvKeys = func() []string { panic("boom") }
		register(t, broken, "")
		if got := PluginStripEnvKeys(); len(got) != 0 {
			t.Fatalf("keys %v", got)
		}
	})
	t.Run("test_restore_registration_unregisters", func(t *testing.T) {
		clean(t)
		p := newRegistryTestProvider("testbox")
		register(t, p, "")
		if !RestoreTerminalBackend("testbox", p, nil) || get("testbox") != nil {
			t.Fatal("restore to nil empties the slot")
		}
	})
	t.Run("test_restore_registration_noop_when_replaced", func(t *testing.T) {
		clean(t)
		p1, p2 := newRegistryTestProvider("testbox"), newRegistryTestProvider("testbox")
		register(t, p1, "")
		register(t, p2, "")
		if RestoreTerminalBackend("testbox", p1, nil) || get("testbox") != p2 {
			t.Fatal("a replaced current never restores")
		}
	})
	t.Run("test_restore_registration_restores_previous", func(t *testing.T) {
		clean(t)
		p1, p2 := newRegistryTestProvider("testbox"), newRegistryTestProvider("testbox")
		register(t, p1, "")
		register(t, p2, "")
		if !RestoreTerminalBackend("testbox", p2, p1) || get("testbox") != p1 {
			t.Fatal("previous comes back")
		}
	})
	t.Run("test_abc_defaults", func(t *testing.T) {
		p := newRegistryTestProvider("testbox")
		if !p.SkipContainerGuards() {
			t.Fatal("skip_container_guards defaults to is_container")
		}
		if !strings.Contains(p.EnvDescription(), "TestBox") {
			t.Fatalf("env description %q", p.EnvDescription())
		}
		if status, detail := p.Probe(); status != "ready" || detail != "" {
			t.Fatalf("probe %q %q", status, detail)
		}
		if lines := p.SetupInstructions(nil); len(lines) != 0 {
			t.Fatalf("setup instructions %v", lines)
		}
		if rows := p.DoctorChecks(); len(rows) == 0 || !rows[0].OK {
			t.Fatalf("doctor rows %v", rows)
		}
	})
	t.Run("test_probe_needs_setup_when_unavailable", func(t *testing.T) {
		off := newRegistryTestProvider("offbox")
		off.available = false
		status, detail := off.Probe()
		if status != "needs_setup" || detail == "" {
			t.Fatalf("probe %q %q", status, detail)
		}
	})
	t.Run("test_registry_generation_bumps", func(t *testing.T) {
		clean(t)
		g0 := TerminalEnvRegistryGeneration(ctx, "")
		register(t, newRegistryTestProvider("testbox"), "")
		if g1 := TerminalEnvRegistryGeneration(ctx, ""); g1 == g0 {
			t.Fatalf("generation unchanged %v", g1)
		}
	})
}
