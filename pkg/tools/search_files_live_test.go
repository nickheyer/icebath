package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Host backend at dir running real bash, rg hidden from command lookup when noRg
func searchHostEnv(dir string, noRg bool, intercept func(string) (*ExecResult, bool)) *fakeTermEnv {
	return &fakeTermEnv{envType: "local", cwd: dir, execute: bashExecute(false, func(c string) (*ExecResult, bool) {
		if noRg && strings.HasPrefix(c, "command -v rg") {
			return &ExecResult{ReturnCode: 1}, true
		}
		if intercept != nil {
			return intercept(c)
		}
		return nil, false
	})}
}

// File backend over a real host bash env at dir
func hostSearchOps(t *testing.T, dir string, noRg bool) (*shellFileOps, *fakeTermEnv) {
	t.Helper()
	installTestEnvHooks(t)
	env := searchHostEnv(dir, noRg, nil)
	return newShellFileOps(remoteTestEnv(t), env, dir), env
}

// Skips unless ripgrep is on PATH
func requireRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
}

// Skips on hosts that cannot model unreadable directories
func requireUnreadableDirs(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads chmod 000 dirs")
	}
}

// Fresh dir under the first tempfile root lacking dot named and pruned components that rg globs and grep exclude
func searchableTempDir(t *testing.T) string {
	t.Helper()
	for _, base := range pypath.TempdirCandidates() {
		abs, err := filepath.Abs(base)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil || slices.ContainsFunc(strings.Split(filepath.ToSlash(real), "/"), func(part string) bool {
			return SearchPruneDirNames[part] || (strings.HasPrefix(part, ".") && part != "." && part != "..")
		}) {
			continue
		}
		dir, err := os.MkdirTemp(real, "icebath-search-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	t.Fatalf("no temp root in %q is free of dot named and pruned directory components", pypath.TempdirCandidates())
	return ""
}

// Makes a directory unreadable until the test ends
func lockSearchDir(t *testing.T, p string) {
	t.Helper()
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o755) })
}

// Writes text to a file, creating its parent directories
func writeSearchFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Paths of the matches in order
func matchPaths(r *SearchResult) []string {
	var out []string
	for _, m := range r.Matches {
		out = append(out, m.Path)
	}
	return out
}

// Result dict with match and file lists sorted, for transport parity checks
func normalizedSearchDict(t *testing.T, r *SearchResult) string {
	t.Helper()
	d := searchResultDict(t, r)
	for _, key := range []string{"matches", "files"} {
		items, ok := d[key].([]any)
		if !ok {
			continue
		}
		keys := make([]string, len(items))
		for i, it := range items {
			b, _ := json.Marshal(it)
			keys[i] = string(b)
		}
		sort.Strings(keys)
		d[key] = keys
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// Native rg tree fixture
func nativeSearchTree(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	writeSearchFile(t, filepath.Join(tree, "a.py"), "needle one\nplain\nneedle two\n")
	writeSearchFile(t, filepath.Join(tree, "b.txt"), "needle three\n")
	writeSearchFile(t, filepath.Join(tree, "sub", "c.py"), "needle four\n")
	return tree
}

func TestSearchNativeLaneMatchesTheShellLane(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native rg lane is POSIX only")
	}
	tree := nativeSearchTree(t)
	if probe, _ := hostSearchOps(t, tree, false); !probe.hasCommand("rg") {
		t.Skip("native search parity requires ripgrep in the search environment")
	}
	type tc struct{ pattern, path, target, fileGlob, outputMode string }
	cases := []tc{
		{"needle", tree, "content", "", "content"},
		{"needle", tree, "content", "", "count"},
		{"needle", tree, "content", "*.py", "files_only"},
		{"NEEDLE_NOPE", tree, "content", "", "content"},
		{"*.py", tree, "files", "", "content"},
		{"needle", filepath.Join(tree, "missing"), "content", "", "content"},
	}
	for _, c := range cases {
		t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
		shellOps, _ := hostSearchOps(t, tree, false)
		shell := normalizedSearchDict(t, shellOps.search(c.pattern, c.path, c.target, c.fileGlob, 50, 0, c.outputMode, 0, "discovery"))
		t.Setenv("ICEBATH_NATIVE_FILE_READ", "1")
		nativeOps, env := hostSearchOps(t, tree, false)
		native := normalizedSearchDict(t, nativeOps.search(c.pattern, c.path, c.target, c.fileGlob, 50, 0, c.outputMode, 0, "discovery"))
		if native != shell {
			t.Fatalf("%+v\nnative %s\nshell  %s", c, native, shell)
		}
		for _, cmd := range envCommands(env) {
			if strings.Contains(cmd, "pipefail") || strings.HasPrefix(cmd, "test -e") {
				t.Fatalf("%+v native lane used the shell: %q", c, cmd)
			}
		}
	}
}

func TestSearchNativeRunnerHonoursDeadlineAndInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native rg lane is POSIX only")
	}
	ops, _ := hostSearchOps(t, nativeSearchTree(t), false)
	started := time.Now()
	r := ops.runRgNative([]string{"sh", "-c", "'sleep 30'"}, 5, 1, false)
	if r.ExitCode != 124 || time.Since(started) >= 5*time.Second || !strings.HasSuffix(r.Stdout, "[Command timed out after 1s]") {
		t.Fatalf("deadline %+v after %v", r, time.Since(started))
	}
	id := NewWorkerID()
	t.Cleanup(func() { SetInterrupt(id, false, "") })
	worker := ops.withContext(WithWorker(context.Background(), id))
	time.AfterFunc(300*time.Millisecond, func() { SetInterrupt(id, true, "") })
	started = time.Now()
	r = worker.runRgNative([]string{"sh", "-c", "'sleep 30'"}, 5, 30, false)
	if r.ExitCode != 130 || time.Since(started) >= 5*time.Second || !strings.HasSuffix(r.Stdout, "[Command interrupted]") {
		t.Fatalf("interrupt %+v after %v", r, time.Since(started))
	}
}

