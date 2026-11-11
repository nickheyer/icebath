package plugins

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestUnloadDisposesInReverseOrderAndForgetsTargetedPlugin(t *testing.T) {
	m := NewManager(t.TempDir())
	a := newContext(&Manifest{Name: "alpha", Key: "cat/alpha"}, m)
	b := newContext(&Manifest{Name: "beta"}, m)
	var order []string
	for _, name := range []string{"one", "two", "three"} {
		name := name
		a.track("thing", name, func() error { order = append(order, name); return nil }, false)
	}
	b.track("thing", "b", func() error { order = append(order, "b"); return nil }, false)
	m.plugins["cat/alpha"] = &LoadedPlugin{Manifest: a.manifest, Enabled: true}
	if !m.Unload("alpha") {
		t.Fatal("unload by manifest name found nothing")
	}
	if !reflect.DeepEqual(order, []string{"three", "two", "one"}) {
		t.Fatal(order)
	}
	if _, ok := m.plugins["cat/alpha"]; ok || len(m.ownershipLedger["cat/alpha"]) != 0 {
		t.Fatal("alpha not forgotten")
	}
	if len(m.registrationOrder) != 1 || m.registrationOrder[0].Key != "b" {
		t.Fatal(m.registrationOrder)
	}
	if m.Unload("missing") {
		t.Fatal("unknown plugin reported found")
	}
}

func TestOnUnloadErrorDoesNotBlockOtherTeardown(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "p"}, m)
	ran := false
	c.track("unload", "ok", func() error { ran = true; return nil }, false)
	c.track("unload", "bad", func() error { return errors.New("boom") }, false)
	c.track("unload", "panics", func() error { panic("kaboom") }, false)
	m.Unload(nil)
	if !ran || len(m.registrationOrder) != 0 || len(m.ownershipLedger) != 0 {
		t.Fatal("teardown stopped at a failing release")
	}
}

func TestOnUnloadExceptionDoesNotBlockOtherTeardown(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "boom_probe", Version: "0.1", Description: "", Source: "user"}, m)
	var order []string
	for _, step := range []struct {
		name string
		err  error
	}{{"first", nil}, {"boom", &runtimeError{"cleanup failed"}}, {"last", nil}} {
		if _, err := c.OnUnload(func() error { order = append(order, step.name); return step.err }); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Unload("boom_probe") {
		t.Fatal("unload found nothing of the plugin to dispose")
	}
	if !slices.Equal(order, []string{"last", "boom", "first"}) {
		t.Fatalf("teardown order %v", order)
	}
	if _, kept := m.ownershipLedger["boom_probe"]; kept {
		t.Fatal("the ownership ledger kept the unloaded plugin")
	}
}

func TestPersistentRegistrationsSurviveUnloadAllAndEvictWhenNotReRegistered(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "auth"}, m)
	released := map[string]int{}
	keep := c.track("dashboard_auth", "keep", func() error { released["keep"]++; return nil }, true)
	c.track("dashboard_auth", "drop", func() error { released["drop"]++; return nil }, true)
	m.Unload(nil)
	if len(released) != 0 || len(m.persistentCarryover) != 2 {
		t.Fatalf("persistent disposed by unload-all %v", released)
	}
	m.mu.Lock()
	m.ownershipLedger["auth"] = []*Registration{keep}
	m.mu.Unlock()
	m.evictStalePersistentRegistrations()
	if released["drop"] != 1 || released["keep"] != 0 || len(m.persistentCarryover) != 0 {
		t.Fatalf("eviction %v", released)
	}
	m.Unload("auth")
	if released["keep"] != 1 {
		t.Fatal("targeted unload kept a persistent registration")
	}
}

