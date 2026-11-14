package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestFindGitWorktreeFindsDotgit(t *testing.T) {
	resetCaches(t)
	repo := filepath.Join(t.TempDir(), "repo")
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	sub := filepath.Join(repo, "src", "deep")
	must(t, os.MkdirAll(sub, 0o755))
	if got := FindGitWorktree(sub); got != repo {
		t.Fatalf("got %q", got)
	}
}

func TestNearestRootFindsFirstMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	deep := filepath.Join(root, "src", "pkg")
	must(t, os.MkdirAll(deep, 0o755))
	writeFile(t, filepath.Join(root, "pyproject.toml"), "")
	if got := NearestRoot(filepath.Join(deep, "mod.py"), []string{"pyproject.toml"}, nil, ""); got != root {
		t.Fatalf("got %q", got)
	}
}

func TestNearestRootSkipsPackageDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	pkg := filepath.Join(root, "icebath_cli")
	writeFile(t, filepath.Join(root, "pyproject.toml"), "")
	writeFile(t, filepath.Join(pkg, "__init__.py"), "")
	writeFile(t, filepath.Join(pkg, "setup.py"), "")
	if got := NearestRoot(filepath.Join(pkg, "main.py"), []string{"pyproject.toml", "setup.py"}, nil, ""); got != root {
		t.Fatalf("got %q", got)
	}
}

func TestNearestRootExcludesAndCeiling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	app := filepath.Join(root, "app")
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	writeFile(t, filepath.Join(app, "deno.json"), "{}")
	if got := NearestRoot(filepath.Join(app, "a.ts"), []string{"package.json"}, []string{"deno.json"}, ""); got != "" {
		t.Fatalf("exclude marker must gate the file off, got %q", got)
	}
	if got := rootOrWorkspace(filepath.Join(app, "a.ts"), root, jsMarkers, denoExcludes); got != "" {
		t.Fatalf("typescript root must be gated off by deno.json, got %q", got)
	}
	if got := NearestRoot(filepath.Join(app, "a.ts"), []string{"package.json"}, nil, app); got != "" {
		t.Fatalf("walk must stop at the ceiling, got %q", got)
	}
}

func TestResolveWorkspaceForFileUsesCwdFirst(t *testing.T) {
	resetCaches(t)
	repo := filepath.Join(t.TempDir(), "repo")
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	f := writeFile(t, filepath.Join(repo, "x.py"), "")
	t.Chdir(repo)
	if root, gated := ResolveWorkspaceForFile(f, ""); root != repo || !gated {
		t.Fatalf("got %q %v", root, gated)
	}
}

func TestResolveWorkspaceForFileSurvivesDeletedCwd(t *testing.T) {
	resetCaches(t)
	if runtime.GOOS == "windows" {
		t.Skip("a process cwd cannot be removed on Windows")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	f := writeFile(t, filepath.Join(repo, "x.py"), "")
	scratch := filepath.Join(tmp, "scratch")
	must(t, os.Mkdir(scratch, 0o755))
	t.Chdir(scratch)
	must(t, os.Remove(scratch))
	if _, err := os.Getwd(); err == nil {
		t.Fatal("cwd still resolves")
	}
	if root, gated := ResolveWorkspaceForFile(f, ""); root != repo || !gated {
		t.Fatalf("got %q %v", root, gated)
	}
	if got := shortPath(f); got != f {
		t.Fatalf("short path %q", got)
	}
}

func TestNormalizePathExpandsTilde(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux expanduser semantics")
	}
	t.Setenv("HOME", "/home/user")
	if got := NormalizePath("~/x.py"); got != "/home/user/x.py" {
		t.Fatalf("got %q", got)
	}
}

func TestFindGitWorktreeCacheIsCapped(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	for i := range workspaceCacheCap + 2 {
		d := filepath.Join(tmp, "d"+strconv.Itoa(i))
		must(t, os.Mkdir(d, 0o755))
		FindGitWorktree(d)
	}
	workspaceMu.Lock()
	n := len(workspaceCache)
	workspaceMu.Unlock()
	if n > workspaceCacheCap {
		t.Fatalf("cache grew to %d", n)
	}
}

func TestIsInsideWorkspaceAndTrust(t *testing.T) {
	resetCaches(t)
	tmp := t.TempDir()
	if !IsInsideWorkspace(filepath.Join(tmp, "a", "b"), tmp) || IsInsideWorkspace(tmp+"x", tmp) || !IsInsideWorkspace(tmp, tmp) {
		t.Fatal("inside check")
	}
	launch := filepath.Join(tmp, "launch")
	must(t, os.MkdirAll(filepath.Join(launch, ".git"), 0o755))
	nested := filepath.Join(launch, "vendor", "clone")
	must(t, os.MkdirAll(filepath.Join(nested, ".git"), 0o755))
	listed := filepath.Join(tmp, "listed", "proj")
	must(t, os.MkdirAll(listed, 0o755))
	operators := OperatorWorkspaceRoots([]func() (string, error){func() (string, error) { return filepath.Join(launch, "src"), nil }})
	if !operators[launch] || len(operators) != 1 {
		t.Fatalf("operator roots %v", operators)
	}
	trusted := []string{filepath.Join(tmp, "listed")}
	if !IsTrustedWorkspace(launch, trusted, operators) || !IsTrustedWorkspace(listed, trusted, operators) {
		t.Fatal("operator and listed roots are trusted")
	}
	if IsTrustedWorkspace(nested, trusted, operators) {
		t.Fatal("a nested clone with its own .git is not trusted")
	}
	t.Setenv("HOME", launch)
	if roots := OperatorWorkspaceRoots([]func() (string, error){func() (string, error) { return launch, nil }}); len(roots) != 0 {
		t.Fatalf("a repo at or above home never counts: %v", roots)
	}
}
