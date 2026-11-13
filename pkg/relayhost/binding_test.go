package relayhost_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provisions the nemo_relay environment before the network is blocked, then dispatches the child or runs the suite
func TestMain(m *testing.M) {
	relayhost.DispatchRelayHostEntry(context.Background())
	relaytest.Provision()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Scopes, events and a managed LLM call run in the native library and reach a Go subscriber with their callbacks
func TestBindingDrivesNativeRelay(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	relaytest.Use(t)
	relay, err := tools.LoadNemoRelay()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []*tools.RelayEventView
	if err := relay.Subscribers.Register("test.binding", func(ev *tools.RelayEventView) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	defer relay.Subscribers.Deregister("test.binding")
	ctx := pyctx.With(context.Background(), pyctx.New())
	handle, err := relay.Scope.Push(ctx, "test.agent", relay.ScopeType.Agent, map[string]any{"metadata": map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := handle.(tools.RelayHandleWithUUID); !ok || u.UUID() == nil {
		t.Fatalf("handle %#v has no uuid", handle)
	}
	request, _ := relay.LLMRequest(map[string]any{}, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	var seen any
	callback := tools.RelayProviderCallback(func(_ context.Context, next any) (any, error) {
		seen = next
		return map[string]any{"id": "r", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}}}, nil
	})
	aw, err := relay.LLM.Execute(ctx, "test.llm", request, callback, map[string]any{"handle": handle, "model_name": "m"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.RelayRunAwaitable(ctx, aw, "")
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := seen.(*tools.RelayLLMRequest); !ok || r.Content.(map[string]any)["model"] != "m" {
		t.Fatalf("provider saw %#v", seen)
	}
	if m, _ := result.(map[string]any); m["id"] != "r" {
		t.Fatalf("result %#v", result)
	}
	failure := errors.New("provider down")
	failing := tools.RelayProviderCallback(func(context.Context, any) (any, error) { return nil, failure })
	aw, _ = relay.LLM.Execute(ctx, "test.llm", request, failing, map[string]any{"handle": handle})
	if _, err := tools.RelayRunAwaitable(ctx, aw, ""); err == nil || !(errors.Is(err, failure) || tools.IsRelayWrappedCallbackError(err, failure)) {
		t.Fatalf("callback failure surfaced as %v", err)
	}
	if _, err := relay.Scope.Event(ctx, "test.mark", map[string]any{"data": map[string]any{"n": 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Scope.Pop(ctx, handle, nil); err != nil {
		t.Fatal(err)
	}
	if err := relay.Subscribers.Flush(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	names := map[string]int{}
	for _, ev := range events {
		name, _ := ev.Name.(string)
		names[name]++
	}
	if names["test.agent"] != 2 || names["test.llm"] < 2 || names["test.mark"] != 1 {
		t.Fatalf("events %v", names)
	}
}

// A project without an environment gets the hash pinned runtime set from the index the first time Relay loads
func TestLoadProvisionsTheRuntimeSetOnFirstUse(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	t.Cleanup(pm.PatchRepoRoot(root))
	uv, python := relaytest.Toolchain(t)
	prev := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapToolchain(prev) })
	t.Setenv("UV_INDEX_URL", relaytest.ServeIndex(t))
	relay, err := relayhost.Load()
	if err != nil {
		t.Fatal(err)
	}
	if relay.LLM == nil || relay.LLM.Execute == nil || relay.Plugin == nil || relay.Plugin.Initialize == nil {
		t.Fatalf("binding surface %#v", relay)
	}
	venv, err := pm.SelectedVenv(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	dist := "nemo_relay-0.11.0.dist-info"
	if _, err := os.Stat(filepath.Join(site, dist)); err != nil {
		t.Fatalf("%s missing from %s: %v", dist, site, err)
	}
}
