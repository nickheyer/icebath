package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Python PermissionError analog carrying its own message
type ftrPermissionError struct{ msg string }

func (e ftrPermissionError) Error() string        { return e.msg }
func (e ftrPermissionError) Is(target error) bool { return target == fs.ErrPermission }

// Log handler remembering the level of every record it receives
type ftrLevelRecorder struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (r *ftrLevelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *ftrLevelRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.levels = append(r.levels, rec.Level)
	r.mu.Unlock()
	return nil
}
func (r *ftrLevelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *ftrLevelRecorder) WithGroup(string) slog.Handler      { return r }

// Routes file backend lookups to the scripted host ops and counts every lookup
func ftrInstallCounted(t *testing.T, env *Env, ops *scriptedFileOps) *int {
	t.Helper()
	installScriptedFileOps(t, env, ops, nil)
	n := 0
	inner := getFileOps
	getFileOps = func(e *Env, taskID string) (*fileOpsHandle, error) {
		n++
		return inner(e, taskID)
	}
	t.Cleanup(func() { getFileOps = inner })
	return &n
}

// Makes every file backend lookup fail with the given error
func ftrFailFileOps(t *testing.T, err error) {
	t.Helper()
	prev := getFileOps
	getFileOps = func(*Env, string) (*fileOpsHandle, error) { return nil, err }
	t.Cleanup(func() { getFileOps = prev })
}