func TestReplacementLeasesRestoreNearestLivePredecessor(t *testing.T) {
	coord := &ReplacementCoordinator{active: map[any][]*ReplacementLease{}}
	slot := map[string]any{"k": "base"}
	set := func(cur any) func(any) error {
		return func(replacement any) error {
			if slot["k"] == cur {
				slot["k"] = replacement
			}
			return nil
		}
	}
	type provider struct{ name string }
	p1, p2, p3 := &provider{"one"}, &provider{"two"}, &provider{"three"}
	slot["k"] = p1
	l1 := coord.Acquire("slot", p1, "base", set(p1), nil)
	slot["k"] = p2
	l2 := coord.Acquire("slot", p2, p1, set(p2), nil)
	slot["k"] = p3
	l3 := coord.Acquire("slot", p3, p2, set(p3), nil)
	_ = l2.Dispose()
	if slot["k"] != p3 {
		t.Fatal("non-latest disposal changed the slot")
	}
	_ = l3.Dispose()
	if slot["k"] != p1 {
		t.Fatalf("restored %v, want the live predecessor", slot["k"])
	}
	finalized := false
	l1.finalize = func() error { finalized = true; return nil }
	_ = l1.Dispose()
	if slot["k"] != "base" || !finalized || len(coord.active) != 0 {
		t.Fatal(slot["k"], finalized, coord.active)
	}
	if !sameRegistration([]any{p1, "x"}, []any{p1, "x"}) || sameRegistration(p1, p2) {
		t.Fatal("identity comparison")
	}
}

func TestFallbackHookIsInertWhenGeneralDiscoveryOwnsTheSource(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	c := newContext(&Manifest{Name: "mem"}, m)
	module := dir + "/__init__.py"
	m.plugins["mem"] = &LoadedPlugin{Manifest: c.manifest, Module: "icebath_plugins.mem", ModuleFile: module, Enabled: true}
	src := hookSourceOf("mem", module)
	h := m.registerFallbackHook(c, src, "pre_llm_call", func(context.Context, map[string]any) (any, error) { return nil, nil }, false)
	if m.HasHook("pre_llm_call") || h.Kind != "hook" {
		t.Fatal("owned source registered a live hook")
	}
	m.plugins["mem"].Enabled = false
	m.registerFallbackHook(c, src, "pre_llm_call", func(context.Context, map[string]any) (any, error) { return nil, nil }, false)
	if !m.HasHook("pre_llm_call") {
		t.Fatal("unowned source did not register")
	}
	m.dropFallbackHooks(src)
	if m.HasHook("pre_llm_call") || len(m.memoryHookRegistrations) != 0 {
		t.Fatal("fallback hooks not dropped")
	}
}

func TestHoldStreamOnlyOnTransformHook(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "p"}, m)
	c.RegisterHook("pre_llm_call", func(context.Context, map[string]any) (any, error) { return nil, nil }, true)
	if m.HoldsStream() {
		t.Fatal("hold_stream honoured on the wrong hook")
	}
	h := c.RegisterHook(StreamHoldingHook, func(context.Context, map[string]any) (any, error) { return nil, nil }, true)
	if !m.HoldsStream() {
		t.Fatal("hold_stream ignored")
	}
	_ = h.Dispose()
	if m.HoldsStream() || len(m.hooks[StreamHoldingHook]) != 0 {
		t.Fatal("disposed hook still held")
	}
}

func TestExportedLedgerViewsAndEvictionPass(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "auth"}, m)
	released := 0
	plain := c.track("thing", "plain", func() error { return nil }, false)
	persistent := c.track("dashboard_auth", "drop", func() error { released++; return nil }, true)
	if order := m.RegistrationOrder(); len(order) != 1 || order[0] != plain {
		t.Fatalf("registration order %v", order)
	}
	if owned := m.OwnedRegistrations("auth"); len(owned) != 2 || owned[1] != persistent {
		t.Fatalf("owned %v", owned)
	}
	m.Unload(nil)
	if parked := m.PersistentCarryover(); len(parked) != 1 || parked[0] != persistent || released != 0 {
		t.Fatalf("parked %v released %d", parked, released)
	}
	m.EvictStalePersistentRegistrations()
	if released != 1 || len(m.PersistentCarryover()) != 0 || len(m.OwnedRegistrations("auth")) != 0 {
		t.Fatalf("eviction released %d", released)
	}
}
