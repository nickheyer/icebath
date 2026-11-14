package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Enabled service with the given wait timeout and manual installs, shut down at cleanup
func testService(t *testing.T, rt *Runtime, o Options) *Service {
	t.Helper()
	o.Enabled = true
	if o.WaitMode == "" {
		o.WaitMode = "document"
	}
	if o.InstallStrategy == "" {
		o.InstallStrategy = "manual"
	}
	svc := NewService(rt, o)
	t.Cleanup(svc.Shutdown)
	return svc
}

// Operator roots of the test process cwd
func cwdRoots() map[string]bool {
	return OperatorWorkspaceRoots([]func() (string, error){os.Getwd})
}

func recordBudgets(t *testing.T) func() []time.Duration {
	var mu sync.Mutex
	var budgets []time.Duration
	prev := clientWaitForDiagnostics
	clientWaitForDiagnostics = func(c *Client, ctx context.Context, path string, version int, mode string, timeout time.Duration) (bool, error) {
		mu.Lock()
		budgets = append(budgets, timeout)
		mu.Unlock()
		return prev(c, ctx, path, version, mode, timeout)
	}
	t.Cleanup(func() { clientWaitForDiagnostics = prev })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration{}, budgets...)
	}
}

func TestReusedClientRefreshesLastUsedAndSurvivesReap(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	t.Chdir(repo)
	installMockServer(t, []string{"errors"}, "pyright")
	f := writeFile(t, filepath.Join(repo, "x.py"), "")
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 3.0, IdleTimeout: 60})
	svc.GetDiagnosticsSync(f, true, 0, nil, cwdRoots())
	svc.stateMu.Lock()
	key, _ := svc.clients.first()
	firstUsed := svc.lastUsed[key]
	svc.lastUsed[key] = firstUsed.Add(-120 * time.Second)
	svc.stateMu.Unlock()
	svc.GetDiagnosticsSync(f, true, 0, nil, cwdRoots())
	svc.stateMu.Lock()
	refreshed := svc.lastUsed[key].After(firstUsed.Add(-120 * time.Second))
	svc.stateMu.Unlock()
	if !refreshed {
		t.Fatal("re-acquiring a cached client must refresh its last use")
	}
	_, err := runOnLoop(svc.l, 5*time.Second, func(ctx context.Context) (struct{}, error) { return struct{}{}, svc.reapIdleOnce(ctx) })
	must(t, err)
	svc.stateMu.Lock()
	_, kept := svc.clients.get(key)
	svc.stateMu.Unlock()
	if !kept || len(svc.GetStatus().Clients) == 0 {
		t.Fatal("a sweep right after reuse must keep the client")
	}
}

func TestReaperSurvivesSweepError(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	t.Chdir(repo)
	installMockServer(t, []string{"errors"}, "pyright")
	f := writeFile(t, filepath.Join(repo, "x.py"), "")
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 3.0, IdleTimeout: 0.1})
	var mu sync.Mutex
	calls := 0
	svc.l.with(func() {
		real := svc.reapOnce
		svc.reapOnce = func(ctx context.Context) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return errors.New("sweep sabotage")
			}
			return real(ctx)
		}
	})
	svc.GetDiagnosticsSync(f, true, 0, nil, cwdRoots())
	if len(svc.GetStatus().Clients) == 0 {
		t.Fatal("no client")
	}
	waitUntil(t, 3*time.Second, "the reaper to reap after a failed sweep", func() bool { return len(svc.GetStatus().Clients) == 0 })
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 2 || svc.reaperTask == nil || svc.reaperTask.finished() {
		t.Fatalf("calls %d, reaper alive %v", n, svc.reaperTask != nil && !svc.reaperTask.finished())
	}
}

func TestUnrelatedProjectNotAffectedByBroken(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	repoA, repoB := gitRepo(t, tmp, "repo"), gitRepo(t, tmp, "repo-b")
	aSrc := writeFile(t, filepath.Join(repoA, "x.py"), "")
	bSrc := writeFile(t, filepath.Join(repoB, "x.py"), "")
	t.Chdir(repoA)
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 2.0})
	svc.markBrokenForFile(aSrc, errors.New("simulated"))
	if svc.EnabledFor(aSrc, cwdRoots()) {
		t.Fatal("project A must be skipped")
	}
	t.Chdir(repoB)
	if !svc.EnabledFor(bSrc, cwdRoots()) {
		t.Fatal("project B must stay enabled")
	}
}

func TestMarkBrokenHandlesNoWorkspaceSilently(t *testing.T) {
	resetCaches(t)
	src := writeFile(t, filepath.Join(t.TempDir(), "orphan.py"), "")
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 2.0})
	svc.markBrokenForFile(src, errors.New("x"))
	if svc.broken.len() != 0 {
		t.Fatal("no workspace means no key")
	}
}

// Replaces pyright with a server whose spawn fails, the outer failure path of a snapshot
func installFailingServer(t *testing.T) {
	idx := slices.IndexFunc(Servers, func(s *ServerDef) bool { return s.ServerID == "pyright" })
	original := Servers[idx]
	Servers[idx] = &ServerDef{ServerID: "pyright", Extensions: original.Extensions, ResolveRoot: original.ResolveRoot,
		BuildSpawn: func(string, ServerContext) (*SpawnSpec, error) { return nil, errors.New("outer-timeout simulated") }}
	t.Cleanup(func() { Servers[idx] = original })
}

