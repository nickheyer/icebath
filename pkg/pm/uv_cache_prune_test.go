package pm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Lock fixture directory with the given uv.lock text
func peLockRepo(t *testing.T, root, text string) string {
	t.Helper()
	repo := filepath.Join(root, "repo")
	peWrite(t, filepath.Join(repo, "uv.lock"), text)
	return repo
}

func TestUVCachePruneStagedCachePrunesArchiveBucketsOutsideTheLock(t *testing.T) {
	root := t.TempDir()
	lock := peLockRepo(t, root, "[[package]]\nname = \"keep-me\"\nversion = \"1.0.0\"\nsource = { registry = \"https://pypi.org/simple\" }\n\n"+
		"[[package]]\nname = \"also-keep\"\nversion = \"2.0.0\"\nsource = { registry = \"https://pypi.org/simple\" }\n")
	cache := filepath.Join(root, "cache")
	for bucket, info := range map[string]string{"keep-me": "keep_me-1.0.0.dist-info", "also-keep": "also_keep-2.0.0.dist-info",
		"superseded-pin": "superseded_pin-0.9.9.dist-info", "old-dependency": "old_dependency-3.2.1.dist-info"} {
		peWrite(t, filepath.Join(cache, "archive-v0", bucket, info), "")
	}
	writeFile(t, filepath.Join(cache, "archive-v0", "unknownbucket", "data.bin"), []byte("x"), 0o644)
	pruned, err := PruneUVCacheToLock(cache, lock)
	if err != nil || pruned != 2 {
		t.Fatalf("pruned %d %v", pruned, err)
	}
	for bucket, kept := range map[string]bool{"keep-me": true, "also-keep": true, "superseded-pin": false, "old-dependency": false} {
		if isDirectory(filepath.Join(cache, "archive-v0", bucket)) != kept {
			t.Fatalf("%s kept %v", bucket, !kept)
		}
	}
	if !isRegularFile(filepath.Join(cache, "archive-v0", "unknownbucket", "data.bin")) {
		t.Fatal("unidentifiable bucket pruned")
	}
}

func TestUVCachePruneStagedCachePrunesWheelIndexEntriesOutsideTheLock(t *testing.T) {
	root := t.TempDir()
	lock := peLockRepo(t, root, "[[package]]\nname = \"keep-me\"\nversion = \"1.0.0\"\nsource = { registry = \"https://pypi.org/simple\" }\n")
	cache := filepath.Join(root, "cache")
	for _, entry := range []string{"keep-me", "superseded-pin"} {
		writeFile(t, filepath.Join(cache, "wheels-v6", "pypi", entry, "metadata.msgpack"), nil, 0o644)
	}
	writeFile(t, filepath.Join(cache, "sdists-v9", "pypi", "old-sdist-only", "metadata.msgpack"), nil, 0o644)
	if _, err := PruneUVCacheToLock(cache, lock); err != nil {
		t.Fatal(err)
	}
	if !isDirectory(filepath.Join(cache, "wheels-v6", "pypi", "keep-me")) || existsOrLink(filepath.Join(cache, "wheels-v6", "pypi", "superseded-pin")) ||
		existsOrLink(filepath.Join(cache, "sdists-v9", "pypi", "old-sdist-only")) {
		t.Fatal("index entries pruned wrongly")
	}
}

func TestUVCachePruneStagedCacheKeepsADottedDistTheLockResolves(t *testing.T) {
	root := t.TempDir()
	lock := peLockRepo(t, root, "[[package]]\nname = \"ruamel-yaml\"\nversion = \"0.18.16\"\nsource = { registry = \"https://pypi.org/simple\" }\n")
	cache := filepath.Join(root, "cache")
	bucket := filepath.Join(cache, "archive-v0", "bucket")
	os.MkdirAll(filepath.Join(bucket, "ruamel.yaml-0.18.16.dist-info"), 0o777)
	index := filepath.Join(cache, "wheels-v6", "pypi", "ruamel-yaml")
	os.MkdirAll(index, 0o777)
	if pruned, err := PruneUVCacheToLock(cache, lock); err != nil || pruned != 0 || !isDirectory(bucket) || !isDirectory(index) {
		t.Fatalf("pruned %d %v", pruned, err)
	}
}