func TestSearchNativeRunnerSpawnFailuresUseOSErrorText(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native rg lane is POSIX only")
	}
	ops, _ := hostSearchOps(t, t.TempDir(), false)
	r := ops.runRgNative([]string{"definitely-not-a-binary-xyz"}, 5, 5, false)
	if r.ExitCode != 2 || r.Stdout != "rg: [Errno 2] No such file or directory: 'definitely-not-a-binary-xyz'" {
		t.Fatalf("%+v", r)
	}
}

func TestSearchKillSwitchRoutesSearchBackToTheShell(t *testing.T) {
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	ops, env := hostSearchOps(t, nativeSearchTree(t), false)
	r := ops.search("needle", ops.cwd, "content", "", 50, 0, "content", 0, "discovery")
	var probe, pipeline bool
	for _, c := range envCommands(env) {
		probe = probe || strings.HasPrefix(c, "test -e")
		pipeline = pipeline || strings.HasPrefix(c, "set -o pipefail; ")
	}
	if r.TotalCount != 4 || !probe || !pipeline {
		t.Fatalf("total %d probe %v pipeline %v", r.TotalCount, probe, pipeline)
	}
}

func TestSearchLimitHitKeepsDrainedMatches(t *testing.T) {
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "1")
	ops, _ := hostSearchOps(t, nativeSearchTree(t), false)
	r := ops.search("needle", ops.cwd, "content", "", 2, 0, "content", 0, "discovery")
	if r.Error != "" || len(r.Matches) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestSearchLocalSearchRunsThePinnedRipgrep(t *testing.T) {
	tmp := t.TempDir()
	pinned := filepath.Join(tmp, "store", "ripgrep", "rg")
	writeSearchFile(t, pinned, "#!/bin/sh\n")
	prev := pmRipgrepBinary
	pmRipgrepBinary = func() (string, bool) { return pinned, true }
	t.Cleanup(func() { pmRipgrepBinary = prev })
	ops, env := hostSearchOps(t, tmp, false)
	if got := ops.resolveCommand("rg"); got != strings.ReplaceAll(pinned, `\`, "/") {
		t.Fatalf("resolved %q", got)
	}
	if len(envCommands(env)) != 0 {
		t.Fatalf("probed the terminal PATH %q", envCommands(env))
	}
}

// Zero match fixture with a project and an extra root
func zeroMatchProj(t *testing.T) string {
	t.Helper()
	tmp := searchableTempDir(t)
	writeSearchFile(t, filepath.Join(tmp, "proj", "a.py"), "TOKEN_ALPHA = 'find_me_value'\nother = 1\n")
	writeSearchFile(t, filepath.Join(tmp, "proj", "b.py"), "x = compute(TOKEN_ALPHA)\n")
	writeSearchFile(t, filepath.Join(tmp, "extra", "c.txt"), "TOKEN_ALPHA appears here too\n")
	return tmp
}

// Runs the search_files tool layer and decodes its result
func runSearchTool(t *testing.T, env *Env, a searchArgs, taskID string) map[string]any {
	t.Helper()
	if a.target == "" {
		a.target = "content"
	}
	if a.outputMode == "" {
		a.outputMode = "content"
	}
	if a.order == "" {
		a.order = "discovery"
	}
	if a.limit == nil {
		a.limit = 50
	}
	if a.offset == nil {
		a.offset = 0
	}
	return ftDecode(t, searchTool(context.Background(), env, a, taskID))
}

// Routes the tool layer to one prepared file backend
func installSearchHandle(t *testing.T, ops *shellFileOps) {
	t.Helper()
	prev := getFileOps
	getFileOps = func(*Env, string) (*fileOpsHandle, error) {
		return &fileOpsHandle{ops: ops, terminal: ops.terminalEnv, cwd: ops.cwd, bind: func(ctx context.Context) fileToolOps { return ops.withContext(ctx) }}, nil
	}
	t.Cleanup(func() { getFileOps = prev })
}

// Warning text of a decoded tool result
func toolWarning(r map[string]any) string {
	w, _ := r["warning"].(string)
	return w
}

// Total count of a decoded tool result
func toolTotal(r map[string]any) int {
	n, _ := r["total_count"].(float64)
	return int(n)
}

func TestSearchZeroMatchProbeHints(t *testing.T) {
	requireRg(t)
	env, _ := newFileToolTestEnv(t)
	tmp := zeroMatchProj(t)
	d := filepath.Join(tmp, "proj")
	r := runSearchTool(t, env, searchArgs{pattern: "token_alpha", path: d}, "t-zm-case")
	if w := toolWarning(r); !strings.Contains(w, "a.py") || !strings.Contains(w, "b.py") {
		t.Fatalf("case hint %q", w)
	}
	writeSearchFile(t, filepath.Join(d, "meta.py"), "result = lookup[key+1]\n")
	r = runSearchTool(t, env, searchArgs{pattern: "lookup[key+1]", path: d}, "t-zm-meta")
	if w := toolWarning(r); toolTotal(r) != 0 || !strings.Contains(w, "literal match") || !strings.Contains(w, "meta.py") {
		t.Fatalf("literal hint %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "zzz_totally_absent_zzz", path: d}, "t-zm-zero")
	if _, has := r["warning"]; toolTotal(r) != 0 || has {
		t.Fatalf("true zero %v", r)
	}
	writeSearchFile(t, filepath.Join(d, ".secretdir", "conf.cfg"), "HIDDEN_ONLY_TOKEN = true\n")
	r = runSearchTool(t, env, searchArgs{pattern: "HIDDEN_ONLY_TOKEN", path: d}, "t-zm-hidden")
	if w := toolWarning(r); toolTotal(r) != 0 || !strings.Contains(w, "hidden or gitignored") || !strings.Contains(w, "conf.cfg") {
		t.Fatalf("hidden hint %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: d}, "t-zm-hit")
	if _, has := r["warning"]; toolTotal(r) < 2 || has {
		t.Fatalf("matching search %v", r)
	}
}

func TestSearchZeroMatchHiddenProbePrunesDependencyTrees(t *testing.T) {
	requireRg(t)
	env, _ := newFileToolTestEnv(t)
	d := filepath.Join(zeroMatchProj(t), "proj")
	writeSearchFile(t, filepath.Join(d, "node_modules", "package", ".hidden", "dependency.js"), "BOUNDED_HIDDEN_TOKEN = true\n")
	writeSearchFile(t, filepath.Join(d, ".project-local", "settings.cfg"), "BOUNDED_HIDDEN_TOKEN = true\n")
	writeSearchFile(t, filepath.Join(d, ".gitignore"), "node_modules/\n.project-local/\n")
	r := runSearchTool(t, env, searchArgs{pattern: "BOUNDED_HIDDEN_TOKEN", path: d}, "t-zm-pruned-hidden")
	w := toolWarning(r)
	if toolTotal(r) != 0 || !strings.Contains(w, "hidden or gitignored") || !strings.Contains(w, "settings.cfg") || strings.Contains(w, "dependency.js") {
		t.Fatalf("%v", r)
	}
	writeSearchFile(t, filepath.Join(d, "node_modules", "package", ".hidden", "explicit.js"), "EXPLICIT_ROOT_TOKEN = true\n")
	r = runSearchTool(t, env, searchArgs{pattern: "EXPLICIT_ROOT_TOKEN", path: filepath.Join(d, "node_modules")}, "t-zm-explicit-pruned-root")
	if _, has := r["warning"]; toolTotal(r) != 0 || has {
		t.Fatalf("explicit root %v", r)
	}
}

func TestSearchMultiPathRecovery(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	tmp := zeroMatchProj(t)
	proj, extra := filepath.Join(tmp, "proj"), filepath.Join(tmp, "extra")
	r := runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: proj + " " + extra}, "t-mp-two")
	blob, _ := json.Marshal(r)
	if _, has := r["error"]; has || toolTotal(r) < 3 || !strings.Contains(string(blob), "a.py") || !strings.Contains(string(blob), "c.txt") ||
		!(strings.Contains(toolWarning(r), "2 entries") || strings.Contains(toolWarning(r), "searched 2")) {
		t.Fatalf("two roots %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: proj + " " + filepath.Join(tmp, "nonexistent_dir")}, "t-mp-missing")
	if _, has := r["error"]; has || toolTotal(r) < 2 || !strings.Contains(toolWarning(r), "skipped missing") {
		t.Fatalf("missing %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: proj + "," + extra}, "t-mp-comma")
	if _, has := r["error"]; has || toolTotal(r) < 3 {
		t.Fatalf("comma %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: filepath.Join(tmp, "gone1") + " " + filepath.Join(tmp, "gone2")}, "t-mp-gone")
	if _, has := r["error"]; !has {
		t.Fatalf("all missing %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "TOKEN_ALPHA", path: filepath.Join(tmp, "pro")}, "t-mp-similar")
	if e, _ := r["error"].(string); !strings.Contains(e, "Similar paths: "+proj) {
		t.Fatalf("single missing %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "*.py", target: "files", path: proj + " " + extra}, "t-mp-files")
	blob, _ = json.Marshal(r)
	if _, has := r["error"]; has || !strings.Contains(string(blob), "a.py") {
		t.Fatalf("files %v", r)
	}
}

// Host backend whose count probes answer a sentinel, rg hidden for the first lookups or pinned to realRg
func sentinelProbeOps(t *testing.T, dir string, hideRgLookups int, realRg string) *shellFileOps {
	t.Helper()
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	var lookups atomic.Int32
	env := searchHostEnv(dir, false, func(c string) (*ExecResult, bool) {
		switch {
		case strings.HasPrefix(c, "command -v rg"):
			if int(lookups.Add(1)) <= hideRgLookups {
				return &ExecResult{ReturnCode: 1}, true
			}
			return &ExecResult{Output: "/sentinel/rg\n"}, true
		case strings.Contains(c, "--count-matches"):
			return &ExecResult{Output: "SENTINEL_HINT.py:1\n"}, true
		}
		return nil, false
	})
	installTestEnvHooks(t)
	ops := newShellFileOps(remoteTestEnv(t), env, dir)
	if realRg != "" {
		ops.cacheMu.Lock()
		ops.rgResolutionCache["rg"] = realRg
		ops.cacheMu.Unlock()
	}
	return ops
}

func TestSearchZeroMatchHintIsWiredOnEachEngine(t *testing.T) {
	proj := filepath.Join(zeroMatchProj(t), "proj")
	t.Run("rg", func(t *testing.T) {
		ops := sentinelProbeOps(t, proj, 0, mustLookPath(t, "rg"))
		r := ops.search("token_alpha", proj, "content", "", 50, 0, "content", 0, "discovery")
		if r.TotalCount != 0 || !strings.Contains(r.Warning, "SENTINEL_HINT") {
			t.Fatalf("zero-match hint not wired on the rg path: %+v", r)
		}
	})
	t.Run("grep", func(t *testing.T) {
		mustLookPath(t, "grep")
		ops := sentinelProbeOps(t, proj, 1, "")
		r := ops.search("token_alpha", proj, "content", "", 50, 0, "content", 0, "discovery")
		if r.TotalCount != 0 || !strings.Contains(r.Warning, "SENTINEL_HINT") {
			t.Fatalf("zero-match hint not wired on the grep path: %+v", r)
		}
	})
	t.Run("matches", func(t *testing.T) {
		var ops *shellFileOps
		if rg, err := exec.LookPath("rg"); err == nil {
			ops = sentinelProbeOps(t, proj, 0, rg)
		} else {
			ops = sentinelProbeOps(t, proj, 1<<30, "")
		}
		if r := ops.search("TOKEN_ALPHA", proj, "content", "", 50, 0, "content", 0, "discovery"); r.TotalCount == 0 || strings.Contains(r.Warning, "SENTINEL_HINT") {
			t.Fatalf("hint attached despite matches: %+v", r)
		}
	})
	t.Run("rg skips the line oriented note", func(t *testing.T) {
		requireRg(t)
		plain, _ := hostSearchOps(t, proj, false)
		if r := plain.search(`TOKEN_ALPHA\nother`, proj, "content", "", 50, 0, "content", 0, "discovery"); strings.Contains(r.Warning, "line-oriented") {
			t.Fatalf("rg lane attached the grep newline note: %q", r.Warning)
		}
	})
}

// Absolute path of a binary on PATH
func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip(name + " not installed")
	}
	return p
}

// Symlinked roots fixture under tmp
func linkedSearchTree(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need POSIX")
	}
	tmp := t.TempDir()
	writeSearchFile(t, filepath.Join(tmp, "target", "real.md"), "TOKEN\n")
	if err := os.Mkdir(filepath.Join(tmp, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"linkdir": "target", "link.md": "target/real.md", "emptylink": "empty"} {
		if err := os.Symlink(filepath.Join(tmp, target), filepath.Join(tmp, link)); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

func TestSearchSymlinkedRootListsTargetFilesOnEveryEngine(t *testing.T) {
	linked := linkedSearchTree(t)
	for _, engine := range []string{"find", "rg"} {
		for _, lane := range []string{"linkdir", "link.md"} {
			t.Run(engine+"/"+lane, func(t *testing.T) {
				if engine == "rg" {
					requireRg(t)
				}
				ops, _ := hostSearchOps(t, linked, engine == "find")
				r := ops.search("*.md", filepath.Join(linked, lane), "files", "", 50, 0, "content", 0, "discovery")
				found := false
				for _, f := range r.Files {
					found = found || strings.HasSuffix(f, ".md")
				}
				if r.Error != "" || r.TotalCount < 1 || !found {
					t.Fatalf("a symlinked root listed nothing on the %s lane: %+v", engine, r)
				}
			})
		}
	}
	ops, _ := hostSearchOps(t, linked, true)
	plain := ops.search("*.md", filepath.Join(linked, "target"), "files", "", 50, 0, "content", 0, "discovery")
	if plain.Error != "" || plain.TotalCount < 1 || !strings.HasSuffix(plain.Files[0], "real.md") {
		t.Fatalf("plain %+v", plain)
	}
	for _, root := range []string{"empty", "emptylink"} {
		if r := ops.search("*.md", filepath.Join(linked, root), "files", "", 50, 0, "content", 0, "discovery"); r.Error != "" || r.TotalCount != 0 {
			t.Fatalf("empty root %s answered %+v", root, r)
		}
	}
}

func TestSearchSymlinkedContentRootUnderDotDirOnTheGrepLane(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need POSIX")
	}
	tmp := t.TempDir()
	hidden := filepath.Join(tmp, ".dot")
	writeSearchFile(t, filepath.Join(hidden, "real", "f.md"), "NEEDLE\n")
	os.Symlink(filepath.Join(hidden, "real", "f.md"), filepath.Join(hidden, "link.md"))
	os.Symlink(filepath.Join(hidden, "real"), filepath.Join(hidden, "dirlink"))
	ops, _ := hostSearchOps(t, tmp, true)
	for _, root := range []string{"link.md", "dirlink"} {
		r := ops.search("NEEDLE", filepath.Join(hidden, root), "content", "", 50, 0, "content", 0, "discovery")
		if r.Error != "" || r.TotalCount != 1 {
			t.Fatalf("symlinked root %s answered %+v", root, r)
		}
	}
	if r := ops.search("ABSENT_TOKEN", filepath.Join(hidden, "dirlink"), "content", "", 50, 0, "content", 0, "discovery"); r.Error != "" || r.TotalCount != 0 {
		t.Fatalf("miss %+v", r)
	}
}

func TestSearchSymlinkToHomeIsStillABroadRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need POSIX")
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	os.Mkdir(home, 0o755)
	os.Symlink(home, filepath.Join(tmp, "link-to-home"))
	prev := searchHome
	searchHome = home
	t.Cleanup(func() { searchHome = prev })
	ops, _ := hostSearchOps(t, tmp, false)
	if !ops.isBroadLocalSearchRoot(filepath.Join(tmp, "link-to-home")) {
		t.Fatal("symlink to home not refused")
	}
}

// Two roots each holding a match and an unreadable directory
func twoLockedRoots(t *testing.T, lockedNames ...string) ([]string, []string) {
	t.Helper()
	tmp := t.TempDir()
	var roots, locked []string
	for i, name := range []string{"alpha", "beta"} {
		root := filepath.Join(tmp, name)
		dir := filepath.Join(root, lockedNames[i])
		writeSearchFile(t, filepath.Join(root, name+".py"), "needle\n")
		if len(lockedNames) == 2 && lockedNames[0] == "locked" {
			writeSearchFile(t, filepath.Join(dir, "secret.py"), "needle\n")
		} else if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		lockSearchDir(t, dir)
		roots, locked = append(roots, root), append(locked, dir)
	}
	return roots, locked
}

func TestMultiRootSearchReportsEverySkippedPath(t *testing.T) {
	requireUnreadableDirs(t)
	for _, engine := range []string{"rg", "grep"} {
		for _, target := range []string{"content", "files"} {
			t.Run(engine+"/"+target, func(t *testing.T) {
				if engine == "rg" {
					requireRg(t)
				}
				env, _ := newFileToolTestEnv(t)
				roots, locked := twoLockedRoots(t, "locked", "locked")
				ops, _ := hostSearchOps(t, filepath.Dir(roots[0]), engine == "grep")
				installSearchHandle(t, ops)
				pattern := "needle"
				if target == "files" {
					pattern = "*.py"
				}
				out := runSearchTool(t, env, searchArgs{pattern: pattern, target: target, path: strings.Join(roots, ",")}, "multi-root-"+target+"-"+engine)
				if _, has := out["error"]; has {
					t.Fatalf("%v", out)
				}
				var found []string
				if files, ok := out["files"].([]any); ok {
					for _, f := range files {
						found = append(found, f.(string))
					}
				}
				if ms, ok := out["matches"].([]any); ok {
					for _, m := range ms {
						found = append(found, m.(map[string]any)["path"].(string))
					}
				}
				sort.Strings(found)
				wantSlice(t, "found", found, []string{filepath.Join(roots[0], "alpha.py"), filepath.Join(roots[1], "beta.py")})
				w := toolWarning(out)
				if !strings.Contains(w, "Skipped 2 unreadable path(s)") || !strings.Contains(w, locked[0]) || !strings.Contains(w, locked[1]) ||
					!strings.Contains(w, "Results may be incomplete") || !strings.HasSuffix(strings.TrimRight(w, " "), "searched 2 that exist") {
					t.Fatalf("warning %q", w)
				}
			})
		}
	}
}

func TestMultiRootSkippedDirNameWithCommaSurvivesMerge(t *testing.T) {
	requireUnreadableDirs(t)
	for _, engine := range []string{"rg", "grep"} {
		t.Run(engine, func(t *testing.T) {
			if engine == "rg" {
				requireRg(t)
			}
			env, _ := newFileToolTestEnv(t)
			roots, locked := twoLockedRoots(t, "plain", "one, two")
			ops, _ := hostSearchOps(t, filepath.Dir(roots[0]), engine == "grep")
			installSearchHandle(t, ops)
			out := runSearchTool(t, env, searchArgs{pattern: "needle", path: strings.Join(roots, ",")}, "multi-root-comma-"+engine)
			w := toolWarning(out)
			if !strings.Contains(w, "Skipped 2 unreadable path(s)") || !strings.Contains(w, "permissions: "+locked[0]+", "+locked[1]+". Results may be incomplete") {
				t.Fatalf("warning %q", w)
			}
		})
	}
}

// Five readable logs beside sixty unreadable directories
func lockedSearchTree(t *testing.T) string {
	t.Helper()
	requireUnreadableDirs(t)
	tree := t.TempDir()
	for i := range 5 {
		writeSearchFile(t, filepath.Join(tree, "zz", fmt.Sprintf("f%d.log", i)), "needle\n")
	}
	for i := range 60 {
		d := filepath.Join(tree, fmt.Sprintf("d%02d", i))
		os.Mkdir(d, 0o755)
		lockSearchDir(t, d)
	}
	return tree
}

// Base names of the files or match paths, sorted
func foundBaseNames(r *SearchResult) []string {
	found := r.Files
	if len(found) == 0 {
		found = matchPaths(r)
	}
	var out []string
	for _, p := range found {
		out = append(out, filepath.Base(p))
	}
	sort.Strings(out)
	return out
}

func TestSearchManyUnreadableDirsDoNotEatTheResultBudget(t *testing.T) {
	requireRg(t)
	tree := lockedSearchTree(t)
	var logs []string
	for i := range 5 {
		logs = append(logs, fmt.Sprintf("f%d.log", i))
	}
	for _, tc := range []struct{ target, outputMode string }{{"files", "content"}, {"content", "files_only"}, {"content", "content"}} {
		t.Run(tc.target+"/"+tc.outputMode, func(t *testing.T) {
			ops, _ := hostSearchOps(t, tree, false)
			pattern := "needle"
			if tc.target == "files" {
				pattern = "*.log"
			}
			bounded := ops.search(pattern, tree, tc.target, "", 3, 0, tc.outputMode, 0, "discovery")
			names := foundBaseNames(bounded)
			if bounded.Error != "" || len(names) != 3 || !strings.Contains(bounded.Warning, "unreadable path(s) with restricted permissions") {
				t.Fatalf("bounded %+v", bounded)
			}
			for _, n := range names {
				if !strings.HasSuffix(n, ".log") {
					t.Fatalf("bounded found %q", names)
				}
			}
			prev := stderrRetainBytes
			stderrRetainBytes = 1
			defer func() { stderrRetainBytes = prev }()
			full := ops.search(pattern, tree, tc.target, "", 50, 0, tc.outputMode, 0, "discovery")
			if full.Error != "" || !strings.Contains(full.Warning, "Skipped 60 unreadable path(s)") || !strings.Contains(full.Warning, "(+59 more)") {
				t.Fatalf("full %+v", full)
			}
			wantSlice(t, "full", foundBaseNames(full), logs)
		})
	}
}

func TestSearchFindFallbackNamesTheUnreadableDir(t *testing.T) {
	tree := lockedSearchTree(t)
	ops, _ := hostSearchOps(t, tree, true)
	r := ops.search("*.log", tree, "files", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || !strings.Contains(r.Warning, "Skipped 60 unreadable path(s) with restricted permissions: "+tree+"/d") {
		t.Fatalf("%+v", r)
	}
	wantSlice(t, "files", foundBaseNames(r), []string{"f0.log", "f1.log", "f2.log", "f3.log", "f4.log"})
}

func TestSearchLiveUnreadableDirIsSkipped(t *testing.T) {
	requireRg(t)
	requireUnreadableDirs(t)
	tmp := t.TempDir()
	writeSearchFile(t, filepath.Join(tmp, "open", "a.py"), "needle here\n")
	locked := filepath.Join(tmp, "locked")
	writeSearchFile(t, filepath.Join(locked, "secret.py"), "needle hidden\n")
	lockSearchDir(t, locked)
	ops, _ := hostSearchOps(t, tmp, false)
	r := ops.search("*.py", tmp, "files", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 1 || !strings.Contains(r.Warning, locked) {
		t.Fatalf("files %+v", r)
	}
	wantSlice(t, "files", r.Files, []string{filepath.Join(tmp, "open", "a.py")})
	r = ops.search("needle", tmp, "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || !strings.Contains(r.Warning, locked) {
		t.Fatalf("content %+v", r)
	}
	wantSlice(t, "matches", matchPaths(r), []string{filepath.Join(tmp, "open", "a.py")})
}

// Hidden root with one readable match, plus an unreadable subtree when locked
func hiddenSearchRoot(t *testing.T, locked bool) (string, string) {
	t.Helper()
	requireUnreadableDirs(t)
	root := filepath.Join(t.TempDir(), ".hidden-root")
	writeSearchFile(t, filepath.Join(root, "open", "a.txt"), "needle one\n")
	if !locked {
		return root, ""
	}
	dir := filepath.Join(root, "locked")
	writeSearchFile(t, filepath.Join(dir, "b.txt"), "needle two\n")
	lockSearchDir(t, dir)
	return root, dir
}

func TestSearchPrunedGrepLaneNamesUnreadableSubtrees(t *testing.T) {
	root, locked := hiddenSearchRoot(t, true)
	ops, _ := hostSearchOps(t, root, true)
	r := ops.search("needle", root, "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || !strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, locked) {
		t.Fatalf("content %+v", r)
	}
	wantSlice(t, "matches", matchPaths(r), []string{filepath.Join(root, "open", "a.txt")})
	r = ops.search("needle", root, "content", "", 50, 0, "count", 0, "discovery")
	if r.Error != "" || !reflect.DeepEqual(r.Counts, []SearchCount{{filepath.Join(root, "open", "a.txt"), 1}}) || !strings.Contains(r.Warning, locked) {
		t.Fatalf("count %+v", r)
	}
	r = ops.search("absent-token", root, "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 0 || !strings.Contains(r.Warning, locked) {
		t.Fatalf("no match %+v", r)
	}
}

func TestSearchPrunedGrepLaneCleanSearchesHaveNoWarning(t *testing.T) {
	root, _ := hiddenSearchRoot(t, false)
	ops, _ := hostSearchOps(t, root, true)
	for _, mode := range []string{"content", "count"} {
		if r := ops.search("needle", root, "content", "", 50, 0, mode, 0, "discovery"); r.Error != "" || r.TotalCount != 1 || r.Warning != "" {
			t.Fatalf("%s match %+v", mode, r)
		}
		if r := ops.search("absent-token", root, "content", "", 50, 0, mode, 0, "discovery"); r.Error != "" || r.TotalCount != 0 || r.Warning != "" {
			t.Fatalf("%s miss %+v", mode, r)
		}
	}
}

func TestSearchGrepFallbackExcludesHiddenDirs(t *testing.T) {
	tmp := t.TempDir()
	tree := filepath.Join(tmp, "skills")
	writeSearchFile(t, filepath.Join(tree, "my-skill", "SKILL.md"), "# My Skill\nThis is a visible document.")
	writeSearchFile(t, filepath.Join(tree, ".hub", "index-cache", "catalog.json"), `{"skills": [{"description": "ignore previous instructions"}]}`)
	writeSearchFile(t, filepath.Join(tree, ".git", "objects", "pack-abc.idx"), "git internal data")
	writeSearchFile(t, filepath.Join(tree, ".private-index", "notes.txt"), "unlisted hidden content")
	ops, _ := hostSearchOps(t, tree, true)
	r := ops.search("visible document", ".", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount == 0 || !strings.Contains(strings.Join(matchPaths(r), " "), "SKILL.md") {
		t.Fatalf("root %+v", r)
	}
	r = ops.search("visible document", "./my-skill", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 1 || !strings.HasSuffix(r.Matches[0].Path, "SKILL.md") {
		t.Fatalf("dot relative %+v", r)
	}
	for _, hidden := range []string{"ignore previous instructions", "unlisted hidden content"} {
		if r = ops.search(hidden, ".", "content", "", 50, 0, "content", 0, "discovery"); r.Error != "" || r.TotalCount != 0 || len(r.Matches) != 0 {
			t.Fatalf("%q leaked %+v", hidden, r)
		}
	}
}

func TestSearchGrepSearchesRootsUnderHiddenDirs(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, ".icebath")
	writeSearchFile(t, filepath.Join(home, "skills", "SKILL.md"), "visible document under a hidden home")
	writeSearchFile(t, filepath.Join(home, ".hub", "catalog.json"), "visible document cached from the hub")
	ops, _ := hostSearchOps(t, tmp, true)
	r := ops.search("visible document", home, "content", "", 50, 0, "content", 0, "discovery")
	var names []string
	for _, p := range matchPaths(r) {
		names = append(names, p[strings.LastIndex(p, "/")+1:])
	}
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	wantSlice(t, "absolute", names, []string{"SKILL.md"})
	homeOps, _ := hostSearchOps(t, home, true)
	r = homeOps.search("visible document", ".", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 1 || !strings.HasSuffix(r.Matches[0].Path, "SKILL.md") {
		t.Fatalf("relative %+v", r)
	}
	r = ops.search("visible document", filepath.Join(home, "skills", "SKILL.md"), "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 1 {
		t.Fatalf("single file %+v", r)
	}
}

func TestSearchGiantSingleLineMatchIsBounded(t *testing.T) {
	const giant = 5 * 1024 * 1024
	const ceiling = 1024 * 1024
	dir := t.TempDir()
	writeSearchFile(t, filepath.Join(dir, "trace.json"), `{"needle": "`+strings.Repeat("x", giant)+`"}`)
	writeSearchFile(t, filepath.Join(dir, "small.py"), "needle = 1\n")
	for _, engine := range []string{"rg", "grep"} {
		for _, mode := range []string{"content", "files_only", "count"} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				mustLookPath(t, engine)
				var maxOut atomic.Int64
				run := bashExecute(false, func(c string) (*ExecResult, bool) {
					if engine == "grep" && strings.HasPrefix(c, "command -v rg") {
						return &ExecResult{ReturnCode: 1}, true
					}
					return nil, false
				})
				env := &fakeTermEnv{envType: "fake", cwd: dir, execute: func(ctx context.Context, c string, o ExecOptions) (*ExecResult, error) {
					r, err := run(ctx, c, o)
					if r != nil && int64(len(r.Output)) > maxOut.Load() {
						maxOut.Store(int64(len(r.Output)))
					}
					return r, err
				}}
				r := newShellFileOps(remoteTestEnv(t), env, "").search("needle", dir, "content", "", 50, 0, mode, 0, "discovery")
				if r.Error != "" {
					t.Fatal(r.Error)
				}
				var names []string
				switch mode {
				case "content":
					names = foundBaseNames(r)
					for _, m := range r.Matches {
						if pyLen(m.Content) > 500 {
							t.Fatalf("content %d chars", pyLen(m.Content))
						}
					}
				case "files_only":
					names = foundBaseNames(r)
				default:
					for _, c := range r.Counts {
						names = append(names, filepath.Base(c.Path))
					}
					sort.Strings(names)
				}
				wantSlice(t, "paths", names, []string{"small.py", "trace.json"})
				if maxOut.Load() >= ceiling {
					t.Fatalf("%s pipeline returned %d bytes of stdout", engine, maxOut.Load())
				}
			})
		}
	}
}

func TestSearchAutoMultilineRouting(t *testing.T) {
	requireRg(t)
	env, _ := newFileToolTestEnv(t)
	proj := filepath.Join(t.TempDir(), "proj")
	writeSearchFile(t, filepath.Join(proj, "mod.py"), "def setup():\n    init_db()\n    return True\n\ndef teardown():\n    close_db()\n")
	r := runSearchTool(t, env, searchArgs{pattern: `def setup\(\):\n    init_db\(\)`, path: proj}, "t-ml-1")
	if _, has := r["error"]; has || toolTotal(r) < 1 || !strings.Contains(toolWarning(r), "multiline") {
		t.Fatalf("regex newline %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "return True\n\ndef teardown", path: proj}, "t-ml-2")
	if _, has := r["error"]; has || toolTotal(r) < 1 {
		t.Fatalf("literal newline %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: "init_db", path: proj}, "t-ml-3")
	if toolTotal(r) != 1 || strings.Contains(toolWarning(r), "multiline") {
		t.Fatalf("plain %v", r)
	}
	writeSearchFile(t, filepath.Join(proj, "strings.py"), "SEP = \"a\\\\nb\"\n")
	r = runSearchTool(t, env, searchArgs{pattern: `a\\nb`, path: proj}, "t-ml-4")
	if _, has := r["error"]; has || strings.Contains(toolWarning(r), "multiline") {
		t.Fatalf("escaped %v", r)
	}
	r = runSearchTool(t, env, searchArgs{pattern: `def missing\(\):\n    nope\(\)`, path: proj}, "t-ml-5")
	if _, has := r["error"]; has || toolTotal(r) != 0 {
		t.Fatalf("zero %v", r)
	}
}

func TestSearchErrorGuardOnEachEngine(t *testing.T) {
	matchTree := searchableTempDir(t)
	for i := range 5 {
		writeSearchFile(t, filepath.Join(matchTree, fmt.Sprintf("f%d.txt", i)), fmt.Sprintf("needle line %d\n", i))
	}
	methods := map[string]func(*shellFileOps, string, string, string) *SearchResult{
		"grep": func(s *shellFileOps, pattern, path, mode string) *SearchResult {
			return s.searchWithGrep(pattern, path, "", 50, 0, mode, 0)
		},
	}
	if _, err := exec.LookPath("rg"); err == nil {
		methods["rg"] = func(s *shellFileOps, pattern, path, mode string) *SearchResult {
			return s.searchWithRg(pattern, path, "", 50, 0, mode, 0, "")
		}
	}
	for name, search := range methods {
		t.Run(name, func(t *testing.T) {
			ops, _ := hostSearchOps(t, matchTree, false)
			if r := search(ops, "needle", matchTree, "content"); r.Error != "" || len(r.Matches) != 5 {
				t.Fatalf("happy %+v", r)
			}
			if r := search(ops, "[", matchTree, "content"); !strings.Contains(r.Error, "Search failed") || len(r.Matches) != 0 {
				t.Fatalf("hard error swallowed %+v", r)
			}
			requireUnreadableDirs(t)
			partial := searchableTempDir(t)
			for i := range 4 {
				writeSearchFile(t, filepath.Join(partial, fmt.Sprintf("f%d.txt", i)), fmt.Sprintf("needle line %d\n", i))
			}
			locked := filepath.Join(partial, "sub", "locked.txt")
			writeSearchFile(t, locked, "needle in locked\n")
			lockSearchDir(t, locked)
			partialOps, _ := hostSearchOps(t, partial, false)
			if r := search(partialOps, "needle", partial, "count"); r.Error != "" || r.TotalCount < 4 {
				t.Fatalf("count partial %+v", r)
			}
		})
	}
	ops, _ := hostSearchOps(t, matchTree, false)
	if r := ops.search(`absent\\npattern`, matchTree, "content", "", 50, 0, "content", 0, "discovery"); r.Error != "" || r.TotalCount != 0 || r.Warning != "" {
		t.Fatalf("literal backslash n %+v", r)
	}
}

func TestSearchFindDashPrefixedRelativeRootIsAnExplicitOperand(t *testing.T) {
	tmp := t.TempDir()
	writeSearchFile(t, filepath.Join(tmp, "--version", "dash.py"), "")
	writeSearchFile(t, filepath.Join(tmp, "ordinary", "plain.py"), "")
	ops, env := hostSearchOps(t, tmp, true)
	r := ops.searchFiles("*.py", []string{"--version", "ordinary"}, true, 10, 0, "discovery")
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	files := append([]string{}, r.Files...)
	sort.Strings(files)
	wantSlice(t, "files", files, []string{"./--version/dash.py", "ordinary/plain.py"})
	var executed []string
	for _, c := range envCommands(env) {
		if strings.HasPrefix(c, "set -o pipefail; find ") {
			executed = append(executed, c)
		}
	}
	if len(executed) != 1 {
		t.Fatalf("find runs %q", executed)
	}
	tokens, err := shlexSplit(strings.TrimPrefix(executed[0], "set -o pipefail; "))
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(tokens, "./--version") || containsString(tokens, "--version") {
		t.Fatalf("tokens %q", tokens)
	}
	for _, f := range r.Files {
		if strings.Contains(f, "find (GNU findutils)") {
			t.Fatalf("find parsed the root as an option: %q", f)
		}
	}
}

func TestSearchMacosGrepPrunedSearchFindsNestedProtectedNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	home := filepath.Join(t.TempDir(), "Users", "alice")
	writeSearchFile(t, filepath.Join(home, "work", "repo", "Downloads", "notes.txt"), "needle here\n")
	writeSearchFile(t, filepath.Join(home, "Downloads", "secret.txt"), "needle protected\n")
	asMacosHost(t, home)
	ops, _ := hostSearchOps(t, home, true)
	r := ops.search("needle", home, "content", "", 50, 0, "content", 0, "discovery")
	paths := strings.Join(matchPaths(r), "\n")
	if !strings.Contains(paths, "work/repo/Downloads/notes.txt") || strings.Contains(paths, filepath.Join(home, "Downloads", "secret.txt")) {
		t.Fatalf("matches %q", paths)
	}
}

func TestSearchMacosRealRipgrepSkipsProtectedFolder(t *testing.T) {
	requireRg(t)
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	home := filepath.Join(t.TempDir(), "Users", "alice")
	writeSearchFile(t, filepath.Join(home, "safe", "visible.txt"), "needle")
	writeSearchFile(t, filepath.Join(home, "Downloads", "protected.txt"), "needle")
	asMacosHost(t, home)
	ops, _ := hostSearchOps(t, home, false)
	r := ops.search("needle", home, "content", "", 50, 0, "content", 0, "discovery")
	paths := strings.Join(matchPaths(r), "\n")
	if !strings.Contains(paths, "visible.txt") || strings.Contains(paths, "protected.txt") {
		t.Fatalf("matches %q", paths)
	}
}

func TestSearchNativeRunnerWalksPathLikePopen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native rg lane is POSIX only")
	}
	first, second := t.TempDir(), t.TempDir()
	writeSearchFile(t, filepath.Join(first, "fakeprog"), "#!/bin/sh\necho wrong\n")
	writeSearchFile(t, filepath.Join(second, "fakeprog"), "#!/bin/sh\necho hit\n")
	if err := os.Chmod(filepath.Join(second, "fakeprog"), 0o755); err != nil {
		t.Fatal(err)
	}
	basePath := os.Getenv("PATH")
	t.Setenv("PATH", first+":"+second+":"+basePath)
	ops, _ := hostSearchOps(t, t.TempDir(), false)
	if r := ops.runRgNative([]string{"fakeprog"}, 5, 5, false); r.ExitCode != 0 || r.Stdout != "hit\n" {
		t.Fatalf("second candidate %+v", r)
	}
	t.Setenv("PATH", first+":"+basePath)
	ops, _ = hostSearchOps(t, t.TempDir(), false)
	if r := ops.runRgNative([]string{"fakeprog"}, 5, 5, false); r.ExitCode != 2 || r.Stdout != "rg: [Errno 13] Permission denied: 'fakeprog'" {
		t.Fatalf("non executable only %+v", r)
	}
	missing := filepath.Join(t.TempDir(), "gone")
	ops, _ = hostSearchOps(t, missing, false)
	if r := ops.runRgNative([]string{"sh", "-c", "true"}, 5, 5, false); r.ExitCode != 2 || r.Stdout != "rg: [Errno 2] No such file or directory: '"+missing+"'" {
		t.Fatalf("missing cwd %+v", r)
	}
}

func TestSearchFindLiteralPathExpressionsEscapeGlobs(t *testing.T) {
	got := findLiteralPathExpressions([]string{"a[1]*?", `b\c`, "plain"})
	wantSlice(t, "escaped", got, []string{`a\[1\]\*\?`, `b\c`, "plain"})
}
