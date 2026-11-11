package gateway

import (
	"context"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func ledgerPluginEntry(name, label string) *PlatformEntry {
	return &PlatformEntry{Name: name, Label: label, Source: "plugin", CheckFn: func() bool { return true },
		AdapterFactory: func(context.Context, *PlatformConfig) (any, error) { return struct{}{}, nil }}
}

func TestDeferredPlatformResolutionIsAtomicAcrossThreads(t *testing.T) {
	registry := NewPlatformRegistry()
	ctx := home.WithOverride(context.Background(), t.TempDir())
	entry := ledgerPluginEntry("threaded_platform", "Threaded")
	var loads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	registry.RegisterDeferred("threaded_platform", &plugins.DeferredPlatformLoader{Load: func(ctx context.Context) {
		loads.Add(1)
		close(started)
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			t.Error("the loader was never released")
		}
		if err := registry.Register(ctx, entry, ""); err != nil {
			t.Errorf("register: %v", err)
		}
	}}, "")
	lookup := func() chan *PlatformEntry {
		out := make(chan *PlatformEntry, 1)
		go func() { out <- registry.Get(ctx, "threaded_platform") }()
		return out
	}
	first := lookup()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the deferred loader never started")
	}
	if !registry.IsRegistered(ctx, "threaded_platform") {
		t.Fatal("an in-flight platform is not reported as registered")
	}
	second := lookup()
	select {
	case <-second:
		t.Fatal("concurrent lookup did not wait for materialization")
	case <-time.After(50 * time.Millisecond):
	}
	enumeration := make(chan []*PlatformEntry, 1)
	go func() { enumeration <- registry.AllEntries(ctx) }()
	select {
	case <-enumeration:
		t.Fatal("enumeration omitted an in-flight platform")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	var results []*PlatformEntry
	for _, pending := range []chan *PlatformEntry{first, second} {
		select {
		case got := <-pending:
			results = append(results, got)
		case <-time.After(time.Second):
			t.Fatal("a lookup did not finish after the loader was released")
		}
	}
	select {
	case all := <-enumeration:
		if !reflect.DeepEqual(all, []*PlatformEntry{entry}) {
			t.Fatalf("enumeration = %v", all)
		}
	case <-time.After(time.Second):
		t.Fatal("enumeration did not finish after the loader was released")
	}
	if results[0] != entry || results[1] != entry || loads.Load() != 1 {
		t.Fatalf("results %v loads %d", results, loads.Load())
	}
}

func TestDeferredPlatformRecursiveLookupDoesNotDeadlock(t *testing.T) {
	registry := NewPlatformRegistry()
	ctx := home.WithOverride(context.Background(), t.TempDir())
	entry := ledgerPluginEntry("recursive_platform", "Recursive")
	var nested []*PlatformEntry
	registry.RegisterDeferred("recursive_platform", &plugins.DeferredPlatformLoader{Load: func(ctx context.Context) {
		nested = append(nested, registry.Get(ctx, "recursive_platform"))
		if err := registry.Register(ctx, entry, ""); err != nil {
			t.Errorf("register: %v", err)
		}
	}}, "")
	if got := registry.Get(ctx, "recursive_platform"); got != entry {
		t.Fatalf("lookup resolved %+v", got)
	}
	if len(nested) != 1 || nested[0] != nil {
		t.Fatalf("nested lookups %v", nested)
	}
}

func TestDirectPluginPlatformRegistrationInfersImmutableScope(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	homeA, homeB := filepath.Join(tmp, "direct-a"), filepath.Join(tmp, "direct-b")
	entry := ledgerPluginEntry("ledger_direct_platform", "Direct A")
	module := callableModule(entry.AdapterFactory)
	policy := tools.Default.RegisterPluginOverridePolicy(module, false, homeA)
	name := entry.Name
	scopeA := home.Key(homeA)
	previousEntry, previousLoader := Registry.SnapshotRegistration(name, scopeA)
	globalEntry, globalLoader := Registry.SnapshotRegistration(name, "")
	t.Cleanup(func() {
		e, l := Registry.SnapshotRegistration(name, scopeA)
		Registry.RestoreRegistration(name, e, l, previousEntry, previousLoader, scopeA)
		if e, l := Registry.SnapshotRegistration(name, ""); e != globalEntry || l != globalLoader {
			Registry.RestoreRegistration(name, e, l, globalEntry, globalLoader, "")
		}
		tools.Default.RestorePluginOverridePolicy(module, policy, nil, homeA)
	})
	ctxA, ctxB := home.WithOverride(context.Background(), homeA), home.WithOverride(context.Background(), homeB)
	if err := Registry.Register(ctxB, entry, ""); err != nil {
		t.Fatal(err)
	}
	if got := Registry.Get(ctxB, name); got != nil {
		t.Fatalf("the direct registration leaked into another profile: %+v", got)
	}
	if got := Registry.Get(ctxA, name); got == nil || got.Label != "Direct A" {
		t.Fatalf("the owning profile resolved %+v", got)
	}
}
