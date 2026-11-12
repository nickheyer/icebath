package pm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestEntryKeepsIsolationAndPMOffTheImportPath(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("platforms linux, macos")
	}
	tmp := isolateHome(t)
	venv, _ := peTrapVenv(t, tmp)
	peWrite(t, filepath.Join(tmp, "venv_only_dep.py"), "VALUE = 'cwd shadow'\n")
	payload := filepath.Join(tmp, "payload")
	repo := svSealedPayload(t, payload, svPayloadStore(t, payload, filepath.Join("tools", "python")))
	prefix, err := VenvCommand(context.Background(), repo, venv, []string{"-I"})
	if err != nil {
		t.Fatal(err)
	}
	state := InstallStateDir(context.Background(), RepoRoot())
	code := "import json, os, sys, venv_only_dep; print(json.dumps([venv_only_dep.VALUE, [p for p in sys.path if p in ('', os.getcwd()) or p.startswith(" +
		pyrepr.StrRepr(state) + ")]]))"
	if got := peRunJSON(t, append(append([]string{}, prefix...), "-c", code), tmp); !reflect.DeepEqual(got, []any{"from the venv", []any{}}) {
		t.Fatalf("got %v", got)
	}
}
