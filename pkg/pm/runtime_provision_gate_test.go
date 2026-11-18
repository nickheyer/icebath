package pm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fresh install tree with its own runtime only environment path, returning the tree and the venv it selects
func provisionTree(t *testing.T) (root, venv string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root = t.TempDir()
	t.Cleanup(PatchRepoRoot(root))
	venv, err := SelectedVenv(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, venv
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

// Runs the provisioning child body in this process for every launch, counting the launches
func provisionInProcess(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	launches := 0
	peSwap(t, &launchRuntimeProvisioner, func(venv string, handshake bool) (*provisionLaunch, error) {
		mu.Lock()
		launches++
		mu.Unlock()
		r, w := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- provisionDetached(context.Background(), venv, w, nil) }()
		launch := &provisionLaunch{wait: func() error { return <-done }}
		if handshake {
			launch.handshake = r
		} else {
			go io.Copy(io.Discard, r)
		}
		return launch, nil
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return launches
	}
}

// Moves the gate clock to start plus offset until the test ends
func provisionClockAt(t *testing.T, start time.Time, offset time.Duration) {
	t.Helper()
	prev := provisionNow
	provisionNow = func() time.Time { return start.Add(offset) }
	t.Cleanup(func() { provisionNow = prev })
}

func recordedProvisionFailure(t *testing.T, venv string) provisionFailure {
	t.Helper()
	var rec provisionFailure
	data, err := os.ReadFile(filepath.Join(RuntimeSetStore(venv), "provision-failure.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
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

// Waiting caller of the runtime environment, the non explicit EnsureRuntimeEnvironment
func awaitRuntime() (string, error) {
	return EnsureRuntimeEnvironment(context.Background(), RuntimeEnvironmentOptions{})
}

// Deferral a waiting caller's error carries, failing the test when it carries none
func unprovisionedDeferral(t *testing.T, err error) *ProvisionDeferred {
	t.Helper()
	var unprovisioned *RuntimeUnprovisioned
	var deferred *ProvisionDeferred
	if !errors.As(err, &unprovisioned) || !errors.As(err, &deferred) {
		t.Fatalf("error %v carries no deferral", err)
	}
	return deferred
}

func TestProvisionRetryWaitDoublesFromTheBaseToTheCap(t *testing.T) {
	for count, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 6: 16 * time.Minute, 7: 30 * time.Minute, 40: 30 * time.Minute} {
		if got := provisionRetryAfter(count); got != want {
			t.Fatalf("after %d failures wait %s, want %s", count, got, want)
		}
	}
}

func TestRuntimeEnvironmentReadyNeedsThePinnedSetInTheTreesOwnEnvironment(t *testing.T) {
	root, venv := provisionTree(t)
	ctx, version := context.Background(), pinnedRelayVersion(t)
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("empty tree ready %v err %v", ready, err)
	}
	writeRuntimeVenv(t, venv, "0.1.0")
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("another version in the runtime only environment ready %v err %v", ready, err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if got, ready, err := RuntimeEnvironmentReady(ctx); err != nil || !ready || got != venv {
		t.Fatalf("another version in a project environment %s ready %v err %v", got, ready, err)
	}
	if err := os.Remove(filepath.Join(root, "pyproject.toml")); err != nil {
		t.Fatal(err)
	}
	writeRuntimeVenv(t, venv, version)
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || !ready {
		t.Fatalf("pinned set ready %v err %v", ready, err)
	}
	if err := newProvisionGate(venv, nil).begin(); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := RuntimeEnvironmentReady(ctx); err != nil || ready {
		t.Fatalf("unfinished provisioning ready %v err %v", ready, err)
	}
}

func TestWaitingCallerStartsTheChildAndGetsItsFailureWithTheRetryWait(t *testing.T) {
	_, venv := provisionTree(t)
	lookups, launches := provisionToolchain(t, "download failed"), provisionInProcess(t)
	start := time.Unix(1_800_000_000, 0)
	provisionClockAt(t, start, 0)
	_, err := awaitRuntime()
	if deferred := unprovisionedDeferral(t, err); deferred.Cause != "download failed" || deferred.Retry != 30*time.Second {
		t.Fatalf("deferral %+v", deferred)
	}
	if *lookups != 1 || launches() != 1 {
		t.Fatalf("%d lookups %d launches", *lookups, launches())
	}
	if want := "the runtime environment " + venv + " is not provisioned: download failed (provisioning is tried again in 30s, run `icebath pm install` to provision at once)"; err.Error() != want {
		t.Fatalf("message %q", err)
	}
	if rec := recordedProvisionFailure(t, venv); rec.Count != 1 || rec.Error != "download failed" || rec.AtMillis != start.UnixMilli() {
		t.Fatalf("record %+v", rec)
	}
	log, err := os.ReadFile(filepath.Join(RuntimeSetStore(venv), "provision.log"))
	if err != nil || !strings.Contains(string(log), "failed: download failed") {
		t.Fatalf("log %q %v", log, err)
	}
	if !newProvisionGate(venv, nil).unfinished() {
		t.Fatal("the failed run left no unfinished marker")
	}
}

func TestWaitingCallerInsideTheRetryWaitGetsTheRecordedFailureWithoutAChild(t *testing.T) {
	_, venv := provisionTree(t)
	lookups, launches := provisionToolchain(t, "download failed"), provisionInProcess(t)
	start := time.Unix(1_800_000_000, 0)
	provisionClockAt(t, start, 0)
	if _, err := awaitRuntime(); err == nil {
		t.Fatal("first attempt succeeded")
	}
	provisionClockAt(t, start, 29*time.Second)
	_, err := awaitRuntime()
	if deferred := unprovisionedDeferral(t, err); deferred.Retry != time.Second || *lookups != 1 || launches() != 1 {
		t.Fatalf("inside the wait %+v after %d lookups %d launches", deferred, *lookups, launches())
	}
	provisionClockAt(t, start, 30*time.Second)
	if _, err := awaitRuntime(); unprovisionedDeferral(t, err).Retry != time.Minute || *lookups != 2 || launches() != 2 {
		t.Fatalf("after the wait %v after %d lookups %d launches", err, *lookups, launches())
	}
	if rec := recordedProvisionFailure(t, venv); rec.Count != 2 || rec.AtMillis != start.Add(30*time.Second).UnixMilli() {
		t.Fatalf("second record %+v", rec)
	}
	provisionClockAt(t, start, 89*time.Second)
	if _, err := awaitRuntime(); unprovisionedDeferral(t, err).Retry != time.Second || launches() != 2 {
		t.Fatalf("inside the doubled wait %v after %d launches", err, launches())
	}
	provisionClockAt(t, start, 20*time.Second)
	if _, err := awaitRuntime(); err == nil || *lookups != 3 || launches() != 3 {
		t.Fatalf("clock before the record %v after %d lookups %d launches", err, *lookups, launches())
	}
}

func TestWaitingCallerJoinsTheProvisioningAnotherProcessHolds(t *testing.T) {
	_, venv := provisionTree(t)
	lookups, launches := provisionToolchain(t, "download failed"), provisionInProcess(t)
	provisionClockAt(t, time.Unix(1_800_000_000, 0), 0)
	release := holdProvisionLock(t, venv)
	done := make(chan error, 1)
	go func() {
		_, err := awaitRuntime()
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned %v while the lock was held", err)
	case <-time.After(300 * time.Millisecond):
	}
	if launches() != 1 || *lookups != 0 {
		t.Fatalf("behind the lock %d launches %d lookups", launches(), *lookups)
	}
	release()
	select {
	case err := <-done:
		if deferred := unprovisionedDeferral(t, err); deferred.Cause != "download failed" || *lookups != 1 || launches() != 2 {
			t.Fatalf("after the release %+v %d lookups %d launches", deferred, *lookups, launches())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after the lock was released")
	}
}

func TestWaitingCallerGetsTheEnvironmentAnotherProcessProvisioned(t *testing.T) {
	_, venv := provisionTree(t)
	lookups, launches := provisionToolchain(t, "download failed"), provisionInProcess(t)
	release := holdProvisionLock(t, venv)
	done := make(chan error, 1)
	go func() {
		got, err := awaitRuntime()
		if err == nil && got != venv {
			err = errors.New("selected " + got)
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	writeRuntimeVenv(t, venv, pinnedRelayVersion(t))
	release()
	select {
	case err := <-done:
		if err != nil || *lookups != 0 || launches() != 1 {
			t.Fatalf("joined %v after %d lookups %d launches", err, *lookups, launches())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after the lock was released")
	}
}

func TestProvisioningInstallsThePinnedSetAndClearsItsMarkerAndRecord(t *testing.T) {
	_, venv := provisionTree(t)
	start := time.Unix(1_800_000_000, 0)
	provisionClockAt(t, start, time.Hour)
	gate := newProvisionGate(venv, mustRequirements(t))
	if err := os.MkdirAll(gate.store, 0o777); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(provisionFailure{Set: gate.set, AtMillis: start.UnixMilli(), Count: 1, Error: "old"})
	if err := os.WriteFile(gate.recordPath(), old, 0o666); err != nil {
		t.Fatal(err)
	}
	provisionToolchain(t, "")
	calls, launches := provisionUV(t), provisionInProcess(t)
	got, err := awaitRuntime()
	if err != nil || got != venv || launches() != 1 {
		t.Fatalf("provisioned %s %v after %d launches", got, err, launches())
	}
	if c := calls(); len(c) != 2 || c[0][0] != "venv" || c[1][0] != "pip" || containsString(c[1], "--reinstall") {
		t.Fatalf("uv calls %v", c)
	}
	if _, ready, err := RuntimeEnvironmentReady(context.Background()); !ready || err != nil {
		t.Fatalf("ready %v %v", ready, err)
	}
	if gate.unfinished() {
		t.Fatal("unfinished marker left behind")
	}
	if _, err := os.Stat(gate.recordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure record %v", err)
	}
	if got, err := awaitRuntime(); err != nil || got != venv || launches() != 1 {
		t.Fatalf("ready environment %s %v after %d launches", got, err, launches())
	}
}

func TestProvisioningAfterAnUnfinishedRunReinstallsTheSet(t *testing.T) {
	_, venv := provisionTree(t)
	writeRuntimeVenv(t, venv, pinnedRelayVersion(t))
	gate := newProvisionGate(venv, mustRequirements(t))
	if err := os.MkdirAll(gate.store, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate.unfinishedPath(), []byte("2147483646\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	provisionToolchain(t, "")
	calls, _ := provisionUV(t), provisionInProcess(t)
	if _, err := awaitRuntime(); err != nil {
		t.Fatal(err)
	}
	if c := calls(); len(c) != 1 || c[0][0] != "pip" || !containsString(c[0], "--reinstall") {
		t.Fatalf("uv calls %v", c)
	}
	if gate.unfinished() {
		t.Fatal("unfinished marker left behind")
	}
}

func TestWaitingCallerReportsAChildThatEndedWithoutAResult(t *testing.T) {
	_, venv := provisionTree(t)
	peSwap(t, &launchRuntimeProvisioner, func(string, bool) (*provisionLaunch, error) {
		return &provisionLaunch{handshake: io.NopCloser(strings.NewReader("held\n")), wait: func() error { return errors.New("exit status 2") }}, nil
	})
	_, err := awaitRuntime()
	var unprovisioned *RuntimeUnprovisioned
	if !errors.As(err, &unprovisioned) || unprovisioned.Venv != venv || !strings.Contains(err.Error(), "the provisioning child ended before recording a result: exit status 2") {
		t.Fatalf("error %v", err)
	}
	peSwap(t, &launchRuntimeProvisioner, func(string, bool) (*provisionLaunch, error) {
		return &provisionLaunch{handshake: io.NopCloser(strings.NewReader("")), wait: func() error { return nil }}, nil
	})
	if _, err := awaitRuntime(); err == nil || !strings.Contains(err.Error(), "exited before reporting whether it holds the store lock") {
		t.Fatalf("silent child %v", err)
	}
}

func TestWaitingCallerIsRefusedWhenLazyInstallsAreOff(t *testing.T) {
	provisionTree(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	launches := provisionInProcess(t)
	_, err := awaitRuntime()
	var install *InstallError
	if !errors.As(err, &install) || launches() != 0 || !strings.Contains(err.Error(), "lazy installs are disabled") {
		t.Fatalf("refusal %v after %d launches", err, launches())
	}
}

func TestStartLaunchesTheChildOnlyWhileTheEnvironmentNeedsOne(t *testing.T) {
	_, venv := provisionTree(t)
	provisionToolchain(t, "")
	provisionUV(t)
	launches := provisionInProcess(t)
	gate := newProvisionGate(venv, mustRequirements(t))
	if err := os.MkdirAll(gate.store, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate.unfinishedPath(), []byte("2147483646\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	release := holdProvisionLock(t, venv)
	if err := StartRuntimeEnvironment(context.Background()); err != nil || launches() != 0 {
		t.Fatalf("running provisioning %v %d launches", err, launches())
	}
	release()
	if err := os.Remove(gate.unfinishedPath()); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_800_000_000, 0)
	provisionClockAt(t, start, 0)
	if cause := errors.New("download failed"); gate.failed(cause) != cause || StartRuntimeEnvironment(context.Background()) != nil || launches() != 0 {
		t.Fatalf("inside the retry wait %d launches", launches())
	}
	provisionClockAt(t, start, time.Hour)
	t.Setenv(EnvDisableLazyInstalls, "1")
	if err := StartRuntimeEnvironment(context.Background()); err != nil || launches() != 0 {
		t.Fatalf("lazy installs off %v %d launches", err, launches())
	}
	t.Setenv(EnvDisableLazyInstalls, "")
	if err := StartRuntimeEnvironment(context.Background()); err != nil || launches() != 1 {
		t.Fatalf("missing environment %v %d launches", err, launches())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ready, _ := RuntimeEnvironmentReady(context.Background()); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the started child never provisioned the environment")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.awaitRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := StartRuntimeEnvironment(context.Background()); err != nil || launches() != 1 {
		t.Fatalf("ready environment %v %d launches", err, launches())
	}
}

func TestStartLeavesTheLiveCheckoutUnderTestAlone(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(PatchRepoRoot(resolvePath(binaryTree())))
	launches := provisionInProcess(t)
	if err := StartRuntimeEnvironment(context.Background()); err != nil || launches() != 0 {
		t.Fatalf("live checkout %v %d launches", err, launches())
	}
}

func TestChildReportsBusyAndLeavesTheStoreToTheProcessHoldingIt(t *testing.T) {
	_, venv := provisionTree(t)
	lookups := provisionToolchain(t, "download failed")
	release := holdProvisionLock(t, venv)
	defer release()
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- provisionDetached(context.Background(), venv, w, nil) }()
	held, err := readProvisionHandshake(r)
	if err != nil || held || <-done != nil || *lookups != 0 {
		t.Fatalf("held %v err %v after %d lookups", held, err, *lookups)
	}
	if _, err := os.Stat(filepath.Join(RuntimeSetStore(venv), "provision.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy child wrote a log %v", err)
	}
}

func TestExplicitProvisioningWaitsForTheLockAndIgnoresTheRetryWait(t *testing.T) {
	_, venv := provisionTree(t)
	lookups := provisionToolchain(t, "download failed")
	provisionClockAt(t, time.Unix(1_800_000_000, 0), 0)
	release := holdProvisionLock(t, venv)
	if cause := errors.New("download failed"); newProvisionGate(venv, mustRequirements(t)).failed(cause) != cause {
		t.Fatal("no failure recorded")
	}
	done := make(chan error, 1)
	go func() {
		_, err := EnsureRuntimeEnvironment(context.Background(), RuntimeEnvironmentOptions{Explicit: true})
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
		var deferred *ProvisionDeferred
		if err == nil || errors.As(err, &deferred) || err.Error() != "download failed" || *lookups != 1 {
			t.Fatalf("explicit attempt %v after %d lookups", err, *lookups)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after the lock was released")
	}
	if rec := recordedProvisionFailure(t, venv); rec.Count != 1 {
		t.Fatalf("record %+v", rec)
	}
}

func TestRecordedFailureOfAnotherRequirementSetDoesNotDefer(t *testing.T) {
	_, venv := provisionTree(t)
	lookups, launches := provisionToolchain(t, "download failed"), provisionInProcess(t)
	provisionClockAt(t, time.Unix(1_800_000_000, 0), 0)
	store := RuntimeSetStore(venv)
	if err := os.MkdirAll(store, 0o777); err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(provisionFailure{Set: "another set", AtMillis: provisionNow().UnixMilli(), Count: 5, Error: "old"})
	if err := os.WriteFile(filepath.Join(store, "provision-failure.json"), other, 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := awaitRuntime()
	if unprovisionedDeferral(t, err).Cause != "download failed" || *lookups != 1 || launches() != 1 {
		t.Fatalf("attempt %v after %d lookups %d launches", err, *lookups, launches())
	}
	if rec := recordedProvisionFailure(t, venv); rec.Count != 1 || rec.Set == "another set" {
		t.Fatalf("record %+v", rec)
	}
}

func mustRequirements(t *testing.T) []byte {
	t.Helper()
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		t.Fatal(err)
	}
	return requirements
}

func TestChildThatCannotTakeTheLockTellsTheWaitingParentWhy(t *testing.T) {
	_, venv := provisionTree(t)
	if err := os.WriteFile(RuntimeSetStore(venv), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	launches := provisionInProcess(t)
	_, err := awaitRuntime()
	var unprovisioned *RuntimeUnprovisioned
	if !errors.As(err, &unprovisioned) || !strings.Contains(unprovisioned.Err.Error(), "not a directory") || launches() != 1 {
		t.Fatalf("error %v after %d launches", err, launches())
	}
	if err := StartRuntimeEnvironment(context.Background()); err == nil || !strings.Contains(err.Error(), "not a directory") || launches() != 1 {
		t.Fatalf("start %v after %d launches", err, launches())
	}
}
