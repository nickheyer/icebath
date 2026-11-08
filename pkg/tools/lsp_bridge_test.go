package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/lsp"
)

func lspDiag(line int64, message string, endLine int64) map[string]any {
	return map[string]any{"severity": int64(1), "code": "reportUndefinedVariable", "source": "Pyright", "message": message,
		"range": map[string]any{"start": map[string]any{"line": line, "character": int64(0)},
			"end": map[string]any{"line": endLine, "character": int64(10)}}}
}

func lspTestRuntime(t *testing.T) *lsp.Runtime {
	rt := LSPRuntime(remoteTestEnv(t))
	return rt
}

func TestLSPShiftReplacementInMiddle(t *testing.T) {
	shift, err := lsp.BuildLineShift(lspTestRuntime(t), "a\nb\nc\nd\ne\n", "a\nb\nX\ne\n")
	if err != nil {
		t.Fatal(err)
	}
	for pre, want := range map[int64]int64{0: 0, 1: 1, 4: 3} {
		if got, ok := shift(pre); !ok || got != want {
			t.Errorf("shift(%d) = %d %v", pre, got, ok)
		}
	}
	for _, deleted := range []int64{2, 3} {
		if _, ok := shift(deleted); ok {
			t.Errorf("line %d must be deleted", deleted)
		}
	}
}

func TestLSPShiftDiagRemapsStartAndEnd(t *testing.T) {
	shift, _ := lsp.BuildLineShift(lspTestRuntime(t), "a\nb\nc\nd\n", "X\na\nb\nc\nd\n")
	remapped, ok, err := lsp.ShiftDiagnosticRange(lspDiag(2, "e", 2), shift)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	rng := remapped["range"].(map[string]any)
	if rng["start"].(map[string]any)["line"] != int64(3) || rng["end"].(map[string]any)["line"] != int64(3) {
		t.Fatalf("%v", rng)
	}
}

func TestLSPShiftBaselineDropsDeletedAndRemapsRest(t *testing.T) {
	shift, _ := lsp.BuildLineShift(lspTestRuntime(t), "a\nb\nc\nd\ne\n", "a\ne\n")
	out, err := lsp.ShiftBaseline([]map[string]any{lspDiag(0, "err on a", 0), lspDiag(1, "err on b", 1),
		lspDiag(2, "err on c", 2), lspDiag(4, "err on e", 4)}, shift)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0]["message"] != "err on a" || out[1]["message"] != "err on e" ||
		out[0]["range"].(map[string]any)["start"].(map[string]any)["line"] != int64(0) ||
		out[1]["range"].(map[string]any)["start"].(map[string]any)["line"] != int64(1) {
		t.Fatalf("%v", out)
	}
}

func TestLSPDiagKeyMatchesForShiftedBaselineAndPreservesNewInstances(t *testing.T) {
	shifted, ok, _ := lsp.ShiftDiagnosticRange(lspDiag(200, "Undefined variable", 200), func(l int64) (int64, bool) {
		if l >= 14 {
			return l - 14, true
		}
		return l, true
	})
	a, _ := lsp.DiagnosticKey(shifted)
	b, _ := lsp.DiagnosticKey(lspDiag(186, "Undefined variable", 186))
	if !ok || a != b {
		t.Fatalf("%q != %q", a, b)
	}
	shift, _ := lsp.BuildLineShift(lspTestRuntime(t), "good\ngood\ngood\n", "good\nbad\ngood\nbad\n")
	baseline, _ := lsp.ShiftBaseline([]map[string]any{lspDiag(0, "bad style", 0)}, shift)
	seen := map[string]bool{}
	for _, d := range baseline {
		k, _ := lsp.DiagnosticKey(d)
		seen[k] = true
	}
	var lines []int64
	for _, d := range []map[string]any{lspDiag(0, "bad style", 0), lspDiag(1, "bad style", 1), lspDiag(3, "bad style", 3)} {
		if k, _ := lsp.DiagnosticKey(d); !seen[k] {
			lines = append(lines, d["range"].(map[string]any)["start"].(map[string]any)["line"].(int64))
		}
	}
	if !slices.Equal(lines, []int64{1, 3}) {
		t.Fatalf("new instances %v", lines)
	}
}