func TestSnapshotFailureMarksBrokenViaOuterTimeout(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	t.Chdir(repo)
	src := writeFile(t, filepath.Join(repo, "x.py"), "")
	installFailingServer(t)
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 2.0})
	if !svc.EnabledFor(src, cwdRoots()) {
		t.Fatal("not enabled before the failure")
	}
	svc.SnapshotBaseline(src, cwdRoots())
	if _, ok := svc.broken.get(pairKey{"pyright", repo}); !ok || svc.EnabledFor(src, cwdRoots()) {
		t.Fatal("the failed pair must be broken and skipped")
	}
}

func TestSkippedRequestOnBrokenRootIsLoggedAtInfo(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	src := writeFile(t, filepath.Join(repo, "x.py"), "")
	t.Chdir(repo)
	rt, h, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 2.0})
	svc.markBrokenForFile(src, errors.New("simulated"))
	for range 2 {
		if d, _ := svc.GetDiagnosticsSync(src, true, 0, nil, cwdRoots()); len(d) != 0 {
			t.Fatal(d)
		}
	}
	skipped := h.matching("marked broken")
	if len(skipped) != 2 || skipped[0].level != slog.LevelInfo || skipped[1].level != slog.LevelDebug ||
		!strings.Contains(skipped[0].msg, repo) || !strings.Contains(skipped[0].msg, "x.py") {
		t.Fatalf("%+v", skipped)
	}
}

func TestBrokenRootIsRetriedAfterBrokenRetrySeconds(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	t.Chdir(repo)
	src := writeFile(t, filepath.Join(repo, "x.py"), "")
	installFailingServer(t)
	rt, _, _ := testRuntime(t)
	svc, err := ServiceFromConfig(rt, map[string]any{"wait_timeout": 1.0, "install_strategy": "manual", "broken_retry_seconds": 0.2}, cwdRoots())
	must(t, err)
	t.Cleanup(svc.Shutdown)
	svc.SnapshotBaseline(src, cwdRoots())
	if svc.EnabledFor(src, cwdRoots()) || !slices.Contains(svc.GetStatus().Broken, [2]string{"pyright", repo}) {
		t.Fatal("not broken")
	}
	time.Sleep(300 * time.Millisecond)
	if !svc.EnabledFor(src, cwdRoots()) || len(svc.GetStatus().Broken) != 0 {
		t.Fatal("not retried after the window")
	}
}

// Replaces pyright with the errors mock, multi root or not, logging folder changes
func installFolderLoggingServer(t *testing.T, multiRoot bool, foldersLog string) func() int {
	idx := slices.IndexFunc(Servers, func(s *ServerDef) bool { return s.ServerID == "pyright" })
	original := Servers[idx]
	var mu sync.Mutex
	spawns := 0
	Servers[idx] = &ServerDef{ServerID: "pyright", Extensions: original.Extensions, ResolveRoot: func(_, ws string) string { return ws },
		BuildSpawn: func(root string, _ ServerContext) (*SpawnSpec, error) {
			mu.Lock()
			spawns++
			mu.Unlock()
			return &SpawnSpec{Command: mockCommand(t), WorkspaceRoot: root, Cwd: root,
				Env: mockEnv("errors", map[string]string{"MOCK_LSP_FOLDERS_LOG": foldersLog})}, nil
		}, MultiRoot: multiRoot, Description: "mock pyright"}
	t.Cleanup(func() { Servers[idx] = original })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return spawns
	}
}

func TestMultiRootServerSharesOneClientAcrossRoots(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	repoA, repoB := gitRepo(t, tmp, "repo-a"), gitRepo(t, tmp, "repo-b")
	writeFile(t, filepath.Join(repoA, "x.py"), "print('hi')\n")
	writeFile(t, filepath.Join(repoB, "x.py"), "print('hi')\n")
	foldersLog := filepath.Join(tmp, "folders.jsonl")
	spawns := installFolderLoggingServer(t, true, foldersLog)
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 3.0})
	t.Chdir(repoA)
	diagsA, _ := svc.GetDiagnosticsSync(filepath.Join(repoA, "x.py"), true, 0, nil, cwdRoots())
	t.Chdir(repoB)
	diagsB, _ := svc.GetDiagnosticsSync(filepath.Join(repoB, "x.py"), true, 0, nil, cwdRoots())
	if spawns() != 1 || svc.clients.len() != 1 {
		t.Fatalf("spawns %d clients %d", spawns(), svc.clients.len())
	}
	st := svc.GetStatus()
	if !slices.Equal(st.Clients[0].WorkspaceFolders, []string{repoA, repoB}) || st.Clients[0].WorkspaceRoot != repoA {
		t.Fatalf("status %+v", st.Clients)
	}
	raw, err := os.ReadFile(foldersLog)
	must(t, err)
	var added []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e map[string]any
		must(t, json.Unmarshal([]byte(line), &e))
		for _, f := range e["event"].(map[string]any)["added"].([]any) {
			added = append(added, f.(map[string]any)["uri"].(string))
		}
	}
	if !slices.Equal(added, []string{"file://" + repoB}) {
		t.Fatalf("added %v", added)
	}
	if len(diagsA) != 1 || len(diagsB) != 1 {
		t.Fatalf("diags %v %v", diagsA, diagsB)
	}
}