// Number of scripted calls whose label starts with the prefix
func ftrCalls(ops *scriptedFileOps, prefix string) int {
	n := 0
	for _, c := range ops.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// search_tool arguments with the Python keyword defaults
func ftrSearchArgs(pattern, path string, offset, limit any) searchArgs {
	return searchArgs{pattern: pattern, target: "content", path: path, limit: limit, offset: offset,
		outputMode: "content", order: "discovery"}
}

// Unsets a variable for the test and restores its prior value afterwards
func ftrUnsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// Error text of a decoded tool result, empty when absent
func ftrErr(r map[string]any) string {
	s, _ := r["error"].(string)
	return s
}

// Stores the main provider and model the session runs on
func ftrSetMain(env *Env, provider, model string) {
	env.Values.Store("runtime_main", map[string]any{"provider": provider, "model": model})
}

// Captures the cwd handed to backend creation and returns a fake env rooted there
func ftrCaptureCreate(t *testing.T, envCwd string) *[]string {
	t.Helper()
	var cwds []string
	createEnvironmentFn = func(ctx context.Context, env *Env, envType, image, cwd string, timeout int, sshConfig, containerConfig, localConfig map[string]any, taskID, hostCwd string, probeOnly bool) (Environment, error) {
		cwds = append(cwds, cwd)
		return newFakeTermEnv(envCwd, nil), nil
	}
	return &cwds
}

func TestFileToolsReadHandlerDispatchWithoutLimitUsesSchemaDefault(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	gotLimit := -1
	ops := &scriptedFileOps{readFileFn: func(path string, offset, limit int) *ReadResult {
		gotLimit = limit
		return &ReadResult{Content: "x", TotalLines: 1}
	}}
	installScriptedFileOps(t, env, ops, nil)
	handleReadFile(context.Background(), &Call{Args: map[string]any{"path": filepath.Join(ws, "test.txt")}, TaskID: "t-default", Env: env})
	props := readFileSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	want := props["limit"].(map[string]any)["default"]
	if gotLimit != want.(int) {
		t.Fatalf("limit %d, schema default %v", gotLimit, want)
	}
}

func TestFileToolsReadToolExceptionReturnsErrorJSON(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	ftrFailFileOps(t, errors.New("terminal not available"))
	r := ftDecode(t, readFileTool(context.Background(), env, filepath.Join(ws, "test.txt"), nil, nil, "default"))
	if !strings.Contains(ftrErr(r), "terminal not available") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadWritePermissionErrorReturnsErrorWithoutErrorLog(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	rec := &ftrLevelRecorder{}
	env.Log = slog.New(rec)
	ftrFailFileOps(t, ftrPermissionError{"read-only filesystem"})
	r := ftDecode(t, writeFileTool(context.Background(), env, filepath.Join(ws, "out.txt"), "data", "default", false, ""))
	if !strings.Contains(ftrErr(r), "read-only") {
		t.Fatalf("result %v", r)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, lvl := range rec.levels {
		if lvl >= slog.LevelError {
			t.Fatalf("error level record logged for an expected write denial, levels %v", rec.levels)
		}
	}
}

func TestFileToolsReadWriteRejectsReadFileLineNumberedContent(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	lookups := ftrInstallCounted(t, env, &scriptedFileOps{})
	content := " 1|setting: new_value\n 2|other: thing\n"
	r := ftDecode(t, writeFileTool(context.Background(), env, filepath.Join(ws, "config.yaml"), content, "default", false, ""))
	if !strings.Contains(strings.ToLower(ftrErr(r)), "line-number") {
		t.Fatalf("result %v", r)
	}
	if *lookups != 0 {
		t.Fatalf("file backend looked up %d times", *lookups)
	}
}

func TestFileToolsReadWriteHandlerMissingContentKey(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	r := ftDecode(t, handleWriteFile(context.Background(), &Call{Args: map[string]any{"path": filepath.Join(ws, "oops.md")}, Env: env}))
	if !strings.Contains(ftrErr(r), "content") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadWriteHandlerMissingPathKey(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	r := ftDecode(t, handleWriteFile(context.Background(), &Call{Args: map[string]any{"content": "hello"}, Env: env}))
	if _, ok := r["error"]; !ok {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadWriteHandlerExplicitEmptyContentAllowed(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	ops := &scriptedFileOps{writeFileFn: func(path, content string, preContent *string) *WriteResult {
		return &WriteResult{BytesWritten: len(content)}
	}}
	installScriptedFileOps(t, env, ops, nil)
	r := ftDecode(t, handleWriteFile(context.Background(), &Call{Args: map[string]any{"path": filepath.Join(ws, "empty.txt"), "content": ""}, Env: env}))
	if _, ok := r["error"]; ok {
		t.Fatalf("result %v", r)
	}
	if ftrCalls(ops, "WriteFile:") != 1 {
		t.Fatalf("calls %v", ops.calls)
	}
}

func TestFileToolsReadWriteHandlerNonStringContent(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	r := ftDecode(t, handleWriteFile(context.Background(), &Call{Args: map[string]any{"path": filepath.Join(ws, "x.txt"), "content": map[string]any{"nested": "dict"}}, Env: env}))
	msg := strings.ToLower(ftrErr(r))
	if msg == "" || (!strings.Contains(msg, "string") && !strings.Contains(msg, "content")) {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchUnknownModeErrors(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	installScriptedFileOps(t, env, &scriptedFileOps{}, nil)
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "invalid_mode"}, "default", nil))
	if _, ok := r["error"]; !ok {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchV4ARejectsTraversalInUpdateHeader(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ops := &scriptedFileOps{patchV4AFn: func(string, *editProbe) *PatchResult { return &PatchResult{Success: true} }}
	installScriptedFileOps(t, env, ops, nil)
	p := "*** Begin Patch\n*** Update File: ../../../etc/shadow\n@@ -1,3 +1,3 @@\n-old\n+new\n*** End Patch\n"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: p}, "default", nil))
	if !strings.Contains(strings.ToLower(ftrErr(r)), "traversal") {
		t.Fatalf("result %v", r)
	}
	if ftrCalls(ops, "patchV4A") != 0 {
		t.Fatalf("patch applied despite traversal header, calls %v", ops.calls)
	}
}

func TestFileToolsReadPatchV4ARejectsTraversalInAddHeader(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	installScriptedFileOps(t, env, &scriptedFileOps{}, nil)
	p := "*** Begin Patch\n*** Add File: ../../../tmp/dropped.py\n+print('pwned')\n*** End Patch\n"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: p}, "default", nil))
	if !strings.Contains(strings.ToLower(ftrErr(r)), "traversal") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchMoveToSensitiveDstBlocked(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	lookups := ftrInstallCounted(t, env, &scriptedFileOps{})
	p := "*** Begin Patch\n*** Move File: /tmp/work.txt -> /etc/crontab\n*** End Patch\n"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: p}, "default", nil))
	if !strings.Contains(strings.ToLower(ftrErr(r)), "sensitive") {
		t.Fatalf("result %v", r)
	}
	if *lookups != 0 {
		t.Fatalf("file backend looked up %d times", *lookups)
	}
}

func TestFileToolsReadPatchUpdateNoSpaceAfterAsterisksBlocked(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	lookups := ftrInstallCounted(t, env, &scriptedFileOps{})
	p := "*** Begin Patch\n***Update File: /etc/resolv.conf\n@@ @@\n-old\n+new\n*** End Patch\n"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: p}, "default", nil))
	if !strings.Contains(strings.ToLower(ftrErr(r)), "sensitive") {
		t.Fatalf("result %v", r)
	}
	if *lookups != 0 {
		t.Fatalf("file backend looked up %d times", *lookups)
	}
}

