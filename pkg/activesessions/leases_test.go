package activesessions

import (
	"context"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Registry file of a home
func statePathOf(h string) string {
	statePath, _ := leasePaths(context.Background(), h)
	return statePath
}

// Ages every lease in the given registries past the self orphan grace
func backdateLeases(t *testing.T, homes ...string) {
	t.Helper()
	for _, h := range homes {
		statePath := statePathOf(h)
		entries, err := readEntries(statePath, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			entry.Set("started_at", nowSeconds()-600.0)
		}
		if err := writeEntries(statePath, entries); err != nil {
			t.Fatal(err)
		}
	}
}

// Lease ids of entries in order
func leaseIDs(entries []*pyjson.Object) []string {
	ids := []string{}
	for _, e := range entries {
		ids = append(ids, leaseIDOf(e))
	}
	return ids
}

// Session ids of entries in order
func sessionIDs(entries []*pyjson.Object) []string {
	ids := []string{}
	for _, e := range entries {
		ids = append(ids, e.GetOr("session_id", nil).(string))
	}
	return ids
}

// Fails unless err is a registry error
func requireRegistryError(t *testing.T, err error, match string) {
	t.Helper()
	var regErr *RegistryError
	if !errors.As(err, &regErr) {
		t.Fatalf("want RegistryError, got %v", err)
	}
	if match != "" && !strings.Contains(err.Error(), match) {
		t.Fatalf("error %q does not match %q", err, match)
	}
}

// Text of a file, failing the test when unreadable
func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Acquires a lease that must succeed
func mustAcquire(t *testing.T, sessionID, surface string, cfg map[string]any, registryHome string, track bool) *Lease {
	t.Helper()
	lease, refusal, err := TryAcquire(context.Background(), sessionID, surface, cfg, nil, registryHome, track)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("acquire %s: lease=%v refusal=%v err=%v", sessionID, lease, refusal, err)
	}
	return lease
}

// Snapshot of the active registry that must be readable
func mustSnapshot(t *testing.T, registryHome string) []*pyjson.Object {
	t.Helper()
	entries, err := RegistrySnapshot(context.Background(), registryHome, false)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// Guard of the session that must succeed, returning whether it is leased
func mustGuard(t *testing.T, sessionID, registryHome string, own map[string]bool) bool {
	t.Helper()
	var active bool
	if err := LivenessGuard(context.Background(), sessionID, registryHome, own, func(a bool) error { active = a; return nil }); err != nil {
		t.Fatal(err)
	}
	return active
}

func TestResolveMaxConcurrentSessionsValues(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want int64
	}{
		{map[string]any{}, 0},
		{map[string]any{"max_concurrent_sessions": nil}, 0},
		{map[string]any{"max_concurrent_sessions": 0}, 0},
		{map[string]any{"max_concurrent_sessions": -1}, 0},
		{map[string]any{"max_concurrent_sessions": "3"}, 3},
		{map[string]any{"gateway": map[string]any{"max_concurrent_sessions": 4}}, 4},
		{map[string]any{"max_concurrent_sessions": 2, "gateway": map[string]any{"max_concurrent_sessions": 4}}, 2},
		{map[string]any{"max_concurrent_sessions": "many"}, 0},
	}
	for _, c := range cases {
		got := ResolveMaxConcurrentSessions(c.cfg)
		if c.want == 0 {
			if got != nil {
				t.Fatalf("%v: want None, got %v", c.cfg, got)
			}
			continue
		}
		if got == nil || got.Cmp(big.NewInt(c.want)) != 0 {
			t.Fatalf("%v: want %d, got %v", c.cfg, c.want, got)
		}
	}
}

const crossProcessWorkerEnv = "ICEBATH_ACTIVE_SESSIONS_WORKER"