func TestSingleRootServerStillSpawnsPerRoot(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	repoA, repoB := gitRepo(t, tmp, "repo-a"), gitRepo(t, tmp, "repo-b")
	writeFile(t, filepath.Join(repoA, "x.py"), "print('hi')\n")
	writeFile(t, filepath.Join(repoB, "x.py"), "print('hi')\n")
	foldersLog := filepath.Join(tmp, "folders.jsonl")
	spawns := installFolderLoggingServer(t, false, foldersLog)
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 3.0})
	t.Chdir(repoA)
	svc.GetDiagnosticsSync(filepath.Join(repoA, "x.py"), true, 0, nil, cwdRoots())
	t.Chdir(repoB)
	svc.GetDiagnosticsSync(filepath.Join(repoB, "x.py"), true, 0, nil, cwdRoots())
	keys := svc.clients.keys()
	if spawns() != 2 || len(keys) != 2 || !slices.Contains(keys, pairKey{"pyright", repoA}) || !slices.Contains(keys, pairKey{"pyright", repoB}) {
		t.Fatalf("spawns %d keys %v", spawns(), keys)
	}
	if _, err := os.Stat(foldersLog); err == nil {
		t.Fatal("single root servers never get folder changes")
	}
}

func TestWorkspaceRemovalReleasesOnlyItsLanguageServers(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single-root", true: "multi-root"}[multi], func(t *testing.T) {
			resetCaches(t)
			resetRegistry(t)
			tmp := t.TempDir()
			kept, gone := gitRepo(t, tmp, "project"), gitRepo(t, tmp, "wt")
			writeFile(t, filepath.Join(kept, "x.py"), "x = 1\n")
			writeFile(t, filepath.Join(gone, "x.py"), "x = 1\n")
			installFolderLoggingServer(t, multi, filepath.Join(tmp, "folders.jsonl"))
			rt, _, _ := testRuntime(t)
			svc, _ := ServiceFor("/h", func() (*Service, error) {
				return NewService(rt, Options{Enabled: true, WaitMode: "document", WaitTimeout: 3, InstallStrategy: "manual", IdleTimeout: 600}), nil
			})
			roots := map[string]bool{kept: true, gone: true}
			svc.SnapshotBaseline(filepath.Join(gone, "x.py"), roots)
			if d, _ := svc.GetDiagnosticsSync(filepath.Join(gone, "x.py"), false, 0, nil, roots); len(d) == 0 {
				t.Fatal("no diagnostics for the doomed root")
			}
			if d, _ := svc.GetDiagnosticsSync(filepath.Join(kept, "x.py"), false, 0, nil, roots); len(d) == 0 {
				t.Fatal("no diagnostics for the kept root")
			}
			procs := map[pairKey]*serverProc{}
			svc.l.with(func() {
				for _, k := range svc.clients.keys() {
					c, _ := svc.clients.get(k)
					procs[k] = c.proc
				}
			})
			if n := ReleaseWorkspace(gone); n != map[bool]int{false: 1, true: 0}[multi] {
				t.Fatalf("released %d", n)
			}
			must(t, os.RemoveAll(gone))
			svc.l.with(func() {
				for _, k := range svc.clients.keys() {
					c, _ := svc.clients.get(k)
					for _, f := range c.workspaceFolders {
						if strings.HasPrefix(f, gone) {
							t.Errorf("released folder %s still served", f)
						}
					}
				}
			})
			for _, p := range svc.deltaBaseline.keys() {
				if strings.HasPrefix(p, gone) {
					t.Errorf("released baseline %s kept", p)
				}
			}
			if len(procs) == 2 {
				goneKey := pairKey{"pyright", gone}
				if _, ok := svc.clients.get(goneKey); ok || procs[goneKey].returncode() == nil {
					t.Fatal("released server process must have exited")
				}
			} else {
				svc.l.with(func() {
					c, _ := svc.clients.get(svc.clients.keys()[0])
					if !slices.Equal(c.workspaceFolders, []string{kept}) || !c.isRunning() {
						t.Errorf("folders %v running %v", c.workspaceFolders, c.isRunning())
					}
				})
			}
			if n, err := svc.ReleaseWorkspace(gone); err != nil || n != 0 {
				t.Fatal("a repeat release must be a no-op")
			}
			if d, _ := svc.GetDiagnosticsSync(filepath.Join(kept, "x.py"), false, 0, nil, roots); len(d) == 0 {
				t.Fatal("the sibling stopped serving diagnostics")
			}
		})
	}
}

