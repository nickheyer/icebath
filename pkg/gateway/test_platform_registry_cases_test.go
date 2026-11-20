package gateway

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Adapter value a registry factory hands back, compared by identity
type registryCaseAdapter struct{ name string }

// Plugin entry of the registry cases with its factory, check and validation outcomes
func registryCaseEntry(name string, checkOK, validateOK, factoryOK bool) (*PlatformEntry, *registryCaseAdapter) {
	adapter := &registryCaseAdapter{name: name}
	entry := NewPlatformEntry(name, pyTitle(name), func(context.Context, *PlatformConfig) (any, error) {
		if !factoryOK {
			return nil, errors.New("factory error")
		}
		return adapter, nil
	}, func() bool { return checkOK })
	entry.ValidateConfig = func(*PlatformConfig) (bool, error) { return validateOK, nil }
	entry.Source = "plugin"
	return entry, adapter
}

// Title case of a lowercase ascii name, the label Python's str.title gives the cases
func pyTitle(name string) string {
	if name == "" {
		return name
	}
	return string(name[0]-32) + name[1:]
}

// test_platform_registry.py TestPlatformRegistry.test_unregister
func TestPlatformRegistryUnregister(t *testing.T) {
	ctx := context.Background()
	reg := NewPlatformRegistry()
	entry, _ := registryCaseEntry("beta", true, true, true)
	if err := reg.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	if removed, err := reg.Unregister(ctx, "beta", ""); err != nil || !removed {
		t.Fatalf("unregister = %v, %v", removed, err)
	}
	if reg.Get(ctx, "beta") != nil {
		t.Fatal("entry still registered")
	}
	if removed, err := reg.Unregister(ctx, "beta", ""); err != nil || removed {
		t.Fatalf("second unregister = %v, %v", removed, err)
	}
}

// test_platform_registry.py TestPlatformRegistry.test_create_adapter_no_validate
func TestCreateAdapterNoValidate(t *testing.T) {
	ctx := context.Background()
	reg := NewPlatformRegistry()
	adapter := &registryCaseAdapter{name: "novalidate"}
	entry := NewPlatformEntry("novalidate", "NoValidate", func(context.Context, *PlatformConfig) (any, error) { return adapter, nil },
		func() bool { return true })
	entry.Source = "plugin"
	if err := reg.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	if got := reg.CreateAdapter(ctx, "novalidate", NewPlatformConfig()); got != adapter {
		t.Fatalf("create_adapter = %#v", got)
	}
}

// test_platform_registry.py TestPlatformRegistry.test_registered_names_includes_deferred_without_materializing
func TestRegisteredNamesIncludesDeferredWithoutMaterializing(t *testing.T) {
	ctx := context.Background()
	reg := NewPlatformRegistry()
	entry, _ := registryCaseEntry("concrete", true, true, true)
	calls := 0
	loader := &plugins.DeferredPlatformLoader{Load: func(context.Context) { calls++ }}
	if err := reg.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	reg.RegisterDeferred("deferred", loader, "")
	names := slices.Sorted(maps.Keys(reg.RegisteredNames(ctx)))
	if !slices.Equal(names, []string{"concrete", "deferred"}) {
		t.Fatalf("registered_names = %v", names)
	}
	if calls != 0 {
		t.Fatalf("loader called %d times", calls)
	}
	if reg.Get(ctx, "concrete") != entry {
		t.Fatal("concrete entry lost")
	}
	if !reg.IsRegistered(ctx, "deferred") {
		t.Fatal("deferred name not registered")
	}
}

// Plugin entry of the ensure deps cases with a counting installer
func ensureDepsEntry(name string, check func() bool, installer func(context.Context) (bool, error)) (*PlatformEntry, *registryCaseAdapter) {
	adapter := &registryCaseAdapter{name: name}
	entry := NewPlatformEntry(name, pyTitle(name), func(context.Context, *PlatformConfig) (any, error) { return adapter, nil }, check)
	entry.EnsureDepsFn = installer
	entry.Source = "plugin"
	return entry, adapter
}

// test_platform_registry.py TestEnsureDepsFn, one row per case
func TestEnsureDepsFn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		check         func() bool
		install       func() (bool, error)
		wantAdapter   bool
		wantInstalled int
	}{
		{"test_deps_present_skips_installer", func() bool { return true }, func() (bool, error) { return true, nil }, true, 0},
		{"test_missing_deps_runs_installer_then_creates", func() bool { return false }, func() (bool, error) { return true, nil }, true, 1},
		{"test_install_failure_returns_none", func() bool { return false }, func() (bool, error) { return false, nil }, false, 1},
		{"test_no_installer_missing_deps_returns_none", func() bool { return false }, nil, false, 0},
		{"test_installer_exception_returns_none", func() bool { return false }, func() (bool, error) { panic("pip exploded") }, false, 1},
		{"test_check_fn_exception_falls_through_to_installer", func() bool { panic("probe error") }, func() (bool, error) { return true, nil }, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewPlatformRegistry()
			installed := 0
			var installer func(context.Context) (bool, error)
			if tc.install != nil {
				installer = func(context.Context) (bool, error) {
					installed++
					return tc.install()
				}
			}
			entry, adapter := ensureDepsEntry("plat", tc.check, installer)
			if err := reg.Register(ctx, entry, ""); err != nil {
				t.Fatal(err)
			}
			got := reg.CreateAdapter(ctx, "plat", NewPlatformConfig())
			if tc.wantAdapter && got != adapter {
				t.Fatalf("create_adapter = %#v, want the factory's adapter", got)
			}
			if !tc.wantAdapter && got != nil {
				t.Fatalf("create_adapter = %#v, want None", got)
			}
			if installed != tc.wantInstalled {
				t.Fatalf("installer called %d times, want %d", installed, tc.wantInstalled)
			}
		})
	}
}
