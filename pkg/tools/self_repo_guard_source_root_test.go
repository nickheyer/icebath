package tools

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

const selfRepoProbeEnv = "ICEBATH_SELF_REPO_PROBE"

type selfRepoProbe struct {
	SourceRoot  string `json:"source_root"`
	PackageRoot string `json:"package_root"`
	Hit         bool   `json:"hit"`
	Message     string `json:"message"`
	InstallTree bool   `json:"install_tree"`
}

// Prints the source root, package root and guard verdict this binary sees from where it was placed
func selfRepoProbeEntry() {
	target := os.Getenv(selfRepoProbeEnv)
	if target == "" {
		return
	}
	hit, msg := DetectSelfRepoGitMutation(nil, "git checkout main", target, "")
	json.NewEncoder(os.Stdout).Encode(selfRepoProbe{RunningSourceRoot(), PackageRoot, hit, msg, IsInstallTree(filepath.Join(target, "pkg"))})
	os.Exit(0)
}

// Reports whether this test binary was built with trimmed source paths
func builtTrimmed(t *testing.T) bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("test binary carries no build info")
	}
	for _, s := range info.Settings {
		if s.Key == "-trimpath" {
			return s.Value == "true"
		}
	}
	return false
}

// Places this test binary at dest by hard link, copying when the link crosses devices
func placeTestBinary(t *testing.T, dest string) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Link(exe, dest) == nil {
		return
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Lays out a module tree holding this binary's source paths, with a git dir, a worktree git file or none
func fixtureCheckout(t *testing.T, git string) string {
	root := filepath.Join(t.TempDir(), "icebath")
	for _, rel := range []string{"pkg/config/facade_misc.go", "pkg/tools/self_repo_guard.go"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("package x\n"), 0o644)
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/nickheyer/icebath\n\ngo 1.27\n"), 0o644)
	switch git {
	case "dir":
		os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	case "file":
		os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /somewhere/.git/worktrees/wt\n"), 0o644)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Runs a placed binary from cwd with the probe aimed at target and decodes what it saw
func runSelfRepoProbe(t *testing.T, binary, cwd, target string, env ...string) selfRepoProbe {
	cmd := exec.Command(binary)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), selfRepoProbeEnv+"="+target, "ICEBATH_HOME="+t.TempDir(), "ICEBATH_INSTALL_ROOT=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe %s: %v %s", binary, err, out)
	}
	var p selfRepoProbe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	return p
}

// Compile time tree of a build without trimmed paths, the checkout every placement reports
func compiledTree(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	root, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// Source and package roots a placement reports, the compile time tree for a build without trimmed paths
func wantRoots(t *testing.T, source, pkgRoot string) (string, string) {
	if builtTrimmed(t) {
		return source, pkgRoot
	}
	tree := compiledTree(t)
	if _, err := os.Stat(filepath.Join(tree, ".git")); err != nil {
		return "", tree
	}
	return tree, tree
}

// Fails unless the probe reports the roots a placement implies
func checkRoots(t *testing.T, name string, p selfRepoProbe, source, pkgRoot string) {
	t.Helper()
	source, pkgRoot = wantRoots(t, source, pkgRoot)
	if p.SourceRoot != source || p.PackageRoot != pkgRoot {
		t.Fatalf("%s: source root %q package root %q, want %q %q", name, p.SourceRoot, p.PackageRoot, source, pkgRoot)
	}
}

// Places this test binary in a fresh install directory outside any checkout
func packagedInstall(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	placeTestBinary(t, filepath.Join(dir, "icebath"))
	return dir
}

// A trimmed build placed in a checkout guards that checkout and its tree, a packaged one guards nothing
func TestRunningSourceRootSurvivesTrimmedBuilds(t *testing.T) {
	trimmed := builtTrimmed(t)
	checkout := fixtureCheckout(t, "dir")
	placeTestBinary(t, filepath.Join(checkout, "dist", "icebath"))
	p := runSelfRepoProbe(t, filepath.Join(checkout, "dist", "icebath"), t.TempDir(), checkout)
	checkRoots(t, "checkout build", p, checkout, checkout)
	if trimmed && (!p.Hit || !strings.Contains(p.Message, "("+checkout+")") || !p.InstallTree) {
		t.Fatalf("guard inactive in the checkout it was built from: %+v", p)
	}

	archive := fixtureCheckout(t, "")
	placeTestBinary(t, filepath.Join(archive, "dist", "icebath"))
	p = runSelfRepoProbe(t, filepath.Join(archive, "dist", "icebath"), t.TempDir(), archive)
	checkRoots(t, "source without git", p, "", archive)
	if trimmed && (p.Hit || !p.InstallTree) {
		t.Fatalf("a tree without git must be an install tree with no guard: %+v", p)
	}

	installDir := packagedInstall(t)
	p = runSelfRepoProbe(t, filepath.Join(installDir, "icebath"), checkout, checkout)
	checkRoots(t, "packaged install inside a checkout cwd", p, "", installDir)
	if trimmed && (p.Hit || p.Message != "" || p.InstallTree) {
		t.Fatalf("a packaged install guards the checkout it was started in: %+v", p)
	}

	p = runSelfRepoProbe(t, filepath.Join(installDir, "icebath"), t.TempDir(), checkout, "ICEBATH_INSTALL_ROOT="+checkout)
	checkRoots(t, "steward install root", p, checkout, checkout)
	if trimmed && !p.Hit {
		t.Fatalf("guard inactive for the steward install root: %+v", p)
	}
}

// test_self_repo_guard.py TestSourceRootResolution.test_worktree_git_file_counts
func TestSourceRootResolutionWorktreeGitFileCounts(t *testing.T) {
	root := fixtureCheckout(t, "file")
	placeTestBinary(t, filepath.Join(root, "dist", "icebath"))
	p := runSelfRepoProbe(t, filepath.Join(root, "dist", "icebath"), t.TempDir(), root)
	checkRoots(t, "worktree git file", p, root, root)
	if builtTrimmed(t) && !p.Hit {
		t.Fatalf("worktree checkout unguarded: %+v", p)
	}
}

// test_self_repo_guard.py TestAllowsSafeCommands.test_packaged_install_is_inert
func TestAllowsSafeCommandsPackagedInstallIsInert(t *testing.T) {
	installDir := packagedInstall(t)
	target := t.TempDir()
	p := runSelfRepoProbe(t, filepath.Join(installDir, "icebath"), target, target)
	checkRoots(t, "packaged install", p, "", installDir)
	if builtTrimmed(t) && (p.Hit || p.Message != "") {
		t.Fatalf("packaged install guard fired: %+v", p)
	}
}