func TestReaperShutsDownClientWhoseRootWasDeleted(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	repo := gitRepo(t, tmp, "repo")
	f := writeFile(t, filepath.Join(repo, "x.py"), "x = 1\n")
	installFolderLoggingServer(t, false, filepath.Join(tmp, "folders.jsonl"))
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 3.0, IdleTimeout: 600})
	roots := map[string]bool{repo: true}
	if d, _ := svc.GetDiagnosticsSync(f, false, 0, nil, roots); len(d) == 0 {
		t.Fatal("no diagnostics")
	}
	key, _ := svc.clients.first()
	var p *serverProc
	svc.l.with(func() { c, _ := svc.clients.get(key); p = c.proc })
	reap := func() {
		_, err := runOnLoop(svc.l, 10*time.Second, func(ctx context.Context) (struct{}, error) { return struct{}{}, svc.reapIdleOnce(ctx) })
		must(t, err)
	}
	reap()
	if _, ok := svc.clients.get(key); !ok {
		t.Fatal("a recently used root must survive the sweep")
	}
	must(t, os.RemoveAll(repo))
	reap()
	svc.stateMu.Lock()
	_, inClients := svc.clients.get(key)
	_, inUsed := svc.lastUsed[key]
	svc.stateMu.Unlock()
	if inClients || inUsed || p.returncode() == nil {
		t.Fatal("a deleted root's server must be reaped and exit")
	}
}

func TestExcludeRootsSkipsMatchingRootOnly(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	big, small := gitRepo(t, tmp, "big"), gitRepo(t, tmp, "small")
	writeFile(t, filepath.Join(big, "x.py"), "")
	writeFile(t, filepath.Join(small, "x.py"), "")
	t.Chdir(tmp)
	idx := slices.IndexFunc(Servers, func(s *ServerDef) bool { return s.ServerID == "pyright" })
	original := Servers[idx]
	Servers[idx] = &ServerDef{ServerID: "pyright", Extensions: original.Extensions, ResolveRoot: original.ResolveRoot,
		BuildSpawn: func(string, ServerContext) (*SpawnSpec, error) {
			t.Error("excluded root reached the server")
			return nil, nil
		}}
	t.Cleanup(func() { Servers[idx] = original })
	rt, h, _ := testRuntime(t)
	roots := map[string]bool{big: true, small: true}
	svc, err := ServiceFromConfig(rt, map[string]any{"wait_timeout": 1.0, "install_strategy": "manual",
		"exclude_roots": []any{filepath.Join(tmp, "bi*")}}, roots)
	must(t, err)
	svc.SnapshotBaseline(filepath.Join(big, "x.py"), roots)
	if svc.EnabledFor(filepath.Join(big, "x.py"), roots) || !svc.EnabledFor(filepath.Join(small, "x.py"), roots) {
		t.Fatal("exclude must gate only the matching root")
	}
	if len(h.matching("exclude_roots", big)) == 0 {
		t.Fatal("the gate must be visible in the log")
	}
	svc.Shutdown()
	bad, err := ServiceFromConfig(rt, map[string]any{"install_strategy": "manual", "exclude_roots": big}, roots)
	must(t, err)
	defer bad.Shutdown()
	if bad.EnabledFor(filepath.Join(small, "x.py"), roots) {
		t.Fatal("a malformed key fails closed")
	}
	var warned []logRecord
	for _, r := range h.all() {
		if r.level >= slog.LevelWarn && strings.Contains(r.msg, "exclude_roots") {
			warned = append(warned, r)
		}
	}
	if len(warned) == 0 || !strings.Contains(warned[0].msg, "list") || !strings.Contains(warned[0].msg, "str") {
		t.Fatalf("%+v", warned)
	}
}

func TestWarmupTimeoutAppliesToColdRootOnly(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	src := writeFile(t, filepath.Join(repo, "x.py"), "")
	t.Chdir(repo)
	installMockServer(t, []string{"clean"}, "pyright")
	budgets := recordBudgets(t)
	rt, _, _ := testRuntime(t)
	svc, err := ServiceFromConfig(rt, map[string]any{"wait_timeout": 1.0, "warmup_timeout": 30.0, "install_strategy": "manual"}, cwdRoots())
	must(t, err)
	t.Cleanup(svc.Shutdown)
	svc.SnapshotBaseline(src, cwdRoots())
	if d, _ := svc.GetDiagnosticsSync(src, true, 0, nil, cwdRoots()); len(d) != 0 {
		t.Fatal(d)
	}
	if got := budgets(); !slices.Equal(got, []time.Duration{30 * time.Second, time.Second}) {
		t.Fatalf("budgets %v", got)
	}
}

func TestSlowPushIsWaitedFor(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	f := writeFile(t, filepath.Join(dir, "x.py"), "bad code\n")
	c := mockClient(t, l, rt, dir, "slow_push", map[string]string{"MOCK_LSP_PUSH_DELAY": "0.8"})
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		defer c.shutdown(ctx)
		if err := c.start(ctx); err != nil {
			return struct{}{}, err
		}
		v0, _ := c.openFile(ctx, f, "python", nil)
		if ok, _ := c.waitForDiagnostics(ctx, f, v0, "document", 2*time.Second); !ok {
			t.Error("first push")
		}
		if d, _ := c.diagnosticsFor(f, true); len(d) != 1 {
			t.Errorf("first diags %v", d)
		}
		must(t, os.WriteFile(f, []byte("good code\n"), 0o644))
		v1, _ := c.openFile(ctx, f, "python", nil)
		if ok, _ := c.waitForDiagnostics(ctx, f, v1, "document", 5*time.Second); !ok {
			t.Error("slow push within budget must satisfy the wait")
		}
		if d, _ := c.diagnosticsFor(f, true); len(d) != 0 {
			t.Errorf("fresh diags %v", d)
		}
		return struct{}{}, nil
	})
	must(t, err)
}

