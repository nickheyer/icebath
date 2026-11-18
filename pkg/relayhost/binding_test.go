package relayhost_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provisions the nemo_relay environment before the network is blocked, then dispatches a child or runs the suite
func TestMain(m *testing.M) {
	relaytest.ChildToolchain()
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

// Fresh install tree whose provisioning children find the pinned test toolchain and loopback index, or fail with a set cause
func freshRelayTree(t *testing.T, cause string) (root, venv string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root = t.TempDir()
	t.Cleanup(pm.PatchRepoRoot(root))
	relaytest.ChildUsesToolchain(t, cause)
	t.Setenv("UV_INDEX_URL", relaytest.ServeIndex(t))
	venv, err := pm.SelectedVenv(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, venv
}

func provisionLog(t *testing.T, venv string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pm.RuntimeSetStore(venv), "provision.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A load on a tree without an environment waits for the detached child that provisions the hash pinned set
func TestLoadWaitsForTheDetachedChildToProvisionTheRuntimeSet(t *testing.T) {
	_, venv := freshRelayTree(t, "")
	relay, err := relayhost.Load()
	if err != nil {
		t.Fatal(err)
	}
	if relay.LLM == nil || relay.LLM.Execute == nil || relay.Plugin == nil || relay.Plugin.Initialize == nil {
		t.Fatalf("binding surface %#v", relay)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	dist := "nemo_relay-0.11.0.dist-info"
	if _, err := os.Stat(filepath.Join(site, dist)); err != nil {
		t.Fatalf("%s missing from %s: %v", dist, site, err)
	}
	if log := provisionLog(t, venv); !strings.Contains(log, " provisioned\n") {
		t.Fatalf("provision log %q", log)
	}
	if _, ready, err := pm.RuntimeEnvironmentReady(context.Background()); err != nil || !ready {
		t.Fatalf("ready %v %v", ready, err)
	}
}

// A failed provisioning reaches the loader as the recorded failure, defers without a child, and an explicit install ends it
func TestLoadAfterAFailedProvisioningDefersUntilAnExplicitInstall(t *testing.T) {
	_, venv := freshRelayTree(t, "download failed")
	_, err := relayhost.Load()
	var unprovisioned *pm.RuntimeUnprovisioned
	var deferred *pm.ProvisionDeferred
	if !errors.As(err, &unprovisioned) || !errors.As(err, &deferred) || deferred.Cause != "download failed" || unprovisioned.Venv != venv {
		t.Fatalf("load over a failing toolchain %v", err)
	}
	record := filepath.Join(pm.RuntimeSetStore(venv), "provision-failure.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("failure record %v", err)
	}
	log := provisionLog(t, venv)
	if !strings.Contains(log, "failed: download failed") {
		t.Fatalf("provision log %q", log)
	}
	if _, err := relayhost.Load(); !errors.As(err, &deferred) || deferred.Cause != "download failed" {
		t.Fatalf("load inside the retry wait %v", err)
	}
	if again := provisionLog(t, venv); again != log {
		t.Fatalf("a child ran inside the retry wait %q", again)
	}
	uv, python := relaytest.Toolchain(t)
	prev := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapToolchain(prev) })
	if _, err := pm.EnsureRuntimeEnvironment(context.Background(), pm.RuntimeEnvironmentOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure record after the explicit install %v", err)
	}
	relay, err := relayhost.Load()
	if err != nil || relay.LLM == nil || relay.Plugin == nil {
		t.Fatalf("load after the explicit install %v %#v", err, relay)
	}
}

// Readiness answers at once on a tree without an environment, starts the background child, and turns true once it provisioned
func TestReadyStartsTheBackgroundProvisioningAndTurnsTrueOnceItFinished(t *testing.T) {
	_, venv := freshRelayTree(t, "")
	started := time.Now()
	if relayhost.Ready() {
		t.Fatal("ready without an environment")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("readiness took %s", took)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for !relayhost.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("background provisioning never finished, log %q", provisionLog(t, venv))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if log := provisionLog(t, venv); !strings.Contains(log, " provisioned\n") {
		t.Fatalf("provision log %q", log)
	}
	relay, err := relayhost.Load()
	if err != nil || relay.Plugin == nil {
		t.Fatalf("load after the background provisioning %v %#v", err, relay)
	}
}

// The provisioning entry refuses a command line without the environment it provisions
func TestProvisionEntryRefusesAMissingEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], relayhost.ProvisionEntry)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "usage: --venv <environment>") {
		t.Fatalf("exit %v output %q", err, out)
	}
}

// The update step provisions a tree that lacks the set in this process and leaves a ready tree alone
func TestProvisionRuntimeInstallsTheSetOnceAndSkipsAReadyEnvironment(t *testing.T) {
	freshRelayTree(t, "")
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
	freshRelayTree(t, "")
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
