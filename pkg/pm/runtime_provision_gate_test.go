package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Fresh install tree, returning the tree and the relay environment it keeps in its install state
func provisionTree(t *testing.T) (root, venv string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root = t.TempDir()
	t.Cleanup(PatchRepoRoot(root))
	return root, RelayVenv(context.Background())
}

// Toolchain lookup counting its calls, failing with cause when cause is not empty
func provisionToolchain(t *testing.T, cause string) *int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	tools := t.TempDir()
	prev := SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if cause != "" {
			return "", "", false, errors.New(cause)
		}
		return filepath.Join(tools, "uv"), filepath.Join(tools, "python3"), true, nil
	})
	t.Cleanup(func() { SwapToolchain(prev) })
	return &calls
}

// Version the runtime set pins for nemo-relay
func pinnedRelayVersion(t *testing.T) string {
	t.Helper()
	reqs, err := RuntimeRequirements()
	if err != nil || len(reqs) == 0 {
		t.Fatalf("requirements %v %v", reqs, err)
	}
	return reqs[0].Version
}

// Lays out an environment whose interpreter exists and whose site-packages holds nemo_relay at version when not empty
func layRuntimeVenv(venv, version string) error {
	if err := os.MkdirAll(filepath.Dir(VenvPython(venv)), 0o777); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("version_info = 3.14.9\n"), 0o666); err != nil {
		return err
	}
	if err := os.WriteFile(VenvPython(venv), nil, 0o755); err != nil {
		return err
	}
	if version == "" {
		return nil
	}
	site, err := SitePackages(venv)
	if err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(site, "nemo_relay-"+version+".dist-info"), 0o777)
}

func writeRuntimeVenv(t *testing.T, venv, version string) {
	t.Helper()
	if err := layRuntimeVenv(venv, version); err != nil {
		t.Fatal(err)
	}
}

// uv commands the environment ran, venv laying out the interpreter and pip installing the pinned set
func provisionUV(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	version := pinnedRelayVersion(t)
	peSwap(t, &uvRunHook, func(e *PythonEnvironment, _ context.Context, args []string, _ string, _ time.Duration) (uvResult, error) {
		mu.Lock()
		calls = append(calls, append([]string{}, args...))
		mu.Unlock()
		var err error
		switch args[0] {
		case "venv":
			err = layRuntimeVenv(e.Destination, "")
		case "pip":
			err = layRuntimeVenv(e.Destination, version)
		}
		return uvResult{}, err
	})
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string{}, calls...)
	}
}

// Holds the provisioning lock of the store beside venv the way another process does until release runs
func holdProvisionLock(t *testing.T, venv string) (release func()) {
	t.Helper()
	store := RuntimeSetStore(venv)
	if err := os.MkdirAll(store, 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(store, ".provision.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := LockFD(f, false, 0); err != nil || !held {
		t.Fatalf("lock held %v err %v", held, err)
	}
	return func() { f.Close() }
}

func TestRuntimeEnvironmentReadyNeedsThePinnedSetInTheRelayEnvironment(t *testing.T) {
	_, venv := provisionTree(t)
	ctx, version := context.Background(), pinnedRelayVersion(t)
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("empty tree ready %v err %v", ready, err)
	}
	writeRuntimeVenv(t, venv, "0.1.0")
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("another version in the relay environment ready %v err %v", ready, err)
	}
	writeRuntimeVenv(t, venv, version)
	if got, ready, err := RuntimeEnvironmentReady(ctx); err != nil || !ready || got != venv {
		t.Fatalf("pinned set %s ready %v err %v", got, ready, err)
	}
	if err := os.MkdirAll(RuntimeSetStore(venv), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := newProvisionGate(venv).begin(); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("unfinished provisioning ready %v err %v", ready, err)
	}
}

func TestProvisioningInstallsThePinnedSetAndClearsItsMarker(t *testing.T) {
	_, venv := provisionTree(t)
	provisionToolchain(t, "")
	calls := provisionUV(t)
	got, err := EnsureRuntimeEnvironment(context.Background(), RuntimeEnvironmentOptions{})
	if err != nil || got != venv {
		t.Fatalf("provisioned %s %v", got, err)
	}
	if c := calls(); len(c) != 2 || c[0][0] != "venv" || c[1][0] != "pip" || containsString(c[1], "--reinstall") {
		t.Fatalf("uv calls %v", c)
	}
	if _, ready, err := RuntimeEnvironmentReady(context.Background()); !ready || err != nil {
		t.Fatalf("ready %v %v", ready, err)
	}
	if newProvisionGate(venv).unfinished() {
		t.Fatal("unfinished marker left behind")
	}
}

func TestProvisioningAfterAnUnfinishedRunReinstallsTheSet(t *testing.T) {
	_, venv := provisionTree(t)
	writeRuntimeVenv(t, venv, pinnedRelayVersion(t))
	gate := newProvisionGate(venv)
	if err := os.MkdirAll(gate.store, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate.unfinishedPath(), []byte("2147483646\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	provisionToolchain(t, "")
	calls := provisionUV(t)
	if _, err := EnsureRuntimeEnvironment(context.Background(), RuntimeEnvironmentOptions{}); err != nil {
		t.Fatal(err)
	}
	if c := calls(); len(c) != 1 || c[0][0] != "pip" || !containsString(c[0], "--reinstall") {
		t.Fatalf("uv calls %v", c)
	}
	if gate.unfinished() {
		t.Fatal("unfinished marker left behind")
	}
}

func TestProvisioningWaitsForTheLockThenReportsItsFailure(t *testing.T) {
	_, venv := provisionTree(t)
	lookups := provisionToolchain(t, "download failed")
	release := holdProvisionLock(t, venv)
	done := make(chan error, 1)
	go func() {
		_, err := EnsureRuntimeEnvironment(context.Background(), RuntimeEnvironmentOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned %v while the lock was held", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err == nil || err.Error() != "download failed" || *lookups != 1 {
			t.Fatalf("attempt %v after %d lookups", err, *lookups)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after the lock was released")
	}
}