func TestServiceReportsNoDataNotStaleErrors(t *testing.T) {
	resetCaches(t)
	repo := gitRepo(t, t.TempDir(), "repo")
	t.Chdir(repo)
	installMockServer(t, []string{"stale"}, "pyright")
	f := writeFile(t, filepath.Join(repo, "x.py"), "bad code\n")
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 1.0})
	if first, _ := svc.GetDiagnosticsSync(f, false, 0, nil, cwdRoots()); len(first) != 1 {
		t.Fatalf("first %v", first)
	}
	must(t, os.WriteFile(f, []byte("good code\n"), 0o644))
	if ghost, _ := svc.GetDiagnosticsSync(f, false, 0, nil, cwdRoots()); len(ghost) != 0 {
		t.Fatalf("stale pre-edit error reported as current: %v", ghost)
	}
	if !svc.EnabledFor(f, cwdRoots()) || len(svc.GetStatus().Broken) != 0 {
		t.Fatal("slow is not dead")
	}
}

func TestReusedMultirootClientAttachesOutsideStateLock(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	repo := gitRepo(t, tmp, "repo")
	root := filepath.Join(repo, "worktree-b")
	src := writeFile(t, filepath.Join(root, "example.py"), "")
	idx := slices.IndexFunc(Servers, func(s *ServerDef) bool { return s.ServerID == "pyright" })
	original := Servers[idx]
	Servers[idx] = &ServerDef{ServerID: "pyright", Extensions: []string{".py"}, ResolveRoot: func(string, string) string { return root },
		BuildSpawn: func(string, ServerContext) (*SpawnSpec, error) { return nil, nil }, MultiRoot: true}
	t.Cleanup(func() { Servers[idx] = original })
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 0.1, OperatorRoots: map[string]bool{repo: true}})
	shared := newClient(svc.l, rt, ClientOptions{ServerID: "pyright", WorkspaceRoot: repo, Command: mockCommand(t), Env: mockEnv("clean", nil)})
	_, err := runOnLoop(svc.l, 10*time.Second, func(ctx context.Context) (struct{}, error) { return struct{}{}, shared.start(ctx) })
	must(t, err)
	svc.stateMu.Lock()
	svc.clients.set(pairKey{"pyright", ""}, shared)
	svc.lastUsed[pairKey{"pyright", ""}] = time.Time{}
	svc.stateMu.Unlock()
	lockFree := false
	prev := clientSendNotification
	clientSendNotification = func(c *Client, ctx context.Context, method string, params any) error {
		if method == "workspace/didChangeWorkspaceFolders" {
			if svc.stateMu.TryLock() {
				lockFree = true
				svc.stateMu.Unlock()
			}
		}
		return prev(c, ctx, method, params)
	}
	t.Cleanup(func() { clientSendNotification = prev })
	got, err := runOnLoop(svc.l, 10*time.Second, func(ctx context.Context) (*Client, error) { return svc.getOrSpawn(ctx, src) })
	must(t, err)
	if got != shared || !lockFree {
		t.Fatalf("got %p want %p, lock free during attach %v", got, shared, lockFree)
	}
}

func TestDeltaBaselineIsCappedByWriteRecency(t *testing.T) {
	rt, _, _ := testRuntime(t)
	svc := NewService(rt, Options{WaitMode: "document", WaitTimeout: 0.1, InstallStrategy: "manual"})
	for i := range deltaBaselineCap {
		svc.setDeltaBaseline("/repo/f"+strings.Repeat("x", i)+".py", []map[string]any{})
	}
	rewritten := []map[string]any{{"message": "rewritten"}}
	svc.setDeltaBaseline("/repo/f.py", rewritten)
	svc.setDeltaBaseline("/repo/new.py", []map[string]any{})
	if svc.deltaBaseline.len() != deltaBaselineCap {
		t.Fatalf("len %d", svc.deltaBaseline.len())
	}
	if _, ok := svc.deltaBaseline.get("/repo/fx.py"); ok {
		t.Fatal("the path written longest ago must be evicted")
	}
	if got, _ := svc.deltaBaseline.get("/repo/f.py"); len(got) != 1 {
		t.Fatal("a re-touched path is refreshed, not aged out")
	}
	if _, ok := svc.deltaBaseline.get("/repo/new.py"); !ok {
		t.Fatal("new path missing")
	}
}