func TestFileToolsReadPatchMoveSafePathsNotBlocked(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	ops := &scriptedFileOps{patchV4AFn: func(string, *editProbe) *PatchResult { return &PatchResult{Success: true} }}
	installScriptedFileOps(t, env, ops, nil)
	p := "*** Begin Patch\n*** Move File: " + filepath.Join(ws, "a.txt") + " -> " + filepath.Join(ws, "b.txt") + "\n*** End Patch\n"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: p}, "default", nil))
	if _, ok := r["error"]; ok {
		t.Fatalf("result %v", r)
	}
	if ftrCalls(ops, "patchV4A") != 1 {
		t.Fatalf("calls %v", ops.calls)
	}
}

func TestFileToolsReadSearchExceptionReturnsError(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ftrFailFileOps(t, errors.New("no terminal"))
	r := ftDecode(t, searchTool(context.Background(), env, ftrSearchArgs("x", ".", 0, 50), "default"))
	if _, ok := r["error"]; !ok {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchNoMatchIncludesHint(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ops := &scriptedFileOps{patchReplaceFn: func(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult {
		return &PatchResult{Error: "Could not find match for old_string in foo.py"}
	}}
	installScriptedFileOps(t, env, ops, nil)
	x, y := "x", "y"
	r := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "replace", path: "foo.py", oldString: &x, newString: &y}, "default", nil))
	if h, _ := r["_hint"].(string); h == "" {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchSuccessNoHint(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ops := &scriptedFileOps{patchReplaceFn: func(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult {
		return &PatchResult{Success: true, Diff: "--- a\n+++ b"}
	}}
	installScriptedFileOps(t, env, ops, nil)
	x, y := "x", "y"
	raw := patchTool(context.Background(), env, patchArgs{mode: "replace", path: "foo.py", oldString: &x, newString: &y}, "default", nil)
	if strings.Contains(raw, "_hint") {
		t.Fatalf("raw %s", raw)
	}
}

func TestFileToolsReadSearchTruncatedHintWithNonzeroOffset(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	matches := make([]SearchMatch, 50)
	for i := range matches {
		matches[i] = SearchMatch{Path: "a.py", LineNumber: 1, Content: "x"}
	}
	ops := &scriptedFileOps{searchFn: func(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
		return &SearchResult{TotalCount: 150, Matches: matches, Truncated: true}
	}}
	installScriptedFileOps(t, env, ops, nil)
	r := ftDecode(t, searchTool(context.Background(), env, ftrSearchArgs("foo", ".", 50, 50), "default"))
	if h, _ := r["_hint"].(string); !strings.Contains(h, "offset=100") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadSensitiveConfigBlockedForWriteFile(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	cfg := filepath.Join(env.Home, "config.yaml")
	r := ftDecode(t, writeFileTool(context.Background(), env, cfg, "approvals:\n  mode: off\n", "default", false, ""))
	if !strings.Contains(ftrErr(r), "icebath config") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadSensitiveSystemPathStillBlocked(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	r := ftDecode(t, writeFileTool(context.Background(), env, "/etc/passwd", "evil", "default", false, ""))
	if !strings.Contains(ftrErr(r), "sensitive system path") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadSensitiveMacOSPrivateVarCarveouts(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	for path, blocked := range map[string]bool{
		"/private/var/folders/xy/T/tmp.txt": false,
		"/private/var/tmp/build.log":        false,
		"/private/var/db/secret":            true,
		"/private/var/root/x":               true,
		"/private/etc/hosts":                true,
	} {
		msg, err := checkSensitivePath(env, path, "default")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if (msg != "") != blocked {
			t.Fatalf("%s blocked=%v want %v (%q)", path, msg != "", blocked, msg)
		}
	}
}

func TestFileToolsReadSensitiveNormalFileNotBlocked(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	ops := &scriptedFileOps{writeFileFn: func(path, content string, preContent *string) *WriteResult {
		return &WriteResult{BytesWritten: len(content)}
	}}
	installScriptedFileOps(t, env, ops, nil)
	r := ftDecode(t, writeFileTool(context.Background(), env, filepath.Join(ws, "other.txt"), "hello", "default", false, ""))
	if _, ok := r["error"]; ok {
		t.Fatalf("result %v", r)
	}
	if r["bytes_written"] != float64(5) {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadPatchV4ALayerKeepsPerModeDocumentation(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ftrSetMain(env, "openai", "gpt-5.2")
	o := patchSchemaOverrides(env)
	params, _ := o["parameters"].(map[string]any)
	if params == nil {
		t.Fatalf("override %v", o)
	}
	req, _ := params["required"].([]any)
	if len(req) != 1 || req[0] != "mode" {
		t.Fatalf("required %v", params["required"])
	}
	props, _ := params["properties"].(map[string]any)
	for _, k := range []string{"mode", "patch", "path", "old_string", "new_string"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("property %s missing from %v", k, props)
		}
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if _, ok := params[k]; ok {
			t.Fatalf("%s present in %v", k, params)
		}
	}
}

func TestFileToolsReadRecordedCwdUsedForRecreatedEnv(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	envConfigFn = func(*Env) (map[string]any, error) {
		return localTestConfig("/default/path", map[string]any{"timeout": 30}), nil
	}
	cwds := ftrCaptureCreate(t, "/Users/user/project")
	RecordSessionCwd(env, "default", "/Users/user/project")
	t.Cleanup(func() { ClearSessionCwd(env, "default") })
	if _, err := getFileOps(env, "default"); err != nil {
		t.Fatal(err)
	}
	if len(*cwds) == 0 {
		t.Fatal("backend creation was not called")
	}
	if got := (*cwds)[len(*cwds)-1]; got != "/Users/user/project" {
		t.Fatalf("cwd %q", got)
	}
}

func TestFileToolsReadStaleCacheCwdRescuedIntoRecord(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ClearSessionCwd(env, "default")
	t.Cleanup(func() { ClearSessionCwd(env, "default") })
	key, err := resolveContainerTaskID(env, "default")
	if err != nil {
		t.Fatal(err)
	}
	fileOpsCache.Lock()
	fileOpsCache.m[key] = &fileOpsHandle{cwd: "/Users/user/project"}
	fileOpsCache.Unlock()
	envConfigFn = func(*Env) (map[string]any, error) {
		return localTestConfig("/config/default/path", map[string]any{"timeout": 30}), nil
	}
	cwds := ftrCaptureCreate(t, "/Users/user/project")
	if _, err := getFileOps(env, "default"); err != nil {
		t.Fatal(err)
	}
	if len(*cwds) == 0 {
		t.Fatal("backend creation was not called")
	}
	if got := (*cwds)[len(*cwds)-1]; got != "/Users/user/project" {
		t.Fatalf("restored cwd %q", got)
	}
}

func TestFileToolsReadRelativeWriteAfterEnvCleanupLandsInUserCwd(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, configDefault := filepath.Join(root, "project"), filepath.Join(root, "config_default")
	for _, d := range []string{project, configDefault} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ftrUnsetenv(t, "TERMINAL_CWD")
	envConfigFn = func(*Env) (map[string]any, error) { return localTestConfig(configDefault, nil), nil }
	ClearSessionCwd(env, "default")
	t.Cleanup(func() { ClearSessionCwd(env, "default") })
	ctx := context.Background()

	fo, err := getFileOps(env, "default")
	if err != nil {
		t.Fatal(err)
	}
	fo.terminal.SetCwd(project)
	RecordSessionCwd(env, "default", project)
	writeFileTool(ctx, env, "alive.txt", "1\n", "default", false, "")
	if _, err := os.Stat(filepath.Join(project, "alive.txt")); err != nil {
		t.Fatalf("alive.txt not in project: %v", err)
	}

	key, err := resolveContainerTaskID(env, "default")
	if err != nil {
		t.Fatal(err)
	}
	termPool.Lock()
	delete(termPool.active, key)
	delete(termPool.lastActivity, key)
	termPool.Unlock()
	fileOpsCache.Lock()
	delete(fileOpsCache.m, key)
	fileOpsCache.Unlock()

	r := ftDecode(t, writeFileTool(ctx, env, "report.txt", "hello\n", "default", false, ""))
	if r["resolved_path"] != filepath.Join(project, "report.txt") {
		t.Fatalf("result %v", r)
	}
	if _, err := os.Stat(filepath.Join(project, "report.txt")); err != nil {
		t.Fatalf("file should be in the user's cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDefault, "report.txt")); err == nil {
		t.Fatal("file silently misplaced into config default")
	}
}

func TestFileToolsReadInvalidateEvictsTaskResolvedKey(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, proc := filepath.Join(root, "workspace"), filepath.Join(root, "proc")
	for _, d := range []string{workspace, proc} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ftrUnsetenv(t, "TERMINAL_CWD")
	t.Chdir(proc)
	env.Cwd = proc
	taskID := "acp-dedup"
	if err := RegisterTaskEnvOverrides(env, taskID, map[string]any{"cwd": workspace}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "data.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	correct, err := resolvePathForTaskID(env, taskID, "data.txt")
	if err != nil {
		t.Fatal(err)
	}
	buggy, err := resolvePathForTaskID(env, "default", "data.txt")
	if err != nil {
		t.Fatal(err)
	}
	if correct == buggy {
		t.Fatalf("test precondition, cwds must diverge, both %s", correct)
	}
	dedupPaths := func() []string {
		readTrackerLock.Lock()
		defer readTrackerLock.Unlock()
		var out []string
		if td := readTracker[taskID]; td != nil {
			for _, k := range td.dedup.keys() {
				out = append(out, k.path)
			}
		}
		return out
	}
	readFileTool(context.Background(), env, "data.txt", nil, nil, taskID)
	if keys := dedupPaths(); !inList(keys, correct) {
		t.Fatalf("dedup keys %v lack %s", keys, correct)
	}
	if err := updateReadTimestamp(env, "data.txt", taskID); err != nil {
		t.Fatal(err)
	}
	if keys := dedupPaths(); inList(keys, correct) {
		t.Fatalf("dedup keys %v still hold %s", keys, correct)
	}
}

func TestFileToolsReadNotFoundCachedAndSkipsRetry(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "does-not-exist-neg-1.txt")
	ops := &scriptedFileOps{readFileFn: func(string, int, int) *ReadResult {
		return &ReadResult{Error: "File not found: " + p, SimilarFiles: []string{}}
	}}
	installScriptedFileOps(t, env, ops, nil)
	tid := "neg-cache-read-1"
	first := ftDecode(t, readFileTool(context.Background(), env, p, nil, nil, tid))
	if !strings.Contains(ftrErr(first), "File not found") || ftrCalls(ops, "readFile:") != 1 {
		t.Fatalf("first %v calls %v", first, ops.calls)
	}
	second := ftDecode(t, readFileTool(context.Background(), env, p, nil, nil, tid))
	if !strings.Contains(ftrErr(second), "File not found") {
		t.Fatalf("second %v", second)
	}
	if ftrCalls(ops, "readFile:") != 1 {
		t.Fatalf("negative cache hit must skip the backend on retry, calls %v", ops.calls)
	}
}

func TestFileToolsReadNotFoundCacheIsolatedPerTask(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "does-not-exist-neg-2.txt")
	ops := &scriptedFileOps{readFileFn: func(string, int, int) *ReadResult {
		return &ReadResult{Error: "File not found: " + p, SimilarFiles: []string{}}
	}}
	installScriptedFileOps(t, env, ops, nil)
	readFileTool(context.Background(), env, p, nil, nil, "neg-cache-iso-A")
	readFileTool(context.Background(), env, p, nil, nil, "neg-cache-iso-B")
	if ftrCalls(ops, "readFile:") != 2 {
		t.Fatalf("calls %v", ops.calls)
	}
}

func TestFileToolsReadSearchNotFoundCachedAndSkipsRetry(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "does-not-exist-search-3")
	ops := &scriptedFileOps{searchFn: func(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
		return &SearchResult{Error: "Path not found: " + p}
	}}
	installScriptedFileOps(t, env, ops, nil)
	tid := "neg-cache-search-3"
	first := ftDecode(t, searchTool(context.Background(), env, ftrSearchArgs("foo", p, 0, 50), tid))
	if !strings.Contains(ftrErr(first), "Path not found") || ftrCalls(ops, "search:") != 1 {
		t.Fatalf("first %v calls %v", first, ops.calls)
	}
	second := ftDecode(t, searchTool(context.Background(), env, ftrSearchArgs("foo", p, 0, 50), tid))
	if !strings.Contains(ftrErr(second), "Path not found") {
		t.Fatalf("second %v", second)
	}
	if ftrCalls(ops, "search:") != 1 {
		t.Fatalf("search negative cache hit must skip the backend on retry, calls %v", ops.calls)
	}
}

func TestFileToolsReadNotFoundCachesNamespaced(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "does-not-exist-namespace-4")
	ops := &scriptedFileOps{
		readFileFn: func(string, int, int) *ReadResult { return &ReadResult{Error: "File not found: " + p} },
		searchFn: func(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
			return &SearchResult{Error: "Path not found: " + p}
		},
	}
	installScriptedFileOps(t, env, ops, nil)
	tid := "neg-cache-namespace-4"
	readFileTool(context.Background(), env, p, nil, nil, tid)
	searchTool(context.Background(), env, ftrSearchArgs("foo", p, 0, 50), tid)
	if ftrCalls(ops, "readFile:") != 1 || ftrCalls(ops, "search:") != 1 {
		t.Fatalf("calls %v", ops.calls)
	}
}

func TestFileToolsReadWriteInvalidatesReadNegativeCache(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	p := filepath.Join(ws, "will-be-created-neg-5.txt")
	reads := 0
	ops := &scriptedFileOps{
		readFileFn: func(string, int, int) *ReadResult {
			reads++
			if reads == 1 {
				return &ReadResult{Error: "File not found: " + p}
			}
			return &ReadResult{Content: "after write", TotalLines: 1}
		},
		writeFileFn: func(path, content string, preContent *string) *WriteResult {
			return &WriteResult{BytesWritten: len(content)}
		},
	}
	installScriptedFileOps(t, env, ops, nil)
	tid := "neg-cache-write-invalidate-5"
	first := ftDecode(t, readFileTool(context.Background(), env, p, nil, nil, tid))
	if !strings.Contains(ftrErr(first), "File not found") {
		t.Fatalf("first %v", first)
	}
	writeFileTool(context.Background(), env, p, "after write", tid, false, "")
	second := ftDecode(t, readFileTool(context.Background(), env, p, nil, nil, tid))
	if second["content"] != "after write" {
		t.Fatalf("write_file must invalidate the negative cache, second %v", second)
	}
	if ftrCalls(ops, "readFile:") != 2 {
		t.Fatalf("calls %v", ops.calls)
	}
}

// Points HOME at a fresh directory and returns its SSH client config path
func ftrSSHConfig(t *testing.T) (string, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	return h, filepath.Join(h, ".ssh", "config")
}

func TestFileToolsReadSSHConfigNoHumanBlocksAndWritesNothing(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	_, sshConfig := ftrSSHConfig(t)
	r := ftDecode(t, writeFileTool(context.Background(), env, sshConfig, "Host x\n  ProxyCommand evil\n", "default", false, ""))
	if !strings.Contains(ftrErr(r), "BLOCKED") {
		t.Fatalf("result %v", r)
	}
	if _, err := os.Stat(sshConfig); err == nil {
		t.Fatal("ssh config was written")
	}
}

func TestFileToolsReadSSHConfigSingleQueryDeniesWithQModeMessage(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	_, sshConfig := ftrSSHConfig(t)
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	t.Setenv("ICEBATH_INTERACTIVE", "1")
	r := ftDecode(t, writeFileTool(context.Background(), env, sshConfig, "Host x\n  ProxyCommand evil\n", "default", false, ""))
	msg := ftrErr(r)
	if !strings.Contains(msg, "BLOCKED") || !strings.Contains(msg, "single-query") {
		t.Fatalf("result %v", r)
	}
	if _, err := os.Stat(sshConfig); err == nil {
		t.Fatal("ssh config was written")
	}
}

func TestFileToolsReadSSHConfigRelativePathGatedFromTaskCwd(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	h, sshConfig := ftrSSHConfig(t)
	ws, elsewhere := filepath.Join(h, "ws"), filepath.Join(h, "elsewhere")
	for _, d := range []string{ws, elsewhere, filepath.Dir(sshConfig)} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Dir(sshConfig), filepath.Join(ws, "sshdir")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_CWD", ws)
	t.Chdir(elsewhere)
	env.Cwd = elsewhere
	msg, err := checkApprovalRequiredWrite(context.Background(), env, []string{"sshdir/config"}, "ssh-gate-task")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "BLOCKED") {
		t.Fatalf("relative ssh config write not gated, %q", msg)
	}
}

const ftrSynthToken = "3JcQ1UqZ8mNp4Rt6vWx2Yb9Ad0Ef7Gh5Ij2kS"

// Scripted ops whose reads return the given rendered body
func ftrReadOps(body string) *scriptedFileOps {
	return &scriptedFileOps{readFileFn: func(string, int, int) *ReadResult {
		return &ReadResult{Content: body, TotalLines: strings.Count(body, "\n")}
	}}
}

func TestFileToolsReadSecretConfigMasksOpaqueToken(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	body := "1|mcp_servers:\n2|  nasa_ads:\n3|    env:\n" +
		"4|      ADS_API_TOKEN: " + ftrSynthToken + "\n5|MAX_TOKENS: 100\n"
	ops := ftrReadOps(body)
	installScriptedFileOps(t, env, ops, nil)
	out := ftDecode(t, readFileTool(context.Background(), env, filepath.Join(env.Home, "config.yaml"), nil, nil, "secret-read"))
	content, _ := out["content"].(string)
	if strings.Contains(content, ftrSynthToken) || !strings.Contains(content, "\u00abredacted") {
		t.Fatalf("content %q", content)
	}
	if !strings.Contains(content, "5|MAX_TOKENS: 100") {
		t.Fatalf("non secret scalar and gutter lost, content %q", content)
	}
	ops.readFileFn = ftrReadOps("4|      ADS_API_TOKEN: " + ftrSynthToken + "\n").readFileFn
	out = ftDecode(t, readFileTool(context.Background(), env, filepath.Join(filepath.Dir(env.Home), "proj-config.yaml"), nil, nil, "plain-read"))
	if content, _ := out["content"].(string); !strings.Contains(content, ftrSynthToken) {
		t.Fatalf("project config mangled, result %v", out)
	}
}

func TestFileToolsReadSearchInHomeMasksOpaqueToken(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	cfg := filepath.Join(env.Home, "config.yaml")
	ops := &scriptedFileOps{searchFn: func(pattern, path, target, fileGlob string, limit, offset int, outputMode string, context int, order string) *SearchResult {
		return &SearchResult{TotalCount: 1, Matches: []SearchMatch{{Path: cfg, Content: "      ADS_API_TOKEN: " + ftrSynthToken}}}
	}}
	installScriptedFileOps(t, env, ops, nil)
	raw := searchTool(context.Background(), env, ftrSearchArgs("ADS_API_TOKEN", env.Home, 0, 50), "secret-search")
	if strings.Contains(raw, ftrSynthToken) || !strings.Contains(raw, "\u00abredacted") {
		t.Fatalf("raw %s", raw)
	}
}

func TestFileToolsReadConflictMarkerFlagsBalancedBlocksOnly(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	conflicted := filepath.Join(ws, "c.py")
	if err := os.WriteFile(conflicted, []byte("x=1\n<<<<<<< HEAD\ny=2\n=======\ny=3\n>>>>>>> feature\nz=4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ftDecode(t, readFileTool(context.Background(), env, conflicted, nil, nil, "default"))
	if r["conflict_blocks"] != float64(1) {
		t.Fatalf("result %v", r)
	}
	if h, _ := r["_hint"].(string); !strings.Contains(h, "merge-conflict") {
		t.Fatalf("result %v", r)
	}
	prose := filepath.Join(ws, "p.py")
	if err := os.WriteFile(prose, []byte("print('<<<<<<< not a conflict')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := ftDecode(t, readFileTool(context.Background(), env, prose, nil, nil, "default")); r["conflict_blocks"] != nil {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadSpecialFileKindRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k := specialFileKind(p); k != "" {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindDirectory(t *testing.T) {
	if k := specialFileKind(t.TempDir()); k != "" {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindMissingPath(t *testing.T) {
	if k := specialFileKind(filepath.Join(t.TempDir(), "nope")); k != "" {
		t.Fatalf("kind %q", k)
	}
}

// Makes a named pipe at the path
func ftrMkfifo(t *testing.T, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileToolsReadSpecialFileKindFifo(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "p.pipe")
	ftrMkfifo(t, fifo)
	if k := specialFileKind(fifo); !strings.Contains(k, "FIFO") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if k := specialFileKind(sockPath); !strings.Contains(k, "socket") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindSymlinkToFifoFollowed(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "p.pipe")
	ftrMkfifo(t, fifo)
	link := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	if k := specialFileKind(link); !strings.Contains(k, "FIFO") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindCharDevice(t *testing.T) {
	if k := specialFileKind("/dev/null"); !strings.Contains(k, "character device") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadFifoReadReturnsNoteInstantly(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	fifo := filepath.Join(ws, "live.pipe")
	ftrMkfifo(t, fifo)
	done := make(chan string, 1)
	go func() { done <- readFileTool(context.Background(), env, fifo, nil, nil, "default") }()
	var raw string
	select {
	case raw = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guard must not block on the FIFO")
	}
	r := ftDecode(t, raw)
	if r["success"] != false {
		t.Fatalf("result %v", r)
	}
	if note, _ := r["note"].(string); !strings.Contains(note, "FIFO") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadRegularFileUnaffectedByFifoGuard(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	f := filepath.Join(ws, "ok.txt")
	if err := os.WriteFile(f, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ftDecode(t, readFileTool(context.Background(), env, f, nil, nil, "default"))
	if r["success"] == false {
		t.Fatalf("result %v", r)
	}
	if c, _ := r["content"].(string); !strings.Contains(c, "alpha") {
		t.Fatalf("result %v", r)
	}
}

func TestFileToolsReadSchemaHostedOCRGateStates(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	base := readFileSchema["description"].(string)
	upgraded := strings.Replace(base, "PDF (text layer)", "PDF (scanned or text)", 1)
	check := func(state string, want bool) {
		t.Helper()
		if got := HostedOCRAvailable(env); got != want {
			t.Fatalf("%s available %v want %v", state, got, want)
		}
		o := readFileSchemaOverrides(env)
		if !want {
			if len(o) != 0 {
				t.Fatalf("%s override %v", state, o)
			}
			return
		}
		if o["description"] != upgraded || len(o) != 1 {
			t.Fatalf("%s override %v", state, o)
		}
	}
	t.Setenv("FIRECRAWL_API_KEY", "fc-x")
	env.Config = config.Tree{}
	check("direct key", true)
	env.Config = config.Tree{"file_tools": map[string]any{"hosted_ocr": false}}
	check("config false beats key", false)
	os.Unsetenv("FIRECRAWL_API_KEY")
	env.Config = config.Tree{"file_tools": map[string]any{"hosted_ocr": true}}
	check("config true without key", false)
	env.Config = config.Tree{}
	check("nothing", false)
}

// About forty thousand characters of distinct numbered lines
func ftrBig(nLines int) string {
	var b strings.Builder
	for i := 0; i < nLines; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "line %d: %s", i, strings.Repeat("x", 40))
	}
	b.WriteByte('\n')
	return b.String()
}

func TestFileToolsReadRewriteOfLargeFileWithFewChangesGetsPatchHint(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	f := filepath.Join(ws, "mod.py")
	old := ftrBig(800)
	if err := os.WriteFile(f, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	nw := strings.ReplaceAll(strings.ReplaceAll(old, "line 400:", "line 400 (edited):"), "line 401:", "line 401 (edited):")
	if r := ftDecode(t, readFileTool(context.Background(), env, f, nil, nil, "t")); r["error"] != nil {
		t.Fatalf("read %v", r)
	}
	r := ftDecode(t, writeFileTool(context.Background(), env, f, nw, "t", false, ""))
	if r["error"] != nil {
		t.Fatalf("write %v", r)
	}
	if b, _ := os.ReadFile(f); string(b) != nw {
		t.Fatal("the write did not happen")
	}
	if h, _ := r["hint"].(string); h == "" {
		t.Fatalf("no rewrite hint, result %v", r)
	}
}

func TestFileToolsReadNewFilesSmallFilesAndRealRewritesGetNoHint(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	ctx := context.Background()
	newFile := filepath.Join(ws, "new.py")
	if r := ftDecode(t, writeFileTool(ctx, env, newFile, ftrBig(800), "t", false, "")); r["hint"] != nil || r["error"] != nil {
		t.Fatalf("new file %v", r)
	}
	small := filepath.Join(ws, "small.py")
	if err := os.WriteFile(small, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readFileTool(ctx, env, small, nil, nil, "t")
	if r := ftDecode(t, writeFileTool(ctx, env, small, "a\nc\n", "t", false, "")); r["hint"] != nil || r["error"] != nil {
		t.Fatalf("small file %v", r)
	}
	big := filepath.Join(ws, "big.py")
	if err := os.WriteFile(big, []byte(ftrBig(800)), 0o644); err != nil {
		t.Fatal(err)
	}
	readFileTool(ctx, env, big, nil, nil, "t")
	var b strings.Builder
	for i := 0; i < 800; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "other %d: %s", i, strings.Repeat("y", 40))
	}
	b.WriteByte('\n')
	if r := ftDecode(t, writeFileTool(ctx, env, big, b.String(), "t", false, "")); r["hint"] != nil || r["error"] != nil {
		t.Fatalf("real rewrite %v", r)
	}
}

func TestFileToolsReadV4AFamilyDetector(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	for _, c := range []struct {
		prov, model string
		want        bool
	}{
		{"openai", "gpt-5.2", true},
		{"openai-codex", "codex-large", true},
		{"azure-openai", "deploy-x", true},
		{"openrouter", "openai/gpt-5.2", true},
		{"nous", "openai/o5-mini", true},
		{"openrouter", "anthropic/claude-sonnet-4", false},
		{"anthropic", "claude-fable-5", false},
		{"nous", "hermes-4-405b", false},
		{"", "", false},
	} {
		ftrSetMain(env, c.prov, c.model)
		if got := isOpenAIFamilyMain(env); got != c.want {
			t.Fatalf("(%q, %q) family %v want %v", c.prov, c.model, got, c.want)
		}
	}
}

func TestFileToolsReadV4AOpenAIFamilyGetsLayer(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ftrSetMain(env, "openai", "gpt-5.2")
	params, _ := patchSchemaOverrides(env)["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	for _, k := range []string{"mode", "patch"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("property %s missing from %v", k, props)
		}
	}
}

func TestFileToolsReadV4ANonOpenAIGetsNoOverride(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	ftrSetMain(env, "anthropic", "claude-fable-5")
	if o := patchSchemaOverrides(env); len(o) != 0 {
		t.Fatalf("override %v", o)
	}
}

func TestFileToolsReadV4AHandlerAcceptsBothShapesRegardless(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	f1 := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f1, []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := ftDecode(t, handlePatch(ctx, &Call{Args: map[string]any{"path": f1, "old_string": "beta", "new_string": "B"}, Env: env}))
	if truthy(r["error"]) {
		t.Fatalf("replace without mode %v", r)
	}
	v4a := "*** Begin Patch\n*** Update File: " + f1 + "\n@@\n-alpha B\n+A B\n*** End Patch"
	r = ftDecode(t, handlePatch(ctx, &Call{Args: map[string]any{"mode": "patch", "patch": v4a}, Env: env}))
	if truthy(r["error"]) {
		t.Fatalf("v4a %v", r)
	}
	b, err := os.ReadFile(f1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "A B" {
		t.Fatalf("disk %q", b)
	}
}
