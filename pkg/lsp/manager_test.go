package lsp

import (
	"context"
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