func TestNestedSingleRootClientIsFoundByCurrentLookup(t *testing.T) {
	resetCaches(t)
	repo := filepath.Join(t.TempDir(), "repo")
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	nested := filepath.Join(repo, "package")
	writeFile(t, filepath.Join(nested, "package.json"), "{}")
	src := writeFile(t, filepath.Join(nested, "x.ts"), "const x = 1;\n")
	t.Chdir(repo)
	idx := slices.IndexFunc(Servers, func(s *ServerDef) bool { return s.ServerID == "typescript" })
	original := Servers[idx]
	if original.ResolveRoot(src, repo) != nested || original.MultiRoot {
		t.Fatal("typescript resolves the nested package root")
	}
	mocked := *original
	mocked.BuildSpawn = func(root string, _ ServerContext) (*SpawnSpec, error) {
		return &SpawnSpec{Command: mockCommand(t), WorkspaceRoot: root, Cwd: root, Env: mockEnv("errors", nil)}, nil
	}
	mocked.SeedFirstPush = false
	Servers[idx] = &mocked
	t.Cleanup(func() { Servers[idx] = original })
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 5})
	svc.SnapshotBaseline(src, cwdRoots())
	live, _ := svc.GetDiagnosticsSync(src, false, 0, nil, cwdRoots())
	if len(live) == 0 {
		t.Fatal("no live diagnostics")
	}
	current, err := runOnLoop(svc.l, 5*time.Second, svc.currentDiagsAsync(src))
	must(t, err)
	if len(current) != len(live) {
		t.Fatalf("current %v live %v", current, live)
	}
}

// Recorded initialization options per server and root of spawns that never start
func recordSpawns(t *testing.T) map[pairKey]map[string]any {
	handed := map[pairKey]map[string]any{}
	var mu sync.Mutex
	prev := clientStart
	clientStart = func(c *Client, _ context.Context) error {
		mu.Lock()
		handed[pairKey{c.ServerID, c.WorkspaceRoot}] = maps.Clone(c.initOptions)
		mu.Unlock()
		return errors.New("recorded, not started")
	}
	t.Cleanup(func() { clientStart = prev })
	return handed
}

var trustServers = map[string]string{"pyright": "a.py", "typescript": "a.ts", "vue-language-server": "a.vue",
	"svelte-language-server": "a.svelte", "rust-analyzer": "a.rs", "jdtls": "A.java", "kotlin-language-server": "a.kt"}

// Icebath side staging tree with a JS TypeScript SDK and Vue 2.x, returning a launcher inside it
func icebathSideTree(t *testing.T, rt *Runtime) string {
	t.Setenv("VIRTUAL_ENV", "")
	staging := filepath.Join(rt.Home, "lsp", "node_modules")
	writeFile(t, filepath.Join(staging, "typescript", "lib", "typescript.js"), "")
	writeFile(t, filepath.Join(staging, "typescript", "lib", "tsserver.js"), "")
	writeFile(t, filepath.Join(staging, "@vue", "language-server", "package.json"), `{"version": "2.2.12"}`)
	return writeFile(t, filepath.Join(staging, "server-launcher"), "")
}

// Checkout shipping its own interpreter, TypeScript SDK and build files
func checkoutWithToolchain(t *testing.T, root string) {
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	for _, p := range []string{"pyproject.toml", ".venv/bin/python", ".venv/Scripts/python.exe", "node_modules/typescript/lib/typescript.js",
		"node_modules/typescript/lib/tsserver.js", "node_modules/.bin/tsc", "Cargo.toml", "build.rs", "rust-toolchain.toml", "build.gradle.kts"} {
		writeFile(t, filepath.Join(root, p), "")
	}
}

// Every string inside a decoded value
func allStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case map[string]any:
		var out []string
		for _, e := range t {
			out = append(out, allStrings(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, allStrings(e)...)
		}
		return out
	}
	return nil
}

