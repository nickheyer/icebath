package pm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Store holding real uv as its installed tool, uv and python pinned at test
func peInstalledUV(t *testing.T) (string, string, *Facts, string, string) {
	t.Helper()
	tmp := isolateHome(t)
	store := filepath.Join(tmp, "store")
	t.Setenv(EnvRuntimeDir, store)
	target, _ := CurrentTarget()
	digest := strings.Repeat("1", 64)
	binary := filepath.Join(store, "uv", "uv")
	writeFile(t, binary, readFile(t, peUV(t)), 0o755)
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	lock := NewLockfile(lockPath)
	for _, name := range []string{"uv", "python"} {
		lock.SetPin(name, "test", map[string]any{target: map[string]any{"url": "https://test.invalid/" + name, "sha256": digest}})
	}
	lock.Save()
	peSwap(t, &lockfilePath, func() string { return lockPath })
	facts, err := NewFacts(filepath.Join(store, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record("uv", "test", "uv", EnvDiff{}, store, FactRecord{Target: &target, Artifacts: []string{digest}}); err != nil {
		t.Fatal(err)
	}
	return tmp, binary, facts, target, digest
}

// Registers the catalogue Python with its interpreter at bin/python, the layout a uv venv gives
func peFixturePython(t *testing.T) {
	t.Helper()
	p, err := GetPackage("python")
	if err != nil {
		t.Fatal(err)
	}
	fixture := *p.(*Python)
	fixture.BinaryRel = map[string]string{"win32": "Scripts/python.exe", "posix": "bin/python"}
	fixture.bind(&fixture)
	peRegister(t, &fixture)
}

func TestUVToolchainAllUVCommandsKeepThePMInterpreter(t *testing.T) {
	root, uv, facts, target, digest := peInstalledUV(t)
	store := filepath.Join(root, "store")
	selected := filepath.Join(store, "selected-python")
	clean := map[string]string{}
	for k, v := range pyEnviron() {
		if !strings.HasPrefix(k, "UV_") {
			clean[k] = v
		}
	}
	clean["UV_NO_CONFIG"], clean["UV_OFFLINE"], clean["UV_PYTHON_DOWNLOADS"] = "1", "1", "never"
	peRun(t, []string{uv, "venv", "--python", peHostPython(t), selected}, root, clean)
	python := filepath.Join(selected, "bin", "python")
	peFixturePython(t)
	if err := facts.Record("python", "test", "selected-python", EnvDiff{}, store, FactRecord{Target: &target, Artifacts: []string{digest}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UV_PYTHON", filepath.Join(root, "ambient-python"))
	t.Setenv("UV_PROJECT_ENVIRONMENT", filepath.Join(root, "ambient-venv"))
	before := os.Environ()
	ctx := context.Background()
	gotUV, gotPython, found, err := Toolchain(ctx, false, false)
	if err != nil || !found || gotUV != uv || gotPython != python || !reflect.DeepEqual(os.Environ(), before) {
		t.Fatalf("toolchain %q %q %v %v", gotUV, gotPython, found, err)
	}
	project := filepath.Join(root, "project")
	peWrite(t, filepath.Join(project, "pyproject.toml"), "[project]\nname=\"pm-python-proof\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.uv]\npackage=false\n")
	peWrite(t, filepath.Join(project, ".python-version"), filepath.Join(root, "host-only-python"))
	e, err := ManagedEnvironment(ctx, filepath.Join(root, "candidate"), ManagedEnvironmentOptions{Offline: true})
	if err != nil || e.Python != python {
		t.Fatalf("environment python %v %v", e, err)
	}
	if err := e.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Lock(ctx, project, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, project, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, ""); err != nil {
		t.Fatal(err)
	}
	prefix, err := exec.Command(e.Executable(), "-I", "-c", "import sys; print(sys.prefix)").Output()
	if err != nil || resolvePath(strings.TrimSpace(string(prefix))) != resolvePath(e.Destination) || !reflect.DeepEqual(os.Environ(), before) {
		t.Fatalf("prefix %q %v", prefix, err)
	}
}

func TestUVToolchainRefusesDiscoveryWhenPMPythonIsMissing(t *testing.T) {
	root, _, facts, target, digest := peInstalledUV(t)
	t.Setenv(EnvDisableLazyInstalls, "1")
	t.Setenv("UV_PYTHON", filepath.Join(root, "ambient-python"))
	ctx := context.Background()
	if _, _, found, err := Toolchain(ctx, false, false); err != nil || found {
		t.Fatalf("read only probe %v %v", found, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "store", "python-*")); len(matches) != 0 {
		t.Fatalf("probe installed %v", matches)
	}
	if _, _, _, err := Toolchain(ctx, true, false); err == nil || !strings.Contains(err.Error(), "python") {
		t.Fatalf("realizing probe %v", err)
	}
	entry := filepath.Join(root, "store", "missing-binary")
	os.MkdirAll(entry, 0o777)
	if err := facts.Record("python", "test", "missing-binary", EnvDiff{}, filepath.Join(root, "store"), FactRecord{Target: &target, Artifacts: []string{digest}}); err != nil {
		t.Fatal(err)
	}
	recorded := peRead(t, facts.Path)
	if _, _, found, err := Toolchain(ctx, false, false); err != nil || found {
		t.Fatalf("missing binary probe %v %v", found, err)
	}
	var ie *InstallError
	if _, _, _, err := Toolchain(ctx, true, false); !errors.As(err, &ie) || !strings.Contains(err.Error(), "lazy installs are disabled: python") {
		t.Fatalf("missing binary realize %v", err)
	}
	children, _ := os.ReadDir(entry)
	if peRead(t, facts.Path) != recorded || len(children) != 0 {
		t.Fatal("refused realization wrote state")
	}
}

// Copies a self contained interpreter tree into a store directory without tests, caches and GUI libraries
func peLinkedPython(t *testing.T, source, destination string) string {
	t.Helper()
	skip := map[string]bool{"site-packages": true, "test": true, "__pycache__": true, "idlelib": true, "tkinter": true, "include": true, "share": true, "pkgconfig": true}
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		gui := d.IsDir() && filepath.Base(filepath.Dir(path)) == "lib" && (strings.HasPrefix(name, "tcl") || strings.HasPrefix(name, "tk") || strings.HasPrefix(name, "itcl") || strings.HasPrefix(name, "thread"))
		if path != source && (skip[name] || gui) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o777)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if os.Link(path, target) == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(destination, "bin", "python")
	if !existsOrLink(python) {
		os.Symlink("python3", python)
	}
	return python
}

func TestUVToolchainProjectEnvironmentReplacesGenerationWhenPinnedPythonMoves(t *testing.T) {
	root, uv, facts, target, digest := peInstalledUV(t)
	store := filepath.Join(root, "store")
	uvDigest, _ := TreeDigest(filepath.Dir(uv))
	facts.Record("uv", "test", "uv", EnvDiff{}, store, FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &uvDigest})
	peFixturePython(t)
	pinnedRoot := filepath.Dir(filepath.Dir(pePinnedPython(t)))
	interpreters := []string{peLinkedPython(t, pinnedRoot, filepath.Join(store, "python-a")), peLinkedPython(t, pinnedRoot, filepath.Join(store, "python-b"))}
	project := filepath.Join(root, "project")
	wheel := peWheel(t, root, "side_dep", "", nil, nil)
	peWrite(t, filepath.Join(project, "pyproject.toml"), "[project]\nname=\"pm-generation-proof\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"side-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=[\""+filepath.ToSlash(filepath.Dir(wheel))+"\"]\n")
	env := map[string]string{}
	for k, v := range pyEnviron() {
		if !strings.HasPrefix(k, "UV_") && !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" {
			env[k] = v
		}
	}
	env["UV_CACHE_DIR"], env["UV_PYTHON_DOWNLOADS"] = filepath.Join(root, "cache"), "never"
	peRun(t, []string{uv, "lock", "--python", interpreters[0]}, project, env)
	locked := peRead(t, filepath.Join(project, "uv.lock"))
	selectionRoot := filepath.Join(root, "selection")
	ctx := context.Background()
	record := func(interpreter string) {
		entry := filepath.Dir(filepath.Dir(interpreter))
		d, err := TreeDigest(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := facts.Record("python", "test", filepath.Base(entry), EnvDiff{}, store, FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &d}); err != nil {
			t.Fatal(err)
		}
	}
	var chosen []string
	for _, interpreter := range []string{interpreters[0], interpreters[1], interpreters[0]} {
		record(interpreter)
		selected, err := ensureProjectEnvironment(ctx, EnsureProjectEnvironmentOptions{Name: "test-environment", Project: project, Root: selectionRoot, Explicit: true})
		if err != nil {
			t.Fatal(err)
		}
		chosen = append(chosen, selected)
		if got := peRun(t, []string{selected, "-I", "-c", "import sys, side_dep; print(sys.base_prefix)"}, project, env); got != filepath.Dir(filepath.Dir(interpreter)) {
			t.Fatalf("base prefix %q", got)
		}
		if peRead(t, filepath.Join(project, "uv.lock")) != locked {
			t.Fatal("project lock changed")
		}
	}
	if chosen[0] == chosen[1] || chosen[1] == chosen[2] {
		t.Fatalf("generations %v", chosen)
	}
	record(interpreters[1])
	active := peRead(t, filepath.Join(selectionRoot, "active.json"))
	peSwap(t, &uvToolchain, func(ctx context.Context, realize, explicit bool) (string, string, bool, error) {
		u, p, found, err := Toolchain(ctx, realize, explicit)
		if realize {
			return "", "", false, errors.New("build failed")
		}
		return u, p, found, err
	})
	if _, err := ensureProjectEnvironment(ctx, EnsureProjectEnvironmentOptions{Name: "test-environment", Project: project, Root: selectionRoot, Explicit: true}); err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("failed replacement %v", err)
	}
	if peRead(t, filepath.Join(selectionRoot, "active.json")) != active {
		t.Fatal("failed replacement changed the selection")
	}
	if python, ok, err := EnvironmentPython("test-environment", selectionRoot); err != nil || !ok || python != chosen[2] {
		t.Fatalf("selected %q %v %v", python, ok, err)
	}
}

func TestUVToolchainEnvironmentProbeNeedsNoPMRuntime(t *testing.T) {
	root := t.TempDir()
	if python, ok, err := EnvironmentPython("test", root); err != nil || ok || python != "" {
		t.Fatalf("python %q %v %v", python, ok, err)
	}
	if tool, ok, err := PythonTool("test", "test", root); err != nil || ok || tool != "" {
		t.Fatalf("tool %q %v %v", tool, ok, err)
	}
	var py *PyError
	if _, err := ensureEnvironment(context.Background(), EnsureEnvironmentOptions{Name: "../escape", Requirements: []string{"example==1"}, Explicit: true}); !errors.As(err, &py) || py.Class != "ValueError" {
		t.Fatalf("escape %v", err)
	}
}