func TestLSPDecodeJSONMatchesJSONLoads(t *testing.T) {
	v, err := lspDecodeJSON(`{"id": 7, "big": 123456789012345678901234567890, "f": 1.5, "n": NaN, "i": -Infinity, "s": "\u00e4"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["id"] != int64(7) || string(m["big"].(json.Number)) != "123456789012345678901234567890" || m["f"] != 1.5 ||
		m["s"] != "\u00e4" || !math.IsNaN(m["n"].(float64)) || !math.IsInf(m["i"].(float64), -1) {
		t.Fatalf("%#v", m)
	}
	var jde *lsp.JSONDecodeError
	if _, err := lspDecodeJSON("{\"a\": }"); !errors.As(err, &jde) || jde.Msg != "Expecting value: line 1 column 7 (char 6)" {
		t.Fatalf("%v", err)
	}
	if _, err := lspDecodeJSON("1" + strings.Repeat("0", 5000)); err == nil || errors.As(err, &jde) {
		t.Fatalf("the int digit limit raises ValueError, not a decode error: %v", err)
	}
}

func TestLSPTruncateElidesAtTheTotalCap(t *testing.T) {
	elideCalls = nil
	lspTruncate(strings.Repeat("x", 10000))
	if !slices.Equal(elideCalls, []int{lsp.MaxTotalChars}) {
		t.Fatalf("elide calls %v", elideCalls)
	}
}

func TestLSPReportForFileRaisesOnMalformedEntries(t *testing.T) {
	block := lspReportForFile("/a.py", []map[string]any{lspDiag(0, "m​", 0)})
	if !strings.Contains(block, "ERROR [1:1] m [reportUndefinedVariable] (Pyright)") {
		t.Fatalf("%q", block)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a non dict entry must raise for the write path to swallow")
		}
	}()
	lspReportForFile("/a.py", []map[string]any{nil})
}

// Git worktree under a fresh temp dir
func lspGitRepo(t *testing.T, root string) string {
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLSPOperatorWorkspaceRootsFollowTheSurfaceButNotScheduledWork(t *testing.T) {
	lsp.ClearWorkspaceCache()
	tmp := t.TempDir()
	launch := lspGitRepo(t, filepath.Join(tmp, "launch"))
	sibling := lspGitRepo(t, filepath.Join(tmp, "elsewhere", "clone"))
	if err := os.MkdirAll(filepath.Join(launch, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(launch, "src"))
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	t.Setenv("ICEBATH_CRON_SESSION", "")
	t.Setenv("TERMINAL_CWD", "")
	ResetTerminalScopeCache()
	env := remoteTestEnv(t)
	trusted := func() bool { return lspIsTrustedWorkspace(sibling, nil, lspOperatorWorkspaceRoots(env)) }
	if !lspIsTrustedWorkspace(launch, nil, lspOperatorWorkspaceRoots(env)) || trusted() {
		t.Fatal("only the launch worktree is the operator's")
	}
	SetLogicalSessionCwd(env, sibling)
	if !trusted() {
		t.Fatal("the session workspace a surface sets is the operator's too")
	}
	env.Values.Store("session.cron", true)
	if trusted() {
		t.Fatal("a cron job's workdir is scheduled by the model")
	}
	env.Values.Store("session.cron", false)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_1")
	if trusted() {
		t.Fatal("a kanban task's workspace is scheduled by the model")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	t.Setenv("HOME", sibling)
	t.Setenv("USERPROFILE", sibling)
	if trusted() {
		t.Fatal("a repo at or above home never counts")
	}
	if got := lspParseTrustedWorkspaces([]any{"~/code"}); len(got) != 1 || got[0] != filepath.Join(sibling, "code") {
		t.Fatalf("trusted %v", got)
	}
	if got := lspParseTrustedWorkspaces("~/code"); len(got) != 0 {
		t.Fatalf("a malformed value trusts nothing extra, got %v", got)
	}
}

func TestLSPDeferredQueueKeepsOlderNotesInFrontAndRestoresTakenOnes(t *testing.T) {
	owner := "S-queue-" + t.Name()
	base1, base2 := "v1", "v2"
	lspDeferredPost(owner, "/a.py", "first", &base1)
	lspDeferredPost(owner, "/a.py", "second", &base2)
	note := lspDeferredTake(owner, "/a.py")
	if note == nil || note.Text != "first\n\nsecond" || *note.BaselineText != "v1" {
		t.Fatalf("%+v", note)
	}
	if lspDeferredTake(owner, "/a.py") != nil || lspDeferredTake("", "/a.py") != nil {
		t.Fatal("a taken note is gone")
	}
	lspDeferredRestore(owner, "/a.py", []lspNote{*note})
	lspDeferredPost(owner, "/b.py", "b", nil)
	got := LSPPopNotes(owner, owner, "")
	if len(got) != 2 || got[0].Path != "/a.py" || got[1].Path != "/b.py" || len(LSPPopNotes(owner)) != 0 {
		t.Fatalf("%+v", got)
	}
	for i := range lspDeferredMaxPerOwner + 3 {
		lspDeferredPost(owner, fmt.Sprintf("/f%d.py", i), "x", nil)
	}
	if n := len(LSPPopNotes(owner)); n != lspDeferredMaxPerOwner {
		t.Fatalf("per owner cap %d", n)
	}
}

func TestLSPSettleForAgentWaitsForRunningChecksThenHandsVerdicts(t *testing.T) {
	session, task := "S-settle-"+t.Name(), "t-settle-"+t.Name()
	token := lspDeferredTrackPending(task)
	go func() {
		time.Sleep(100 * time.Millisecond)
		lspDeferredPost(task, "/c.py", "late MOCK001", nil)
		lspDeferredSettled(task, token)
	}()
	started := time.Now()
	notes := LSPSettleForAgent(session, task, LSPTurnEndWait)
	if len(notes) != 1 || notes[0].Text != "late MOCK001" || time.Since(started) > 3*time.Second {
		t.Fatalf("%+v after %v", notes, time.Since(started))
	}
	if got := LSPSettleForAgent("", "", time.Second); len(got) != 0 {
		t.Fatal(got)
	}
	lspDeferredPost(session, "/d.py", "queued", nil)
	var forwarded []string
	LSPForwardToAgent(session, task, func(key, text string) { forwarded = append(forwarded, key+"="+text) })
	if !slices.Equal(forwarded, []string{"lsp:/d.py=queued"}) {
		t.Fatalf("%v", forwarded)
	}
}

// Fake service recording what the write path asks of it
type fakeLSPService struct {
	mu        sync.Mutex
	enabled   bool
	enabledEr error
	started   [][3]string
	calls     int
}

func (f *fakeLSPService) HandlesExtension(string) (bool, error) { return true, nil }

func (f *fakeLSPService) EnabledFor(string) (bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.enabledEr != nil {
		panic(f.enabledEr)
	}
	return f.enabled, nil
}

func (f *fakeLSPService) StartEditCheck(path string, pre, post *string, _ string, _ []lspNote) (lspEditFuture, error) {
	f.mu.Lock()
	f.started = append(f.started, [3]string{path, *pre, *post})
	f.mu.Unlock()
	return nil, nil
}

func swapLSPLookup(t *testing.T, svc lspService) *int {
	lookups := 0
	prev := lspServiceLookup
	lspServiceLookup = func(*Env) lspService { lookups++; return svc }
	t.Cleanup(func() { lspServiceLookup = prev })
	return &lookups
}

func TestLSPMaybeDiagnosticsReturnsEmptyForNonLocal(t *testing.T) {
	fake := &fakeLSPService{enabled: true}
	lookups := swapLSPLookup(t, fake)
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "/sandbox")
	content := "x"
	if got := ops.maybeLSPDiagnostics("/sandbox/x.py", nil, &content, time.Now()); got != "" || *lookups != 0 || fake.calls != 0 {
		t.Fatalf("got %q lookups %d calls %d", got, *lookups, fake.calls)
	}
}

func TestLSPEditCheckStartedForLocalEnv(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLSPService{enabled: true}
	swapLSPLookup(t, fake)
	ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, true), dir)
	pre, post := "x = 1\n", "x = 2\n"
	if got := ops.maybeLSPDiagnostics(filepath.Join(dir, "x.py"), &pre, &post, time.Now()); got != "" {
		t.Fatalf("%q", got)
	}
	if len(fake.started) != 1 || fake.started[0] != [3]string{filepath.Join(dir, "x.py"), pre, post} {
		t.Fatalf("%v", fake.started)
	}
}

func TestLSPMaybeDiagnosticsSwallowsEnabledForFailure(t *testing.T) {
	dir := t.TempDir()
	swapLSPLookup(t, &fakeLSPService{enabledEr: errors.New("No such file or directory")})
	ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, true), dir)
	post := "x"
	if got := ops.maybeLSPDiagnostics(filepath.Join(dir, "x.py"), nil, &post, time.Now()); got != "" {
		t.Fatalf("%q", got)
	}
	if ops.lspWillHandle(filepath.Join(dir, "foo.ts")) {
		t.Fatal("a raising service never handles the file")
	}
}

func TestLSPShellLinterSkippedWhenLSPWillHandle(t *testing.T) {
	for _, ext := range []string{".ts", ".go", ".rs"} {
		dir := t.TempDir()
		swapLSPLookup(t, &fakeLSPService{enabled: true})
		termEnv := mockFileOpsEnv(func(command string, _ ExecOptions) *ExecResult {
			t.Errorf("shell linter invoked despite LSP claiming the file: %s", command)
			return &ExecResult{}
		})
		termEnv.envType = "local"
		ops := newShellFileOps(remoteTestEnv(t), termEnv, dir)
		src := writeTestFile(t, dir, "bad"+ext, []byte("intentionally invalid content\n"))
		r := ops.checkLint(src, nil)
		if !r.Skipped || !strings.Contains(r.Message, "LSP") {
			t.Fatalf("%s: %+v", ext, r)
		}
	}
}

var (
	lspMockOnce  sync.Once
	lspMockPath  string
	lspMockErr   error
	lspPkgDir, _ = os.Getwd()
)

// Builds the mock language server the pkg/lsp tests use
func lspMockServer(t *testing.T) string {
	lspMockOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lsp-mock-")
		if err != nil {
			lspMockErr = err
			return
		}
		lspMockPath = filepath.Join(dir, "mockserver")
		build := exec.Command("go", "build", "-o", lspMockPath, "../lsp/testdata/mockserver")
		build.Dir = lspPkgDir
		if out, err := build.CombinedOutput(); err != nil {
			lspMockErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if lspMockErr != nil {
		t.Fatal(lspMockErr)
	}
	return lspMockPath
}

// Session env of a profile whose pyright is the mock, with the by_content push gate path
func lspMockSession(t *testing.T, script string) (*Env, string, string) {
	installTestEnvHooks(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	t.Setenv("ICEBATH_HOME", h)
	repo := lspGitRepo(t, filepath.Join(tmp, "repo"))
	if err := os.WriteFile(filepath.Join(repo, "pyproject.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	gate := filepath.Join(tmp, "release-diagnostics")
	t.Setenv("MOCK_LSP_GATE", gate)
	mock := lspMockServer(t)
	idx := slices.IndexFunc(lsp.Servers, func(s *lsp.ServerDef) bool { return s.ServerID == "pyright" })
	original := lsp.Servers[idx]
	lsp.Servers[idx] = &lsp.ServerDef{ServerID: "pyright", Extensions: original.Extensions,
		ResolveRoot: func(_, ws string) string { return ws },
		BuildSpawn: func(root string, _ lsp.ServerContext) (*lsp.SpawnSpec, error) {
			return &lsp.SpawnSpec{Command: []string{mock}, WorkspaceRoot: root, Cwd: root, Env: map[string]string{"MOCK_LSP_SCRIPT": script}}, nil
		}}
	env := &Env{Home: h, Secrets: config.NewSecrets(h), Values: &sync.Map{}, Config: config.Tree{"lsp": map[string]any{
		"wait_timeout": 20.0, "install_strategy": "manual", "idle_timeout": 0, "trusted_workspaces": []any{repo}}}}
	t.Cleanup(func() {
		os.WriteFile(gate, nil, 0o644)
		lsp.ShutdownServices()
		lsp.Servers[idx] = original
		lsp.ClearWorkspaceCache()
	})
	return env, repo, gate
}

func TestLSPLateDiagnosticsReachTheSession(t *testing.T) {
	env, repo, gate := lspMockSession(t, "by_content")
	target := filepath.Join(repo, "b.py")
	pre, post := "x = 1\n", "x = bad\n"
	if err := os.WriteFile(target, []byte(post), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := newShellFileOps(env, bashFileOpsEnv(repo, true), repo).withContext(WithLSPOwner(context.Background(), "S-lsp"))
	started := time.Now()
	if got := ops.maybeLSPDiagnostics(target, &pre, &post, started); got != "" {
		t.Fatalf("a held server answered inline: %q", got)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the write blocked %v on a held language server", elapsed)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	notes := LSPSettleForAgent("S-lsp", "", 20*time.Second)
	if len(notes) != 1 || notes[0].Path != target || !strings.Contains(notes[0].Text, "MOCK001") || !strings.Contains(notes[0].Text, "earlier edit") {
		t.Fatalf("%+v", notes)
	}
}

func TestLSPDiagnosticsStayWithTheSessionThatMadeTheEdit(t *testing.T) {
	env, repo, gate := lspMockSession(t, "by_content")
	target := filepath.Join(repo, "d.py")
	edit := func(session, pre, post string) {
		if err := os.WriteFile(target, []byte(post), 0o644); err != nil {
			t.Fatal(err)
		}
		ops := newShellFileOps(env, bashFileOpsEnv(repo, true), repo).withContext(WithLSPOwner(context.Background(), session))
		if got := ops.maybeLSPDiagnostics(target, &pre, &post, time.Now()); got != "" {
			t.Fatalf("inline verdict %q", got)
		}
	}
	edit("S-A", "x = 1\n", "x = bad\n")
	edit("S-B", "x = bad\n", "x = bad\ny = 2\n")
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	notesB := LSPSettleForAgent("S-B", "", 20*time.Second)
	notesA := LSPSettleForAgent("S-A", "", 20*time.Second)
	if len(notesB) != 0 {
		t.Fatalf("session B was told about session A's error: %+v", notesB)
	}
	if len(notesA) == 0 || !strings.Contains(notesA[0].Text, "MOCK001") {
		t.Fatalf("session A never heard about its own error: %+v", notesA)
	}
}

func TestLSPCommandContextReportsTheProfileService(t *testing.T) {
	env, repo, _ := lspMockSession(t, "clean")
	cc := LSPCommandContext(env)
	svc, err := cc.Service()
	if err != nil || svc == nil {
		t.Fatal(svc, err)
	}
	if !svc.EnabledFor(filepath.Join(repo, "x.py"), lspOperatorWorkspaceRoots(env)) {
		t.Fatal("the profile service gates the repo in")
	}
	var out, errOut strings.Builder
	if code := lsp.RunCommand(context.Background(), cc, lsp.CommandArgs{Subcommand: "status"}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "  enabled:         True") || !strings.Contains(out.String(), "Registered Servers") {
		t.Fatal(out.String())
	}
}

func TestLSPDeferredExportedPostTrackAndSettle(t *testing.T) {
	owner := "S-exported"
	token := LSPDeferredTrackPending(owner)
	go func() {
		time.Sleep(20 * time.Millisecond)
		LSPDeferredPost(owner, "/repo/x.py", "MOCK001 late", nil)
		LSPDeferredSettled(owner, token)
	}()
	notes := LSPSettleForAgent(owner, "", 5*time.Second)
	if len(notes) != 1 || notes[0].Path != "/repo/x.py" || notes[0].Text != "MOCK001 late" {
		t.Fatalf("%+v", notes)
	}
}
