package plugins_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Compiled-in platform plugin whose register publishes an entry the cancelled load must never publish
type cancelProbePlugin struct{ name string }

func (p cancelProbePlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: p.name + "-platform", Kind: "platform"}
}

func (p cancelProbePlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{Name: p.name, Label: "Should not publish",
		AdapterFactory: func(any) any { return struct{}{} }, Check: func() bool { return true }})
	return err
}

// Gateway platform registry service signalling when the gateway starts running a leased deferred loader
type inflightSignal struct {
	plugins.PlatformRegistry
	started chan struct{}
	once    sync.Once
}

func (s *inflightSignal) RegisterDeferred(name string, loader *plugins.DeferredPlatformLoader, scope string) {
	run := loader.Load
	loader.Load = func(ctx context.Context) {
		s.once.Do(func() { close(s.started) })
		run(ctx)
	}
	s.PlatformRegistry.RegisterDeferred(name, loader, scope)
}

func TestUnloadCancelsADeferredPlatformBeforeModuleLoad(t *testing.T) {
	name := "ledger_cancel"
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	scope := gateway.CurrentScopeKey(ctx)
	registry := gateway.Registry
	previousEntry, previousLoader := registry.SnapshotRegistration(name, scope)
	t.Cleanup(func() {
		entry, loader := registry.SnapshotRegistration(name, scope)
		registry.RestoreRegistration(name, entry, loader, previousEntry, previousLoader, scope)
	})
	registry.RegisterDeferred(name, &plugins.DeferredPlatformLoader{Load: func(ctx context.Context) {
		if err := registry.Register(ctx, &gateway.PlatformEntry{Name: name, Label: "Restored predecessor",
			AdapterFactory: func(context.Context, *gateway.PlatformConfig) (any, error) { return struct{}{}, nil },
			CheckFn:        func() bool { return true }, Source: "plugin"}, scope); err != nil {
			t.Errorf("predecessor registration: %v", err)
		}
	}}, scope)
	signal := &inflightSignal{PlatformRegistry: gateway.PlatformRegistryService(), started: make(chan struct{})}
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.Platforms = signal
	plugins.InstallServices(&next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
	t.Cleanup(plugins.SwapBundled(cancelProbePlugin{name}))
	manager := plugins.NewManager(h)
	manifest := plugins.BundledManifestOf(name + "-platform")
	if manifest == nil {
		t.Fatal("the probe platform plugin is not bundled")
	}
	if err := plugins.RegisterDeferredPlatformOf(ctx, manager, manifest); err != nil {
		t.Fatal(err)
	}
	lookup := make(chan *gateway.PlatformEntry, 1)
	plugins.UnderDiscoveryLock(manager, func(unload func(plugin any) bool) {
		go func() { lookup <- registry.Get(ctx, name) }()
		select {
		case <-signal.started:
		case <-time.After(time.Second):
			t.Fatal("deferred loader never became in-flight")
		}
		if !unload(manifest) {
			t.Fatal("unload found nothing of the plugin to dispose")
		}
	})
	var restored *gateway.PlatformEntry
	select {
	case restored = <-lookup:
	case <-time.After(time.Second):
		t.Fatal("the lookup did not finish after the unload")
	}
	if restored == nil || restored.Label != "Restored predecessor" {
		t.Fatalf("lookup resolved %+v", restored)
	}
	if entry, _ := registry.SnapshotRegistration(name, scope); entry != restored {
		t.Fatalf("registered entry %+v is not the restored predecessor", entry)
	}
	if plugins.OwnsPlatformName(manager, name) {
		t.Fatal("the manager still lists the platform as plugin owned")
	}
}