func TestUVCachePrunePruneDropsAURLWheelLinkWhoseArchiveItPruned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	root := t.TempDir()
	lock := peLockRepo(t, root, "[[package]]\nname = \"keep-me\"\nversion = \"1.0\"\n")
	cache := filepath.Join(root, "cache")
	os.MkdirAll(filepath.Join(cache, "archive-v0", "gone", "gone_dep-1.0.dist-info"), 0o777)
	os.MkdirAll(filepath.Join(cache, "archive-v0", "kept", "keep_me-1.0.dist-info"), 0o777)
	url := filepath.Join(cache, "wheels-v7", "url", "d87c3d39b3c6ef8c")
	os.MkdirAll(filepath.Join(url, "gone-dep"), 0o777)
	os.MkdirAll(filepath.Join(url, "keep-me"), 0o777)
	os.Symlink("../../../../archive-v0/gone", filepath.Join(url, "gone-dep", "1.0-py3-none-any"))
	os.Symlink("../../../../archive-v0/kept", filepath.Join(url, "keep-me", "1.0-py3-none-any"))
	if _, err := PruneUVCacheToLock(cache, lock); err != nil {
		t.Fatal(err)
	}
	if existsOrLink(filepath.Join(url, "gone-dep", "1.0-py3-none-any")) {
		t.Fatal("a dangling wheel link survived the prune")
	}
	if resolvePath(filepath.Join(url, "keep-me", "1.0-py3-none-any")) != resolvePath(filepath.Join(cache, "archive-v0", "kept")) {
		t.Fatal("a resolvable wheel link was dropped")
	}
}

// A venv whose interpreter must never run, holding a module and a pth added directory
func peTrapVenv(t *testing.T, root string) (string, string) {
	t.Helper()
	venv := filepath.Join(root, "venv")
	version := peRun(t, []string{peHostPython(t), "-I", "-c", "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')"}, root, pyEnviron())
	peWrite(t, filepath.Join(venv, "pyvenv.cfg"), "home = /nonexistent\nversion = "+version+".0\n")
	writeFile(t, VenvPython(venv), []byte("#!/bin/sh\nexit 99\n"), 0o755)
	site := filepath.Join(venv, "lib", "python"+version, "site-packages")
	peWrite(t, filepath.Join(site, "venv_only_dep.py"), "VALUE = 'from the venv'\n")
	peWrite(t, filepath.Join(site, "venv_only_cli.py"), "import json, sys, venv_only_dep\nprint(json.dumps([venv_only_dep.VALUE, sys.argv]))\n")
	extra := filepath.Join(root, "pth-added")
	peWrite(t, filepath.Join(extra, "pth_only_dep.py"), "VALUE = 'from a .pth'\n")
	peWrite(t, filepath.Join(site, "extra.pth"), extra+"\n")
	return venv, site
}