// Config to service to spawn for every trust server file in every root with the cwd in tmp/launch/src
func trustSpawns(t *testing.T, rt *Runtime, tmp, launcher string, roots []string, trusted []string) (map[pairKey]map[string]any, Status) {
	servers := map[string]any{}
	for sid := range trustServers {
		servers[sid] = map[string]any{"command": []any{launcher}}
	}
	tw := []any{}
	for _, p := range trusted {
		tw = append(tw, p)
	}
	must(t, os.MkdirAll(filepath.Join(tmp, "launch", "src"), 0o755))
	t.Chdir(filepath.Join(tmp, "launch", "src"))
	ClearWorkspaceCache()
	handed := recordSpawns(t)
	svc, err := ServiceFromConfig(rt, map[string]any{"trusted_workspaces": tw, "servers": servers}, cwdRoots())
	must(t, err)
	defer svc.Shutdown()
	for _, root := range roots {
		for _, name := range trustServers {
			f := writeFile(t, filepath.Join(root, name), "")
			if _, err := runOnLoop(svc.l, 10*time.Second, func(ctx context.Context) (*Client, error) { return svc.getOrSpawn(ctx, f) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	return handed, svc.GetStatus()
}

func TestUntrustedCheckoutStartsOnlyAllowlistedServersPinnedToIcebathCode(t *testing.T) {
	rt, _, _ := testRuntime(t)
	tmp := t.TempDir()
	launcher := icebathSideTree(t, rt)
	launch, clone := filepath.Join(tmp, "launch"), filepath.Join(tmp, "elsewhere", "clone")
	checkoutWithToolchain(t, launch)
	checkoutWithToolchain(t, clone)
	handed, status := trustSpawns(t, rt, tmp, launcher, []string{launch, clone}, nil)
	untrusted, trusted := map[string]map[string]any{}, map[string]map[string]any{}
	for k, init := range handed {
		switch k.root {
		case clone:
			untrusted[k.serverID] = init
		case launch:
			trusted[k.serverID] = init
		}
	}
	for sid := range trustServers {
		_, started := untrusted[sid]
		if started != UntrustedSafeServers[sid] {
			t.Errorf("%s started in the clone: %v", sid, started)
		}
		if _, ok := trusted[sid]; !ok {
			t.Errorf("%s did not start in the trusted launch worktree", sid)
		}
	}
	for _, want := range [][2]string{{"rust-analyzer", clone}, {"jdtls", clone}, {"vue-language-server", clone}} {
		if !slices.Contains(status.UntrustedSkipped, want) {
			t.Errorf("untrusted skips %v lack %v", status.UntrustedSkipped, want)
		}
	}
	for sid, init := range untrusted {
		for _, s := range allStrings(init) {
			if filepath.IsAbs(s) && IsInsideWorkspace(s, clone) {
				t.Errorf("%s handed project code %s", sid, s)
			}
		}
	}
	tsserver := untrusted["typescript"]["tsserver"].(map[string]any)["path"].(string)
	if !isFile(filepath.Join(tsserver, "tsserver.js")) || untrusted["svelte-language-server"]["isTrusted"] != false {
		t.Fatalf("typescript %v svelte %v", untrusted["typescript"], untrusted["svelte-language-server"])
	}
	pythonPath := trusted["pyright"]["python"].(map[string]any)["pythonPath"].(string)
	if !IsInsideWorkspace(pythonPath, filepath.Join(launch, ".venv")) {
		t.Fatalf("trusted pyright interpreter %s", pythonPath)
	}
	for _, sid := range []string{"typescript", "svelte-language-server", "rust-analyzer"} {
		if len(trusted[sid]) != 0 {
			t.Errorf("%s trusted init %v", sid, trusted[sid])
		}
	}
}

func TestOnlyOperatorWorkspacesAndListedDirectoriesAreTrusted(t *testing.T) {
	rt, _, _ := testRuntime(t)
	tmp := t.TempDir()
	launcher := icebathSideTree(t, rt)
	launch, listed := filepath.Join(tmp, "launch"), filepath.Join(tmp, "listed", "proj")
	nested, sibling := filepath.Join(launch, "vendor", "clone"), filepath.Join(tmp, "elsewhere", "clone")
	roots := []string{launch, nested, sibling, listed}
	for _, r := range roots {
		checkoutWithToolchain(t, r)
	}
	handed, _ := trustSpawns(t, rt, tmp, launcher, roots, []string{filepath.Join(tmp, "listed")})
	for _, root := range []string{launch, listed} {
		py := handed[pairKey{"pyright", root}]["python"].(map[string]any)["pythonPath"].(string)
		if !IsInsideWorkspace(py, filepath.Join(root, ".venv")) {
			t.Errorf("%s pyright %s", root, py)
		}
		if _, ok := handed[pairKey{"rust-analyzer", root}]; !ok {
			t.Errorf("%s rust-analyzer missing", root)
		}
	}
	for _, root := range []string{nested, sibling} {
		if py, ok := handed[pairKey{"pyright", root}]["python"].(map[string]any); ok {
			if p, _ := py["pythonPath"].(string); IsInsideWorkspace(p, root) {
				t.Errorf("%s pyright got the project interpreter", root)
			}
		}
		if _, ok := handed[pairKey{"rust-analyzer", root}]; ok {
			t.Errorf("%s rust-analyzer started", root)
		}
	}
}

func TestServiceGetsDiagnosticsFromConfigDeclaredServer(t *testing.T) {
	resetCaches(t)
	rt, _, _ := testRuntime(t)
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	must(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))
	target := writeFile(t, filepath.Join(ws, "doc.pnch"), "hello\n")
	writeFile(t, filepath.Join(ws, "x.rs"), "fn main() {}\n")
	svc, err := ServiceFromConfig(rt, map[string]any{"install_strategy": "manual", "wait_timeout": 5.0, "idle_timeout": 0,
		"trusted_workspaces": []any{ws},
		"servers": map[string]any{"panache": map[string]any{"command": []any{mockBinary(t)}, "extensions": []any{".pnch"},
			"env": map[string]any{"MOCK_LSP_SCRIPT": "errors"}}}}, nil)
	must(t, err)
	defer svc.Shutdown()
	if !svc.EnabledFor(target, nil) {
		t.Fatal("not enabled")
	}
	diags, err := svc.GetDiagnosticsSync(target, true, 0, nil, nil)
	must(t, err)
	if len(diags) == 0 || diags[0]["source"] != "mock-lsp" {
		t.Fatalf("diags %v", diags)
	}
	if svc.serverFor(filepath.Join(ws, "x.rs")).ServerID != "rust-analyzer" {
		t.Fatal("built in extensions stay on the registry")
	}
	if !svc.HandlesExtension(".pnch") || StaticRegistryHandles(".pnch") || !StaticRegistryHandles(".rs") {
		t.Fatal("extension claims")
	}
}

// Repo whose pyright is the mock script, with the push gate path the by_content script waits on
func editCheckRepo(t *testing.T, script string) (string, string, *Service) {
	resetCaches(t)
	tmp := t.TempDir()
	repo := gitRepo(t, tmp, "repo")
	t.Chdir(repo)
	gate := filepath.Join(tmp, "release-diagnostics")
	t.Setenv("MOCK_LSP_GATE", gate)
	installMockServer(t, []string{script}, "pyright")
	rt, _, _ := testRuntime(t)
	svc := testService(t, rt, Options{WaitTimeout: 20, TrustedWorkspaces: []any{repo}})
	t.Cleanup(func() { os.WriteFile(gate, nil, 0o644) })
	return repo, gate, svc
}

func strp(s string) *string { return &s }

func TestEditCheckSilentServerDoesNotHoldTheWrite(t *testing.T) {
	repo, _, svc := editCheckRepo(t, "silent")
	target := writeFile(t, filepath.Join(repo, "a.py"), "x = 1\n")
	for _, step := range [][2]string{{"x = 1\n", "x = 2\n"}, {"x = 2\n", "x = 3\n"}} {
		must(t, os.WriteFile(target, []byte(step[1]), 0o644))
		started := time.Now()
		fut := mustStartEditCheck(t, svc, target, strp(step[0]), strp(step[1]), "S-lsp", nil, cwdRoots())
		if fut == nil {
			t.Fatal("no check")
		}
		if _, ready, _ := fut.Wait(InlineWait(started)); ready {
			t.Fatal("a silent server cannot have answered")
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("the write waited %v", elapsed)
		}
	}
}

func TestEditCheckLateVerdictCarriesOnlyTheIntroducedError(t *testing.T) {
	repo, gate, svc := editCheckRepo(t, "by_content")
	target := writeFile(t, filepath.Join(repo, "b.py"), "x = bad\n")
	started := time.Now()
	fut := mustStartEditCheck(t, svc, target, strp("x = 1\n"), strp("x = bad\n"), "S-lsp", nil, cwdRoots())
	if _, ready, _ := fut.Wait(InlineWait(started)); ready {
		t.Fatal("the held server answered inline")
	}
	done := make(chan *CheckResult, 1)
	fut.OnDone(func(res *CheckResult, cancelled bool, err error) {
		if cancelled || err != nil {
			t.Errorf("cancelled %v err %v", cancelled, err)
		}
		done <- res
	})
	must(t, os.WriteFile(gate, nil, 0o644))
	select {
	case res := <-done:
		if !res.HasVerdict || len(res.Diagnostics) != 1 || res.Diagnostics[0]["code"] != "MOCK001" || *res.BaselineText != "x = 1\n" {
			t.Fatalf("%+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no late verdict")
	}
}

func TestEditCheckSessionsAreJudgedOnTheirOwnBytes(t *testing.T) {
	repo, gate, svc := editCheckRepo(t, "by_content")
	must(t, os.WriteFile(gate, nil, 0o644))
	target := writeFile(t, filepath.Join(repo, "e.py"), "x = 1\n")
	held, release := make(chan struct{}), make(chan struct{})
	svc.l.spawn(context.Background(), func(context.Context) {
		close(held)
		<-release
	})
	<-held
	must(t, os.WriteFile(target, []byte("x = 2\n"), 0o644))
	futA := mustStartEditCheck(t, svc, target, strp("x = 1\n"), strp("x = 2\n"), "S-A2", nil, cwdRoots())
	must(t, os.WriteFile(target, []byte("x = bad\n"), 0o644))
	futB := mustStartEditCheck(t, svc, target, strp("x = 2\n"), strp("x = bad\n"), "S-B2", nil, cwdRoots())
	close(release)
	resA, _, errA := futA.Wait(20 * time.Second)
	resB, _, errB := futB.Wait(20 * time.Second)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if !resA.HasVerdict || len(resA.Diagnostics) != 0 {
		t.Fatalf("A was told about B's error: %+v", resA)
	}
	if !resB.HasVerdict || len(resB.Diagnostics) != 1 {
		t.Fatalf("B never heard about its own error: %+v", resB)
	}
}

func TestEditCheckSameSessionTakesOverItsUnfinishedCheck(t *testing.T) {
	repo, gate, svc := editCheckRepo(t, "by_content")
	target := writeFile(t, filepath.Join(repo, "d.py"), "x = bad\n")
	first := mustStartEditCheck(t, svc, target, strp("x = 1\n"), strp("x = bad\n"), "S-A", nil, cwdRoots())
	time.Sleep(200 * time.Millisecond)
	must(t, os.WriteFile(target, []byte("x = bad\ny = 2\n"), 0o644))
	second := mustStartEditCheck(t, svc, target, strp("x = bad\n"), strp("x = bad\ny = 2\n"), "S-A", nil, cwdRoots())
	must(t, os.WriteFile(gate, nil, 0o644))
	r1, _, err1 := first.Wait(20 * time.Second)
	r2, _, err2 := second.Wait(20 * time.Second)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if !r1.Superseded {
		t.Fatalf("the older check must be superseded: %+v", r1)
	}
	if !r2.HasVerdict || len(r2.Diagnostics) != 1 || *r2.BaselineText != "x = 1\n" {
		t.Fatalf("the newer check diffs against the oldest baseline: %+v", r2)
	}
}