// Child side of the cross process acquire race, one worker per process
func TestHelperActiveSessionWorker(t *testing.T) {
	if os.Getenv(crossProcessWorkerEnv) != "1" {
		return
	}
	idx := os.Getenv("WORKER_INDEX")
	workerCount, _ := strconv.Atoi(os.Getenv("WORKER_COUNT"))
	readyDir, resultsDir, goFile := os.Getenv("READY_DIR"), os.Getenv("RESULTS_DIR"), os.Getenv("GO_FILE")
	fail := func(msg string) { os.Stderr.WriteString(msg + "\n"); os.Exit(1) }
	if err := os.WriteFile(filepath.Join(readyDir, idx), []byte("ready"), 0o644); err != nil {
		fail(err.Error())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(goFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fail("timed out waiting for go file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if idx == os.Getenv("DELAYED_WORKER_INDEX") {
		time.Sleep(2500 * time.Millisecond)
	}
	lease, _, err := TryAcquire(context.Background(), "process-"+idx, "cli", map[string]any{"max_concurrent_sessions": 1}, nil, "", false)
	if err != nil {
		fail(err.Error())
	}
	if lease == nil {
		_ = os.WriteFile(filepath.Join(resultsDir, idx), []byte("BLOCK"), 0o644)
		os.Stdout.WriteString("BLOCK\n")
		os.Exit(0)
	}
	_ = os.WriteFile(filepath.Join(resultsDir, idx), []byte("OK"), 0o644)
	os.Stdout.WriteString("OK\n")
	deadline = time.Now().Add(10 * time.Second)
	for {
		done, _ := os.ReadDir(resultsDir)
		if len(done) >= workerCount {
			break
		}
		if time.Now().After(deadline) {
			fail("timed out waiting for all workers to attempt acquire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lease.Release(); err != nil {
		fail(err.Error())
	}
	os.Exit(0)
}

func TestCrossProcessAcquireClaimsOnlyOneLastSlot(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	t.Setenv(home.EnvHome, h)
	readyDir, resultsDir, goFile := filepath.Join(tmp, "ready"), filepath.Join(tmp, "results"), filepath.Join(tmp, "go")
	for _, d := range []string{readyDir, resultsDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	type worker struct {
		cmd            *exec.Cmd
		stdout, stderr strings.Builder
	}
	var workers []*worker
	defer func() {
		for _, w := range workers {
			if w.cmd.ProcessState == nil {
				_ = w.cmd.Process.Kill()
				_ = w.cmd.Wait()
			}
		}
	}()
	for index := range 6 {
		w := &worker{cmd: exec.Command(os.Args[0], "-test.run=^TestHelperActiveSessionWorker$")}
		w.cmd.Env = append(os.Environ(), crossProcessWorkerEnv+"=1", home.EnvHome+"="+h,
			"WORKER_INDEX="+strconv.Itoa(index), "WORKER_COUNT=6", "DELAYED_WORKER_INDEX=5",
			"READY_DIR="+readyDir, "RESULTS_DIR="+resultsDir, "GO_FILE="+goFile)
		w.cmd.Stdout, w.cmd.Stderr = &w.stdout, &w.stderr
		if err := w.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready, _ := os.ReadDir(readyDir)
		if len(ready) >= len(workers) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workers did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(goFile, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	var outputs []string
	for _, w := range workers {
		done := make(chan error, 1)
		go func() { done <- w.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("worker failed: %v %s", err, w.stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("worker timed out")
		}
		outputs = append(outputs, strings.TrimSpace(w.stdout.String()))
	}
	ok, block := 0, 0
	for _, out := range outputs {
		switch out {
		case "OK":
			ok++
		case "BLOCK":
			block++
		}
	}
	if ok != 1 || block != len(workers)-1 {
		t.Fatalf("outputs %v", outputs)
	}
	if got := mustSnapshot(t, ""); len(got) != 0 {
		t.Fatalf("snapshot %v", got)
	}
}

func TestReleaseOrphanedLeasesReclaimsOnlyUnownedOwnPidEntries(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	cfg := map[string]any{"max_concurrent_sessions": 5}
	kept := mustAcquire(t, "kept", "desktop", cfg, "", false)
	orphan := mustAcquire(t, "orphaned", "desktop", cfg, "", false)
	entries, err := readEntries(statePathOf(""), false)
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, pyjson.ObjectOf("lease_id", "elsewhere", "session_id", "other", "surface", "cli", "pid", int64(os.Getpid())))
	if err := writeEntries(statePathOf(""), entries); err != nil {
		t.Fatal(err)
	}
	backdateLeases(t, h)
	dropped, err := ReleaseOrphanedLeases(map[string]bool{kept.LeaseID: true, "elsewhere": true})
	if err != nil || dropped != 1 {
		t.Fatalf("dropped %d err %v", dropped, err)
	}
	got := sessionIDs(mustSnapshot(t, ""))
	slices.Sort(got)
	if !slices.Equal(got, []string{"kept", "other"}) {
		t.Fatalf("snapshot sessions %v", got)
	}
	if orphan == nil {
		t.Fatal("orphan lease missing")
	}
}

// Root home with one named profile carrying its identity marker
func profileTree(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "hermes")
	profile := filepath.Join(root, "profiles", "worker")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, root)
	return root, profile
}

func TestReleaseOrphanedLeasesSweepsProfileRuntimeRegistries(t *testing.T) {
	root, profile := profileTree(t)
	mustAcquire(t, "root-orphan", "desktop", map[string]any{}, root, false)
	mustAcquire(t, "profile-orphan", "desktop", map[string]any{}, profile, false)
	if n, err := ReleaseOrphanedLeases(map[string]bool{}); err != nil || n != 0 {
		t.Fatalf("fresh leases reaped: %d %v", n, err)
	}
	backdateLeases(t, root, profile)
	if n, err := ReleaseOrphanedLeases(map[string]bool{}); err != nil || n != 2 {
		t.Fatalf("dropped %d err %v", n, err)
	}
	if got := mustSnapshot(t, root); len(got) != 0 {
		t.Fatalf("root %v", got)
	}
	if got := mustSnapshot(t, profile); len(got) != 0 {
		t.Fatalf("profile %v", got)
	}
}

func TestDropSelfOrphansSparesForeignAndVouchedLeases(t *testing.T) {
	own := int64(os.Getpid())
	entries := []*pyjson.Object{
		pyjson.ObjectOf("lease_id", "orphan", "pid", own),
		pyjson.ObjectOf("lease_id", "live", "pid", own),
		pyjson.ObjectOf("lease_id", "foreign", "pid", own+1),
	}
	if got := dropSelfOrphans(entries, nil); !slices.Equal(got, entries) {
		t.Fatalf("nil vouch dropped entries: %v", leaseIDs(got))
	}
	if got := dropSelfOrphans(entries, map[string]bool{"live": true}); !slices.Equal(got, entries[1:]) {
		t.Fatalf("got %v", leaseIDs(got))
	}
}

func TestReleaseUnderProfileHomeOverrideTargetsAcquisitionRegistry(t *testing.T) {
	root, profile := profileTree(t)
	lease := mustAcquire(t, "agent:worker:telegram:dm:synthetic", "gateway:telegram", map[string]any{"max_concurrent_sessions": 2}, "", false)
	rootRegistry := filepath.Join(root, "runtime", "active_sessions.json")
	if _, err := os.Stat(rootRegistry); err != nil {
		t.Fatal(err)
	}
	scoped := home.WithOverride(context.Background(), profile)
	if got, _ := leasePaths(scoped, ""); got != filepath.Join(profile, "runtime", "active_sessions.json") {
		t.Fatalf("override not in effect: %s", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !lease.Released() {
		t.Fatal("lease not released")
	}
	remaining, err := readEntries(rootRegistry, false)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining %v %v", remaining, err)
	}
	if _, err := os.Stat(filepath.Join(profile, "runtime", "active_sessions.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("phantom profile registry: %v", err)
	}
}

func TestTransferUnderProfileHomeOverrideTargetsAcquisitionRegistry(t *testing.T) {
	root, profile := profileTree(t)
	lease := mustAcquire(t, "before", "gateway:telegram", map[string]any{"max_concurrent_sessions": 2}, "", false)
	scoped := home.WithOverride(context.Background(), profile)
	if got, _ := leasePaths(scoped, ""); got != filepath.Join(profile, "runtime", "active_sessions.json") {
		t.Fatalf("override not in effect: %s", got)
	}
	if ok, err := TransferActiveSession(lease, "after", nil); !ok || err != nil {
		t.Fatalf("transfer %v %v", ok, err)
	}
	entries, err := readEntries(filepath.Join(root, "runtime", "active_sessions.json"), false)
	if err != nil || !slices.Equal(sessionIDs(entries), []string{"after"}) {
		t.Fatalf("entries %v %v", sessionIDs(entries), err)
	}
}

func TestLivenessRegistryCorruptionFailsClosedWithoutOverwrite(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	statePath := filepath.Join(h, "runtime", "active_sessions.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := "{not-json"
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	requireRegistryError(t, LivenessGuard(context.Background(), "session-1", "", nil, func(bool) error { return nil }), "")
	_, err := RegistrySnapshot(context.Background(), "", false)
	requireRegistryError(t, err, "")
	if readText(t, statePath) != corrupt {
		t.Fatal("registry overwritten")
	}
	_, _, err = TryAcquire(context.Background(), "desktop-1", "desktop", map[string]any{}, nil, "", true)
	requireRegistryError(t, err, "")
	if readText(t, statePath) != corrupt {
		t.Fatal("registry overwritten")
	}
	lease, message, err := TryAcquire(context.Background(), "cli-1", "cli", map[string]any{"max_concurrent_sessions": 1}, nil, "", false)
	if err != nil || lease != nil {
		t.Fatalf("lease %v err %v", lease, err)
	}
	if message == nil || message.Reason != SessionCoordinationUnavailable {
		t.Fatalf("refusal %v", message)
	}
	if readText(t, statePath) != corrupt {
		t.Fatal("registry overwritten")
	}
}

func TestStrictRegistryRejectsStructurallyInvalidEntries(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	statePath := filepath.Join(h, "runtime", "active_sessions.json")
	base := func() *pyjson.Object {
		return pyjson.ObjectOf("lease_id", "lease-1", "session_id", "session-1", "surface", "desktop", "pid", int64(os.Getpid()), "track_liveness", true)
	}
	without := func(key string) *pyjson.Object { o := base(); o.Delete(key); return o }
	with := func(key string, v any) *pyjson.Object { o := base(); o.Set(key, v); return o }
	invalid := []*pyjson.Object{
		without("lease_id"),
		with("lease_id", ""),
		without("session_id"),
		with("session_id", "  "),
		with("pid", int64(0)),
		with("pid", 1.5),
		with("surface", int64(1)),
		with("track_liveness", "yes"),
		with("metadata", []any{}),
		with("process_start_time", "not-a-number"),
		with("process_start_time", "nan"),
	}
	for _, entry := range invalid {
		if err := writeEntries(statePath, []*pyjson.Object{entry}); err != nil {
			t.Fatal(err)
		}
		original := readText(t, statePath)
		requireRegistryError(t, LivenessGuard(context.Background(), "session-1", "", nil, func(bool) error { return nil }), "")
		if readText(t, statePath) != original {
			t.Fatalf("registry rewritten for %s", original)
		}
	}
}

func TestStrictRegistryRejectsDuplicateLeaseIds(t *testing.T) {
	for _, tc := range []struct{ id, second string }{{"exact-duplicate", "session-a"}, {"conflicting-duplicate", "session-b"}} {
		t.Run(tc.id, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), ".icebath")
			t.Setenv(home.EnvHome, h)
			statePath := filepath.Join(h, "runtime", "active_sessions.json")
			pid := int64(os.Getpid())
			if err := writeEntries(statePath, []*pyjson.Object{
				pyjson.ObjectOf("lease_id", "duplicate-lease", "session_id", "session-a", "surface", "desktop", "pid", pid, "track_liveness", true),
				pyjson.ObjectOf("lease_id", "duplicate-lease", "session_id", tc.second, "surface", "desktop", "pid", pid, "track_liveness", true),
			}); err != nil {
				t.Fatal(err)
			}
			original := readText(t, statePath)
			_, err := RegistrySnapshot(context.Background(), "", false)
			requireRegistryError(t, err, "")
			if readText(t, statePath) != original {
				t.Fatal("registry rewritten")
			}
		})
	}
}

func TestCapTransferDoesNotOverwriteRegistryCorruption(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	statePath := filepath.Join(h, "runtime", "active_sessions.json")
	lease := mustAcquire(t, "cli-old", "cli", map[string]any{"max_concurrent_sessions": 1}, "", false)
	corrupt := "{not-json"
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := TransferActiveSession(lease, "cli-new", nil); ok || err != nil {
		t.Fatalf("transfer %v %v", ok, err)
	}
	if lease.SessionID() != "cli-old" || readText(t, statePath) != corrupt {
		t.Fatal("transfer changed the lease or the registry")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !lease.Released() || readText(t, statePath) != corrupt {
		t.Fatal("release did not settle without overwriting")
	}
}

// Swaps a package seam for the duration of the test
func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

func TestLivenessGuardRejectsUnknownPidState(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	statePath := filepath.Join(h, "runtime", "active_sessions.json")
	if err := writeEntries(statePath, []*pyjson.Object{
		pyjson.ObjectOf("lease_id", "unknown-owner", "session_id", "session-1", "surface", "desktop", "pid", int64(12345), "track_liveness", true),
	}); err != nil {
		t.Fatal(err)
	}
	swap(t, &pidExists, func(int) (bool, error) { return false, errors.New("pid lookup unavailable") })
	requireRegistryError(t, LivenessGuard(context.Background(), "session-1", "", nil, func(bool) error { return nil }), "")
	lease, message, err := TryAcquire(context.Background(), "cli-cap-session", "cli", map[string]any{"max_concurrent_sessions": 1}, nil, "", false)
	if err != nil || lease != nil || message == nil || message.Reason != MaxConcurrentSessions {
		t.Fatalf("lease %v refusal %v err %v", lease, message, err)
	}
	if !strings.Contains(readText(t, statePath), "unknown-owner") {
		t.Fatal("unknown owner dropped")
	}
	lease, message, err = TryAcquire(context.Background(), "cli-cap-session", "cli", map[string]any{}, nil, "", false)
	if err != nil || lease == nil || message != nil {
		t.Fatalf("lease %v refusal %v err %v", lease, message, err)
	}
	if !strings.Contains(readText(t, statePath), "unknown-owner") {
		t.Fatal("unknown owner dropped")
	}
}

// Registry holding one liveness tracked sibling on pid 1 whose start time cannot be read
func unknownSiblingRegistry(t *testing.T) (string, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	statePath := filepath.Join(h, "runtime", "active_sessions.json")
	if err := writeEntries(statePath, []*pyjson.Object{
		pyjson.ObjectOf("lease_id", "stale-sibling", "session_id", "old-chat", "surface", "desktop", "pid", int64(1), "process_start_time", 1234.5, "track_liveness", true),
	}); err != nil {
		t.Fatal(err)
	}
	original := processStartTime
	swap(t, &processStartTime, func(pid int) (float64, bool) {
		if pid == 1 {
			return 0, false
		}
		return original(pid)
	})
	return h, statePath
}

func TestUnknownSiblingLivenessOnlyFencesItsOwnSession(t *testing.T) {
	_, statePath := unknownSiblingRegistry(t)
	if got := pidLiveness(int64(1), 1234.5, false); got != livenessUnknown {
		t.Fatalf("liveness %d", got)
	}
	_, _, err := TryAcquire(context.Background(), "old-chat", "desktop", map[string]any{}, nil, "", true)
	requireRegistryError(t, err, "liveness is unknown")
	lease := mustAcquire(t, "new-chat", "desktop", map[string]any{}, "", true)
	if ok, err := TransferActiveSession(lease, "new-chat-rotated", nil); !ok || err != nil {
		t.Fatalf("transfer %v %v", ok, err)
	}
	if err := lease.Release(); err != nil || !lease.Released() {
		t.Fatalf("release %v", err)
	}
	remaining, err := readEntries(statePath, false)
	if err != nil || !slices.Equal(leaseIDs(remaining), []string{"stale-sibling"}) {
		t.Fatalf("remaining %v %v", leaseIDs(remaining), err)
	}
}

func TestUnknownSiblingDoesNotBlockGuardedReleaseOrOrphanSweep(t *testing.T) {
	h, statePath := unknownSiblingRegistry(t)
	lease := mustAcquire(t, "new-chat", "desktop", map[string]any{}, "", true)
	var active bool
	if err := ReleaseLivenessGuard(context.Background(), lease, "new-chat", map[string]bool{}, func(a bool) error { active = a; return nil }); err != nil {
		t.Fatal(err)
	}
	if active || !lease.Released() {
		t.Fatalf("active %v released %v", active, lease.Released())
	}
	if mustGuard(t, "new-chat", "", nil) {
		t.Fatal("released session still leased")
	}
	remaining, _ := readEntries(statePath, false)
	if !slices.Equal(leaseIDs(remaining), []string{"stale-sibling"}) {
		t.Fatalf("remaining %v", leaseIDs(remaining))
	}
	mustAcquire(t, "orphaned", "desktop", map[string]any{}, "", true)
	backdateLeases(t, h)
	if n, err := releaseOrphanedLeasesInHome(h, map[string]bool{}); n != 1 || err != nil {
		t.Fatalf("dropped %d err %v", n, err)
	}
	remaining, _ = readEntries(statePath, false)
	if !slices.Equal(leaseIDs(remaining), []string{"stale-sibling"}) {
		t.Fatalf("remaining %v", leaseIDs(remaining))
	}
}

// Write seam that fails every registry rewrite
func failingWrite(string, []*pyjson.Object) error { return errors.New("replace failed") }

func TestLivenessReleaseFailureIsRetryable(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), ".icebath"))
	lease := mustAcquire(t, "session-1", "desktop", map[string]any{}, "", true)
	original := writeEntries
	writeEntries = failingWrite
	err := lease.Release()
	writeEntries = original
	if err == nil || !strings.Contains(err.Error(), "replace failed") {
		t.Fatalf("err %v", err)
	}
	if lease.Released() {
		t.Fatal("failed release marked released")
	}
	if err := lease.Release(); err != nil || !lease.Released() {
		t.Fatalf("retry %v", err)
	}
	if got := mustSnapshot(t, ""); len(got) != 0 {
		t.Fatalf("snapshot %v", got)
	}
}

func TestLivenessTransferUpsertsMissingEntryWithoutConsumingANewSlot(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	cfg := map[string]any{"max_concurrent_sessions": 1}
	lease := mustAcquire(t, "session-old", "desktop", cfg, "", true)
	if err := os.Remove(filepath.Join(h, "runtime", "active_sessions.json")); err != nil {
		t.Fatal(err)
	}
	if ok, err := TransferActiveSession(lease, "session-new", nil); !ok || err != nil {
		t.Fatalf("transfer %v %v", ok, err)
	}
	snapshot := mustSnapshot(t, "")
	if len(snapshot) != 1 || leaseIDOf(snapshot[0]) != lease.LeaseID || snapshot[0].GetOr("session_id", nil) != "session-new" {
		t.Fatalf("snapshot %v", sessionIDs(snapshot))
	}
	blocked, limit, err := TryAcquire(context.Background(), "session-other", "desktop", cfg, nil, "", true)
	if err != nil || blocked != nil || limit == nil {
		t.Fatalf("blocked %v limit %v err %v", blocked, limit, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLivenessTransferWriteFailureKeepsOldIdForRetry(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), ".icebath"))
	lease := mustAcquire(t, "session-old", "desktop", map[string]any{}, "", true)
	original := writeEntries
	writeEntries = failingWrite
	_, err := TransferActiveSession(lease, "session-new", nil)
	writeEntries = original
	if err == nil || !strings.Contains(err.Error(), "replace failed") {
		t.Fatalf("err %v", err)
	}
	if lease.SessionID() != "session-old" {
		t.Fatalf("session %s", lease.SessionID())
	}
	if ok, err := TransferActiveSession(lease, "session-new", nil); !ok || err != nil {
		t.Fatalf("retry %v %v", ok, err)
	}
	if lease.SessionID() != "session-new" {
		t.Fatalf("session %s", lease.SessionID())
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseWinsAgainstTransferWaitingOnSameLeaseLock(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), ".icebath"))
	lease := mustAcquire(t, "session-old", "desktop", map[string]any{}, "", true)
	releaseWrote, allowRelease, transferAtLock := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseBlocked, transferring atomic.Bool
	var transferSignal sync.Once
	originalWrite, originalEnter := writeEntries, enterFileLock
	swap(t, &writeEntries, func(path string, entries []*pyjson.Object) error {
		err := originalWrite(path, entries)
		if releaseBlocked.CompareAndSwap(false, true) {
			close(releaseWrote)
			select {
			case <-allowRelease:
			case <-time.After(5 * time.Second):
				t.Error("release never allowed to finish")
			}
		}
		return err
	})
	swap(t, &enterFileLock, func(path string) (func(), error) {
		if transferring.Load() {
			transferSignal.Do(func() { close(transferAtLock) })
		}
		return originalEnter(path)
	})
	releaseDone, transferDone := make(chan error, 1), make(chan bool, 1)
	go func() { releaseDone <- lease.Release() }()
	waitFor(t, releaseWrote)
	go func() {
		transferring.Store(true)
		ok, _ := TransferActiveSession(lease, "session-new", nil)
		transferDone <- ok
	}()
	waitFor(t, transferAtLock)
	close(allowRelease)
	select {
	case err := <-releaseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("release thread still alive")
	}
	select {
	case ok := <-transferDone:
		if ok {
			t.Fatal("transfer resurrected a released lease")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transfer thread still alive")
	}
	if !lease.Released() {
		t.Fatal("lease not released")
	}
	if got := mustSnapshot(t, ""); len(got) != 0 {
		t.Fatalf("snapshot %v", got)
	}
}

// Waits up to five seconds for the channel to close
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("event never set")
	}
}

func TestLivenessGuardKeepsAJustAcquiredOwnLeaseItCannotVouchFor(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	fresh := mustAcquire(t, "fresh", "desktop", map[string]any{}, h, false)
	if !mustGuard(t, "fresh", h, map[string]bool{}) {
		t.Fatal("fresh lease not seen as active")
	}
	if got := leaseIDs(mustSnapshot(t, h)); !slices.Equal(got, []string{fresh.LeaseID}) {
		t.Fatalf("snapshot %v", got)
	}
	backdateLeases(t, h)
	if mustGuard(t, "fresh", h, map[string]bool{}) {
		t.Fatal("aged orphan still active")
	}
	if got := mustSnapshot(t, h); len(got) != 0 {
		t.Fatalf("snapshot %v", leaseIDs(got))
	}
}

func TestPidLivenessSelfPidSkipsExistsProbe(t *testing.T) {
	var calls []int
	swap(t, &pidExists, func(pid int) (bool, error) { calls = append(calls, pid); return true, nil })
	if got := pidLiveness(int64(os.Getpid()), nil, false); got != livenessAlive {
		t.Fatalf("self liveness %d", got)
	}
	if got := pidLiveness(int64(os.Getpid()), 1.0, false); got != livenessDead {
		t.Fatalf("recycled self pid liveness %d", got)
	}
	if len(calls) != 0 {
		t.Fatalf("exists probed %v", calls)
	}
}

func TestSnapshotPrunesAfterLockReleaseAndKeepsConcurrentLease(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	statePath, lockPath := leasePaths(context.Background(), h)
	pid := int64(os.Getpid())
	if err := writeEntries(statePath, []*pyjson.Object{
		pyjson.ObjectOf("lease_id", "live-1", "session_id", "s-live", "pid", pid),
		pyjson.ObjectOf("lease_id", "dead-1", "session_id", "s-dead", "pid", int64(1<<30)),
	}); err != nil {
		t.Fatal(err)
	}
	concurrentAcquire := func() {
		unlock, err := FileLock(lockPath, NoLockTimeout)
		if err != nil {
			t.Error(err)
			return
		}
		defer unlock()
		current, err := readEntries(statePath, true)
		if err != nil {
			t.Error(err)
			return
		}
		if err := writeEntries(statePath, append(current, pyjson.ObjectOf("lease_id", "new-1", "session_id", "s-new", "pid", pid))); err != nil {
			t.Error(err)
		}
	}
	var acquiredDuringPrune []bool
	realPrune := pruneDead
	swap(t, &pruneDead, func(entries []*pyjson.Object, strict bool, targetSessionID *string, targetPid *int) ([]*pyjson.Object, error) {
		done := make(chan struct{})
		go func() { concurrentAcquire(); close(done) }()
		select {
		case <-done:
			acquiredDuringPrune = append(acquiredDuringPrune, true)
		case <-time.After(5 * time.Second):
			acquiredDuringPrune = append(acquiredDuringPrune, false)
		}
		return realPrune(entries, strict, targetSessionID, targetPid)
	})
	live, err := RegistrySnapshot(context.Background(), h, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(acquiredDuringPrune, []bool{true}) {
		t.Fatalf("acquired during prune %v", acquiredDuringPrune)
	}
	if got := leaseIDs(live); !slices.Equal(got, []string{"live-1"}) {
		t.Fatalf("live %v", got)
	}
	persisted, err := readEntries(statePath, true)
	if err != nil {
		t.Fatal(err)
	}
	got := leaseIDs(persisted)
	slices.Sort(got)
	if !slices.Equal(got, []string{"live-1", "new-1"}) {
		t.Fatalf("persisted %v", got)
	}
}
