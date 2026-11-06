package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Raw task id overrides surface their cwd and an unregistered task has none
func TestRegisteredTaskCwdOverrideKeysByRawTaskID(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	workspace := t.TempDir()
	if err := RegisterTaskEnvOverrides(env, "acp-session-7", map[string]any{"cwd": workspace}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ClearTaskEnvOverrides(env, "acp-session-7") })
	if got := RegisteredTaskCwdOverride(env, "acp-session-7"); got != workspace {
		t.Fatalf("override = %q, want %q", got, workspace)
	}
	if got := RegisteredTaskCwdOverride(env, "never-registered"); got != "" {
		t.Fatalf("unregistered override = %q", got)
	}
}

// Repo root entries go, user entries and empty components stay, an all owned value is removed
func TestStripIcebathOwnedPythonpathKeepsUserEntries(t *testing.T) {
	root := runtimeSourceRootDir()
	if root == "" {
		t.Fatal("source checkout unknown")
	}
	sep := string(os.PathListSeparator)
	child := map[string]string{"PYTHONPATH": strings.Join([]string{root, "/home/user/my-lib", "", "/opt/other/lib/python3.99/site-packages"}, sep)}
	if err := StripIcebathOwnedPythonpath(child); err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{"/home/user/my-lib", "", "/opt/other/lib/python3.99/site-packages"}, sep); child["PYTHONPATH"] != want {
		t.Fatalf("PYTHONPATH = %q, want %q", child["PYTHONPATH"], want)
	}
	only := map[string]string{"PYTHONPATH": root}
	if err := StripIcebathOwnedPythonpath(only); err != nil {
		t.Fatal(err)
	}
	if _, ok := only["PYTHONPATH"]; ok {
		t.Fatalf("all owned PYTHONPATH kept %q", only["PYTHONPATH"])
	}
	empty := map[string]string{"PATH": "/usr/bin"}
	if err := StripIcebathOwnedPythonpath(empty); err != nil {
		t.Fatal(err)
	}
	if _, ok := empty["PYTHONPATH"]; ok {
		t.Fatal("absent PYTHONPATH created")
	}
}

// Pathlib parts comparison ignores trailing and dot components and keeps the anchor
func TestSamePathComparesPathlibParts(t *testing.T) {
	cases := []struct {
		left, right string
		want        bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/", "/a/b", true},
		{"/a/./b", "/a//b", true},
		{"a/b", "/a/b", false},
		{"/a/b/..", "/a", false},
		{"//srv/a", "/srv/a", false},
		{"", ".", true},
	}
	for _, c := range cases {
		if got := SamePath(c.left, c.right); got != c.want {
			t.Errorf("SamePath(%q, %q) = %v, want %v", c.left, c.right, got, c.want)
		}
	}
}

// Points the runtime repo root and its aliases at a temporary install tree
func withRuntimeRepoRoot(t *testing.T, root string, aliases []string) {
	prevRoot, prevAliases := runtimeRepoRootDir, runtimeRepoRootAliases
	runtimeRepoRootDir = func() string { return root }
	runtimeRepoRootAliases = func() []string { return aliases }
	t.Cleanup(func() { runtimeRepoRootDir, runtimeRepoRootAliases = prevRoot, prevAliases })
}

// VIRTUAL_ENV counts only as the repo venv layout with a real pyvenv.cfg
func TestValidatedRuntimeVenvRequiresRepoVenvLayout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "junction")
	withRuntimeRepoRoot(t, root, []string{root, alias})
	venv := filepath.Join(root, "venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ValidatedRuntimeVenv(map[string]string{"VIRTUAL_ENV": venv}); err != nil || ok || got != "" {
		t.Fatalf("venv without pyvenv.cfg = %q %v %v", got, ok, err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /usr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ValidatedRuntimeVenv(map[string]string{"VIRTUAL_ENV": venv + "/"}); err != nil || !ok || got != venv {
		t.Fatalf("repo venv = %q %v %v", got, ok, err)
	}
	user := filepath.Join(t.TempDir(), "venv")
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "pyvenv.cfg"), []byte("home = /usr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ValidatedRuntimeVenv(map[string]string{"VIRTUAL_ENV": user}); err != nil || ok || got != "" {
		t.Fatalf("unrelated user venv = %q %v %v", got, ok, err)
	}
	if got, ok, err := ValidatedRuntimeVenv(map[string]string{}); err != nil || ok || got != "" {
		t.Fatalf("no VIRTUAL_ENV = %q %v %v", got, ok, err)
	}
}

// A damaged committed generation record raises instead of falling back to VIRTUAL_ENV
func TestValidatedRuntimeVenvPropagatesDamagedFacts(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	withRuntimeRepoRoot(t, root, []string{root})
	facts := pm.EnvironmentRuntimeFactsPath(t.Context(), root)
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ValidatedRuntimeVenv(map[string]string{"VIRTUAL_ENV": filepath.Join(root, "venv")}); err == nil || ok || got != "" {
		t.Fatalf("damaged facts = %q %v %v", got, ok, err)
	}
}

// Home relative spellings and a re homed profile root join the repo root aliases without duplicates
func TestBuildIcebathRepoRootAliasesRehomesUnderConfiguredHome(t *testing.T) {
	base := t.TempDir()
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	physicalHome := filepath.Join(realBase, "physical")
	repo := filepath.Join(physicalHome, "icebath-agent")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realBase, "configured")
	if err := os.Symlink(physicalHome, link); err != nil {
		t.Fatal(err)
	}
	got := buildIcebathRepoRootAliases(repo, repo, filepath.Join(link, "profiles", "work"))
	want := []string{repo, filepath.Join(link, "icebath-agent")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("aliases = %q, want %q", got, want)
	}
}
