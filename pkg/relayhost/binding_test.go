//go:build !icebath_core && !icebath_noextras

package relayhost_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
	ctxutil "github.com/nickheyer/icebath/pkg/utils/ctx"
)

// Provisions the nemo_relay environment before the network is blocked, then dispatches a child or runs the suite
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
	ctx := ctxutil.WithStore(context.Background(), ctxutil.NewStore())
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

// Fresh install tree whose relay environment the test index provisions, returning the tree and that environment
func freshRelayTree(t *testing.T) (root, venv string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root = t.TempDir()
	t.Cleanup(pm.PatchRepoRoot(root))
	t.Setenv("UV_INDEX_URL", relaytest.ServeIndex(t))
	return root, pm.RelayVenv(context.Background())
}

// A load never provisions, reporting the unprovisioned relay environment until an explicit install provisions it
func TestLoadReportsAnUnprovisionedRuntimeUntilAnExplicitInstall(t *testing.T) {
	_, venv := freshRelayTree(t)
	_, err := relayhost.Load()
	var unprovisioned *pm.RuntimeUnprovisioned
	if !errors.As(err, &unprovisioned) || unprovisioned.Venv != venv || !strings.Contains(err.Error(), "icebath pm install --extra nemo-relay") {
		t.Fatalf("load without a relay environment %v", err)
	}
	if relayhost.Ready() {
		t.Fatal("ready without a relay environment")
	}
	if _, err := os.Stat(venv); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a load created the relay environment %v", err)
	}
	uv, python := relaytest.Toolchain(t)
	prev := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapToolchain(prev) })
	if _, err := pm.EnsureRuntimeEnvironment(context.Background(), pm.RuntimeEnvironmentOptions{}); err != nil {
		t.Fatal(err)
	}
	if !relayhost.Ready() {
		t.Fatal("not ready after the explicit install")
	}
	relay, err := relayhost.Load()
	if err != nil || relay.LLM == nil || relay.Plugin == nil {
		t.Fatalf("load after the explicit install %v %#v", err, relay)
	}
}

// The switch on step provisions a tree that lacks the set in this process and leaves a ready tree alone
func TestProvisionRuntimeInstallsTheSetOnceAndSkipsAReadyEnvironment(t *testing.T) {
	freshRelayTree(t)
	uv, python := relaytest.Toolchain(t)
	prev := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapToolchain(prev) })
	ctx := context.Background()
	var out bytes.Buffer
	if err := relayhost.ProvisionRuntime(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "→ Provisioning the NeMo Relay runtime...") || !strings.Contains(text, "✓ NeMo Relay runtime provisioned") {
		t.Fatalf("output %q", text)
	}
	if _, ready, err := pm.RuntimeEnvironmentReady(ctx); err != nil || !ready {
		t.Fatalf("ready %v %v", ready, err)
	}
	out.Reset()
	if err := relayhost.ProvisionRuntime(ctx, &out); err != nil || out.Len() != 0 {
		t.Fatalf("ready environment %v output %q", err, out.String())
	}
}

// A failed provisioning step returns its cause after the progress line
func TestProvisionRuntimeReturnsTheFailureItsInstallMet(t *testing.T) {
	freshRelayTree(t)
	prev := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) {
		return "", "", false, errors.New("download failed")
	})
	t.Cleanup(func() { pm.SwapToolchain(prev) })
	var out bytes.Buffer
	err := relayhost.ProvisionRuntime(context.Background(), &out)
	if err == nil || err.Error() != "NeMo Relay runtime was not provisioned: download failed" ||
		out.String() != "→ Provisioning the NeMo Relay runtime...\n" {
		t.Fatalf("error %v output %q", err, out.String())
	}
}