// Runs a command expecting success and decodes its JSON output
func peRunJSON(t *testing.T, argv []string, cwd string) any {
	t.Helper()
	env := pyEnviron()
	delete(env, "PYTHONPATH")
	delete(env, "PYTHONHOME")
	var out any
	if err := json.Unmarshal([]byte(peRun(t, argv, cwd, env)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVenvEntryRunsScriptsCodeAndModulesWithoutTheRedirector(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms linux, macos")
	}
	isolateHome(t)
	root := t.TempDir()
	venv, site := peTrapVenv(t, root)
	prefix, err := VenvEntryCommand(context.Background(), peHostPython(t), []string{"-I", "-u"}, site)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(prefix, VenvPython(venv)) || !containsString(prefix, "-S") {
		t.Fatalf("prefix %v", prefix)
	}
	script := filepath.Join(root, "work", "script.py")
	peWrite(t, script, "import json, sys, venv_only_dep, pth_only_dep\nprint(json.dumps([venv_only_dep.VALUE, pth_only_dep.VALUE, sys.argv]))\n")
	if got := peRunJSON(t, append(append([]string{}, prefix...), script, "a", "--flag"), root); !reflect.DeepEqual(got, []any{"from the venv", "from a .pth", []any{script, "a", "--flag"}}) {
		t.Fatalf("script %v", got)
	}
	code := "import json, sys, venv_only_dep, pth_only_dep; print(json.dumps([venv_only_dep.VALUE, pth_only_dep.VALUE, sys.argv, __name__]))"
	if got := peRunJSON(t, append(append([]string{}, prefix...), "-c", code, "x"), root); !reflect.DeepEqual(got, []any{"from the venv", "from a .pth", []any{"-c", "x"}, "__main__"}) {
		t.Fatalf("code %v", got)
	}
	got := peRunJSON(t, append(append([]string{}, prefix...), "-m", "venv_only_cli", "y"), root).([]any)
	argv := got[1].([]any)
	if got[0] != "from the venv" || !reflect.DeepEqual(argv[1:], []any{"y"}) || !strings.HasSuffix(argv[0].(string), "venv_only_cli.py") {
		t.Fatalf("module %v", got)
	}
}

func TestVenvEntryKeepsIsolationAndPMOffTheImportPath(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	_, site := peTrapVenv(t, root)
	peWrite(t, filepath.Join(root, "venv_only_dep.py"), "VALUE = 'cwd shadow'\n")
	entry, err := VenvEntryScript(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code := "import json, sys, venv_only_dep; print(json.dumps([venv_only_dep.VALUE, " + pyrepr.StrRepr(filepath.Dir(entry)) + " in sys.path]))"
	if got := peRunJSON(t, []string{peHostPython(t), "-I", "-S", entry, site, "-c", code}, root); !reflect.DeepEqual(got, []any{"from the venv", false}) {
		t.Fatalf("got %v", got)
	}
}

func TestMarkerEvalAnswersForTheGivenEnvironment(t *testing.T) {
	isolateHome(t)
	helper, err := MarkerEvalScript(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, marker := range []string{"sys_platform == 'linux'", "sys_platform == 'win32'"} {
		result, err := exec.Command(peHostPython(t), helper, marker, pyJSONCompact(map[string]string{"sys_platform": "linux"})).Output()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimSpace(string(result)))
	}
	if !reflect.DeepEqual(out, []string{"1", "0"}) {
		t.Fatalf("got %v", out)
	}
}

func TestMarkerEvalInRuntimeFallsBackToUngatedOnFailure(t *testing.T) {
	isolateHome(t)
	if !EvaluateMarkerInRuntime(context.Background(), "sys_platform == 'win32'", map[string]string{"sys_platform": "linux"}, RuntimeOptions{NoBootstrap: true}) {
		t.Fatal("a runtime that cannot start must leave the marker ungated")
	}
}

// Records real uv and the pinned Python as the installed toolchain of the test store
func peStoreToolchain(t *testing.T, tmp string) {
	t.Helper()
	uv, python := peUV(t), resolvePath(pePinnedPython(t))
	store := filepath.Join(tmp, "store")
	t.Setenv(EnvRuntimeDir, store)
	managed := filepath.Join(store, "uv-fixture", filepath.Base(uv))
	writeFile(t, managed, readFile(t, uv), 0o755)
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	peSwap(t, &lockfilePath, func() string { return lockPath })
	lock := NewLockfile(lockPath)
	target, _ := CurrentTarget()
	facts, _ := NewFacts(filepath.Join(store, "facts.json"), false)
	for _, row := range [][2]string{{"uv", filepath.Dir(managed)}, {"python", filepath.Dir(filepath.Dir(python))}} {
		p, _ := GetPackage(row[0])
		lock.SetPin(row[0], "fixture", map[string]any{target: map[string]any{"url": "https://example.invalid/" + row[0], "sha256": strings.Repeat("1", 64)}})
		digest, err := TreeDigest(row[1])
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(store, row[1])
		if err := facts.Record(row[0], "fixture", rel, p.Env(row[1], target), store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("1", 64)}, Digest: &digest}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkerEvalInRuntimeEvaluatesInTheIsolatedRuntime(t *testing.T) {
	tmp := isolateHome(t)
	peStoreToolchain(t, tmp)
	opts := RuntimeOptions{Project: filepath.Join(peReferences, "pm"), Imports: PMRuntimeImports}
	ctx := context.Background()
	if !EvaluateMarkerInRuntime(ctx, "sys_platform == 'linux'", map[string]string{"sys_platform": "linux"}, opts) {
		t.Fatal("a true marker evaluated false")
	}
	if EvaluateMarkerInRuntime(ctx, "sys_platform == 'win32'", map[string]string{"sys_platform": "linux"}, opts) {
		t.Fatal("a false marker evaluated true")
	}
}
