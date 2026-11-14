package delegationdefault

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

type recordingRegistry struct {
	mu    sync.Mutex
	slots map[string]any
}

func (r *recordingRegistry) Accepts(p any) bool {
	_, ok := p.(tools.DelegationProvider)
	return ok
}
func (r *recordingRegistry) ProviderName(p any) string { return p.(tools.DelegationProvider).Name() }
func (r *recordingRegistry) Identity(any) error        { return nil }
func (r *recordingRegistry) Snapshot(name, _ string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slots[name]
}
func (r *recordingRegistry) Register(p any, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots[p.(tools.DelegationProvider).Name()] = p
	return true, nil
}
func (r *recordingRegistry) Restore(name string, _, replacement any, _ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if replacement == nil {
		delete(r.slots, name)
	} else {
		r.slots[name] = replacement
	}
	return true
}

// Loads every bundled plugin into a fresh home with the given config and returns the delegation registry
func loadHome(t *testing.T, config string) (*plugins.Manager, *recordingRegistry) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if config != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := &recordingRegistry{slots: map[string]any{}}
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"delegation_provider": reg}})
	t.Cleanup(func() { plugins.InstallServices(nil) })
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(context.Background(), true)
	return m, reg
}

func TestProviderIdentityAndInheritedDefaults(t *testing.T) {
	p := NewDefaultDelegationProvider()
	if p.Name() != "default" || p.DisplayName() != "Default" {
		t.Fatal(p.Name(), p.DisplayName())
	}
	if p.Summary() != "Every subagent uses delegation.model / delegation.provider, else the parent's model." {
		t.Fatal(p.Summary())
	}
	if !p.IsAvailable() || len(p.TaskParameters(map[string]any{"model": "m"})) != 0 || p.Description(nil) != "" {
		t.Fatal("inherited defaults changed")
	}
	want := map[string]any{"name": "Default", "badge": "", "tag": "", "env_vars": []any{}}
	if got := p.GetSetupSchema(); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	cfg := map[string]any{"backends": map[string]any{"default": map[string]any{"k": 1}, "other": 2}}
	if got := p.Settings(cfg); !reflect.DeepEqual(got, map[string]any{"k": 1}) {
		t.Fatal(got)
	}
	if got := p.Settings(map[string]any{"backends": map[string]any{"default": "x"}}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestRouteTaskIsTheDelegationSectionWithoutServiceTier(t *testing.T) {
	cfg := map[string]any{"model": "pinned-model", "provider": "openrouter", "base_url": "", "reasoning_effort": "high", "service_tier": "fast"}
	route, err := NewDefaultDelegationProvider().RouteTask(map[string]any{"goal": "one plain delegated task"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": "pinned-model", "provider": "openrouter", "base_url": "", "reasoning_effort": "high"}
	if !reflect.DeepEqual(route, want) {
		t.Fatal(route)
	}
	route["model"] = "changed"
	if cfg["model"] != "pinned-model" || cfg["service_tier"] != "fast" {
		t.Fatal("route aliases the config section", cfg)
	}
	empty, _ := NewDefaultDelegationProvider().RouteTask(nil, map[string]any{})
	if len(empty) != 0 {
		t.Fatal(empty)
	}
}

func TestDefaultStrategyIsTheBundledBackendPlugin(t *testing.T) {
	m, reg := loadHome(t, "")
	found := false
	for _, r := range m.ListPlugins() {
		if r["key"] == "delegation/default" {
			found = true
			if r["source"] != "bundled" || r["enabled"] != true {
				t.Fatalf("%v", r)
			}
		}
	}
	if !found {
		t.Fatal("delegation/default not discovered")
	}
	p, ok := reg.Snapshot("default", "").(*DefaultDelegationProvider)
	if !ok || p.Name() != "default" {
		t.Fatalf("%#v", reg.slots)
	}
}

func TestDisablingTheBundledDefaultLeavesNoStrategy(t *testing.T) {
	m, reg := loadHome(t, "plugins:\n  disabled:\n  - delegation/default\n")
	if len(reg.slots) != 0 {
		t.Fatalf("%#v", reg.slots)
	}
	for _, r := range m.ListPlugins() {
		if r["key"] == "delegation/default" && r["enabled"] != false {
			t.Fatalf("%v", r)
		}
	}
}
