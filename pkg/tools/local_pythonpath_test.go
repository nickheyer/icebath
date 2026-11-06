package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Replaces the cached running venv site packages for one test
func withRunningSitePackages(t *testing.T, dirs []string) {
	prev := runningSitePackages
	runningSitePackages = func() ([]string, error) { return dirs, nil }
	t.Cleanup(func() { runningSitePackages = prev })
}

// Links a directory through a symlink or a Windows junction, skipping when the host refuses both
func makeDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		command := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		if out, jerr := exec.Command(command, "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); jerr == nil {
			return
		} else {
			t.Logf("mklink /J %s", out)
		}
	}
	t.Skipf("directory link unavailable on this host: %v", err)
}

// Creates the physical repo checkout used by the junction cases
func physicalRepoRoot(t *testing.T, tmp string) string {
	root := filepath.Join(tmp, "physical-home", "icebath-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// Real temporary directory with every link in its spelling resolved
func realTempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func joinPP(entries ...string) string { return strings.Join(entries, string(os.PathListSeparator)) }

func mustStrip(t *testing.T, child map[string]string) {
	t.Helper()
	if err := stripIcebathOwnedPythonpath(child); err != nil {
		t.Fatal(err)
	}
}

func resolvedPath(t *testing.T, p string) string {
	r, err := pypath.Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A backslash spelled running venv site packages entry goes while a user path stays
func TestWindowsIcebathOwnedPathsStripped(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "venv")
	owned, err := venvSitePackagesDirs(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "pyvenv.cfg"), []byte("version = 3.12.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if owned, err = venvSitePackagesDirs(prefix); err != nil {
			t.Fatal(err)
		}
	}
	if len(owned) == 0 {
		t.Fatal("no running venv site packages")
	}
	withRunningSitePackages(t, owned)
	venvSite := owned[len(owned)-1]
	user := "/home/user/lib"
	if runtime.GOOS == "windows" {
		user = `D:\\user\\lib`
	}
	child := map[string]string{"PYTHONPATH": joinPP(venvSite, user)}
	mustStrip(t, child)
	entries := strings.Split(child["PYTHONPATH"], string(os.PathListSeparator))
	if slices.Contains(entries, venvSite) || !slices.Contains(entries, user) {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// Empty values, empty components, raw user spellings and a missing key pass through untouched
func TestStripIcebathOwnedPythonpathLeavesUserValues(t *testing.T) {
	empty := map[string]string{"PYTHONPATH": ""}
	mustStrip(t, empty)
	if v, ok := empty["PYTHONPATH"]; !ok || v != "" {
		t.Fatalf("empty PYTHONPATH = %q %v", v, ok)
	}
	cwd := joinPP("/foo", "", "/bar")
	component := map[string]string{"PYTHONPATH": cwd}
	mustStrip(t, component)
	if component["PYTHONPATH"] != cwd {
		t.Fatalf("empty component PYTHONPATH = %q", component["PYTHONPATH"])
	}
	raw := joinPP(" /opt/user-lib ", "relative/../lib", "", "/opt/user-lib", "/opt/user-lib")
	spelled := map[string]string{"PYTHONPATH": raw}
	mustStrip(t, spelled)
	if spelled["PYTHONPATH"] != raw {
		t.Fatalf("raw spelling PYTHONPATH = %q", spelled["PYTHONPATH"])
	}
	missing := map[string]string{"PATH": "/usr/bin"}
	mustStrip(t, missing)
	if _, ok := missing["PYTHONPATH"]; ok {
		t.Fatal("missing PYTHONPATH created")
	}
}

// A base interpreter sanitizer strips the validated repo venv site packages and drops VIRTUAL_ENV
func TestBasePythonSanitizerUsesValidatedSeparateRuntimeVenv(t *testing.T) {
	env := newTestToolEnv(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "icebath-agent")
	venv := filepath.Join(repo, "venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("version = 3.11\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	withRuntimeRepoRoot(t, repo, []string{repo})
	withRunningSitePackages(t, nil)
	unrelated := "/custom/lib/python3.13/site-packages"
	result, err := SanitizeSubprocessEnv(env, map[string]string{"VIRTUAL_ENV": venv, "PYTHONPATH": joinPP(site, unrelated)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["PYTHONPATH"] != unrelated {
		t.Fatalf("PYTHONPATH = %q", result["PYTHONPATH"])
	}
	if _, ok := result["VIRTUAL_ENV"]; ok {
		t.Fatal("VIRTUAL_ENV survived")
	}
}

// An inherited VIRTUAL_ENV outside the repo cannot claim PYTHONPATH ownership
func TestUnrelatedVirtualEnvIsNotRuntimeProvenance(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "icebath-agent")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(tmp, "user-venv")
	site := filepath.Join(user, "Lib", "site-packages")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "pyvenv.cfg"), []byte("version = 3.13\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withRuntimeRepoRoot(t, repo, []string{repo})
	withRunningSitePackages(t, nil)
	child := map[string]string{"VIRTUAL_ENV": user, "PYTHONPATH": site}
	mustStrip(t, child)
	if child["PYTHONPATH"] != site {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// Every subprocess env builder strips the running venv site packages and keeps user entries
func TestBuildersStripIcebathVenvPythonpath(t *testing.T) {
	builders := map[string]func(env *Env) (map[string]string, error){
		"makeRunEnv": func(env *Env) (map[string]string, error) { return makeRunEnv(env, map[string]string{}) },
		"SanitizeSubprocessEnv": func(env *Env) (map[string]string, error) {
			return SanitizeSubprocessEnv(env, environMap(), nil)
		},
		"IcebathSubprocessEnv": func(env *Env) (map[string]string, error) {
			return icebathSubprocessEnv(env, false, nil)
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			env := newTestToolEnv(t)
			venvSite := filepath.Join(t.TempDir(), "venv", "lib", "python3.12", "site-packages")
			withRunningSitePackages(t, []string{venvSite})
			t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+"/bin")
			t.Setenv("PYTHONPATH", joinPP(venvSite, "/home/user/my-lib"))
			result, err := build(env)
			if err != nil {
				t.Fatal(err)
			}
			var entries []string
			if pp := result["PYTHONPATH"]; pp != "" {
				entries = strings.Split(pp, string(os.PathListSeparator))
			}
			if slices.Contains(entries, venvSite) || !slices.Contains(entries, "/home/user/my-lib") {
				t.Fatalf("PYTHONPATH entries %q", entries)
			}
		})
	}
}

// The selective strip removes the runtime site packages and keeps another version's site packages
func TestStripKeepsOtherVersionSitePackages(t *testing.T) {
	venvSite := filepath.Join(t.TempDir(), "venv", "lib", "python3.12", "site-packages")
	withRunningSitePackages(t, []string{venvSite})
	other := "/opt/other-venv/lib/python3.99/site-packages"
	child := map[string]string{"PATH": "/usr/bin", "HOME": "/home/user", "PYTHONPATH": joinPP(venvSite, other, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != joinPP(other, "/home/user/my-lib") {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// A direct child of the real repo root is a user path and survives
func TestRepoRootDirectChildPreserved(t *testing.T) {
	source := runtimeSourceRootDir()
	if source == "" {
		t.Fatal("source checkout unknown")
	}
	if !slices.ContainsFunc(runtimeRepoRootAliases(), func(a string) bool { return SamePath(a, resolvedPath(t, source)) }) {
		t.Fatalf("aliases %q miss the source root %q", runtimeRepoRootAliases(), source)
	}
	direct := filepath.Join(resolvedPath(t, source), "pkg")
	child := map[string]string{"PYTHONPATH": joinPP(direct, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != joinPP(direct, "/home/user/my-lib") {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
	root := map[string]string{"PYTHONPATH": joinPP(source, "/home/user/my-lib")}
	mustStrip(t, root)
	if root["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("repo root kept %q", root["PYTHONPATH"])
	}
}

// The configured home spelling launchers emit is an alias and only the exact entry is stripped
func TestConfiguredHomeAliasMatchesLauncherOutput(t *testing.T) {
	tmp := realTempDir(t)
	physicalHome := filepath.Join(tmp, "physical-home")
	physical := physicalRepoRoot(t, tmp)
	configured := filepath.Join(tmp, "configured-home")
	makeDirectoryLink(t, configured, physicalHome)
	t.Setenv("ICEBATH_HOME", configured)
	launcher := filepath.Join(configured, "icebath-agent")
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, configured)
	if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, launcher) }) {
		t.Fatalf("aliases %q miss %q", aliases, launcher)
	}
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	nested := filepath.Join(launcher, "user-data")
	child := map[string]string{"PYTHONPATH": joinPP(launcher, nested, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != joinPP(nested, "/home/user/my-lib") {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// A profile re homed ICEBATH_HOME still recovers the lexical junction repo root
func TestProfileRehomeKeepsJunctionLexicalAlias(t *testing.T) {
	tmp := realTempDir(t)
	physicalHome := filepath.Join(tmp, "physical-home")
	physical := filepath.Join(physicalHome, "icebath-agent")
	coder := filepath.Join(physicalHome, "profiles", "coder")
	for _, dir := range []string{physical, coder} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(coder, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(tmp, "configured-home")
	makeDirectoryLink(t, configured, physicalHome)
	t.Setenv("ICEBATH_HOME", configured)
	lexical := filepath.Join(configured, "icebath-agent")
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, filepath.Join(configured, "profiles", "coder"))
	if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, lexical) }) {
		t.Fatalf("aliases %q miss %q", aliases, lexical)
	}
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	child := map[string]string{"PYTHONPATH": joinPP(lexical, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// A repo level junction under the configured home is recovered by strict identity
func TestRepoLevelJunctionRecoversLexicalAlias(t *testing.T) {
	tmp := realTempDir(t)
	physical := physicalRepoRoot(t, tmp)
	configured := filepath.Join(tmp, "configured-home")
	if err := os.MkdirAll(configured, 0o755); err != nil {
		t.Fatal(err)
	}
	lexical := filepath.Join(configured, "icebath-agent")
	makeDirectoryLink(t, lexical, physical)
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, configured)
	if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, lexical) }) {
		t.Fatalf("aliases %q miss %q", aliases, lexical)
	}
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	child := map[string]string{"PYTHONPATH": joinPP(lexical, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// Same named directories with no identity proof are never aliased or stripped
func TestSameNamedNonOwnedDirectoriesPreserved(t *testing.T) {
	tmp := realTempDir(t)
	physical := physicalRepoRoot(t, tmp)
	configured := filepath.Join(tmp, "configured-home")
	unrelated := filepath.Join(tmp, "user-tools", "icebath-agent")
	for _, dir := range []string{filepath.Join(configured, "icebath-agent"), unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, configured)
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	for _, lookalike := range []string{filepath.Join(configured, "icebath-agent"), unrelated} {
		if slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, lookalike) }) {
			t.Fatalf("lookalike %q aliased in %q", lookalike, aliases)
		}
		child := map[string]string{"PYTHONPATH": joinPP(lookalike, "/home/user/my-lib")}
		mustStrip(t, child)
		if child["PYTHONPATH"] != joinPP(lookalike, "/home/user/my-lib") {
			t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
		}
	}
}

// A profile home under a root with a repo level junction derives the root and recovers the lexical repo
func TestProfileHomeWithRepoLevelJunction(t *testing.T) {
	tmp := realTempDir(t)
	physical := physicalRepoRoot(t, tmp)
	configuredRoot := filepath.Join(tmp, "configured-root")
	coder := filepath.Join(configuredRoot, "profiles", "coder")
	if err := os.MkdirAll(coder, 0o755); err != nil {
		t.Fatal(err)
	}
	lexical := filepath.Join(configuredRoot, "icebath-agent")
	makeDirectoryLink(t, lexical, physical)
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, coder)
	if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, lexical) }) {
		t.Fatalf("aliases %q miss %q", aliases, lexical)
	}
	if slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, filepath.Join(coder, "icebath-agent")) }) {
		t.Fatalf("aliases %q hold the profile spelling", aliases)
	}
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	child := map[string]string{"PYTHONPATH": joinPP(lexical, "/home/user/my-lib")}
	mustStrip(t, child)
	if child["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// The lexical repo venv validates after junction recovery and its site packages go with the repo root
func TestValidatedRuntimeVenvLexicalAfterRepoRecovery(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := realTempDir(t)
	physical := physicalRepoRoot(t, tmp)
	venv := filepath.Join(physical, "venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(tmp, "configured-home")
	if err := os.MkdirAll(configured, 0o755); err != nil {
		t.Fatal(err)
	}
	lexical := filepath.Join(configured, "icebath-agent")
	makeDirectoryLink(t, lexical, physical)
	aliases := buildIcebathRepoRootAliases(resolvedPath(t, physical), physical, configured)
	if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, lexical) }) {
		t.Fatalf("aliases %q miss %q", aliases, lexical)
	}
	withRuntimeRepoRoot(t, physical, aliases)
	withRunningSitePackages(t, nil)
	lexicalVenv := filepath.Join(lexical, "venv")
	validated, ok, err := ValidatedRuntimeVenv(map[string]string{"VIRTUAL_ENV": lexicalVenv})
	if err != nil || !ok || !SamePath(validated, lexicalVenv) {
		t.Fatalf("validated = %q %v %v", validated, ok, err)
	}
	site, err := pm.SitePackages(lexicalVenv)
	if err != nil {
		t.Fatal(err)
	}
	child := map[string]string{"PYTHONPATH": joinPP(lexical, site, "/home/user/my-lib"), "VIRTUAL_ENV": lexicalVenv}
	mustStrip(t, child)
	if child["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("PYTHONPATH = %q", child["PYTHONPATH"])
	}
}

// A damaged runtime dependency record surfaces from the strip and keeps the markers in place
func TestStripPropagatesDamagedRuntimeRecord(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	withRuntimeRepoRoot(t, root, []string{root})
	withRunningSitePackages(t, nil)
	facts := pm.EnvironmentRuntimeFactsPath(t.Context(), root)
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := map[string]string{"PYTHONPATH": "/home/user/my-lib", "VIRTUAL_ENV": filepath.Join(root, "venv")}
	if err := stripIcebathOwnedPythonpathAndRuntimeMarkers(child); err == nil {
		t.Fatal("damaged record swallowed")
	}
	if child["VIRTUAL_ENV"] == "" || child["PYTHONPATH"] != "/home/user/my-lib" {
		t.Fatalf("child changed %q", child)
	}
}

// Writes an undecodable runtime dependency record under a fresh repo root and returns the venv it names
func damageRuntimeRecord(t *testing.T) string {
	root := t.TempDir()
	withRuntimeRepoRoot(t, root, []string{root})
	withRunningSitePackages(t, nil)
	facts := pm.EnvironmentRuntimeFactsPath(t.Context(), root)
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "venv")
}

// Every env builder hands the damaged runtime dependency record error to its caller like the propagated RuntimeError
func TestBuildersPropagateDamagedRuntimeRecord(t *testing.T) {
	builders := map[string]func(env *Env) (map[string]string, error){
		"finalizeChildEnv": func(env *Env) (map[string]string, error) { return finalizeChildEnv(env, environMap()) },
		"makeRunEnv":       func(env *Env) (map[string]string, error) { return makeRunEnv(env, map[string]string{}) },
		"SanitizeSubprocessEnv": func(env *Env) (map[string]string, error) {
			return SanitizeSubprocessEnv(env, environMap(), nil)
		},
		"icebathSubprocessEnv": func(env *Env) (map[string]string, error) { return icebathSubprocessEnv(env, false, nil) },
		"ServedProfileChildEnv": func(env *Env) (map[string]string, error) {
			return ServedProfileChildEnv(t.Context(), env, nil, "", false)
		},
		"IcebathSubprocessEnv": func(env *Env) (out map[string]string, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err, _ = rec.(error)
				}
			}()
			return IcebathSubprocessEnv(env, false, nil), nil
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			env := newTestToolEnv(t)
			t.Setenv("VIRTUAL_ENV", damageRuntimeRecord(t))
			t.Setenv("PYTHONPATH", "/home/user/my-lib")
			if out, err := build(env); err == nil {
				t.Fatalf("damaged record swallowed, env %q", out["PYTHONPATH"])
			}
		})
	}
}

// A running venv whose pyvenv.cfg is not UTF-8 falls back to the prefix site packages of the pinned interpreter
func TestRunningSitePackagesFallBackToPinnedInterpreter(t *testing.T) {
	prefix := t.TempDir()
	if err := os.WriteFile(filepath.Join(prefix, "pyvenv.cfg"), []byte("version = 3.\xff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := venvSitePackagesDirs(prefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{prefix, filepath.Join(prefix, "Lib", "site-packages")}
	if runtime.GOOS != "windows" {
		major, minor, err := pm.PinnedPythonVersion()
		if err != nil {
			t.Fatal(err)
		}
		want = []string{filepath.Join(prefix, "lib", "python"+major+"."+minor, "site-packages")}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("site packages %q, want %q", got, want)
	}
}

// Literal PYTHONPATH rows keep every byte except exact repo root and running site packages entries
func TestPythonpathLiteralPolicy(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "icebath-agent")
	site := filepath.Join(t.TempDir(), "venv", "lib", "python3.12", "site-packages")
	withRuntimeRepoRoot(t, repo, []string{repo})
	withRunningSitePackages(t, []string{site})
	locations := map[string]string{"REPO": repo, "SITE": site}
	rows := []struct{ entries, expected []string }{
		{[]string{"SITE", "/user/lib"}, []string{"/user/lib"}},
		{[]string{"REPO", "/user/lib"}, []string{"/user/lib"}},
		{[]string{"SITE", "/user/lib", "SITE", "/user/lib"}, []string{"/user/lib", "/user/lib"}},
		{[]string{"SITE", "REPO"}, nil},
		{[]string{"/first", "REPO", "", "SITE", "/last"}, []string{"/first", "", "/last"}},
		{[]string{" /opt/user-lib ", "relative/../lib", "", "/opt/user-lib", "/opt/user-lib"}, []string{" /opt/user-lib ", "relative/../lib", "", "/opt/user-lib", "/opt/user-lib"}},
		{[]string{"/nix/store/user-plugin/lib/python3.12/site-packages", "/old/lib/python2.7/site-packages"}, []string{"/nix/store/user-plugin/lib/python3.12/site-packages", "/old/lib/python2.7/site-packages"}},
		{[]string{"/opt/tools/python3.13/bin", "/opt/downloads/python3.13", "/custom/python3.13"}, []string{"/opt/tools/python3.13/bin", "/opt/downloads/python3.13", "/custom/python3.13"}},
		{[]string{""}, []string{""}},
		{nil, nil},
	}
	for i, row := range rows {
		child := map[string]string{}
		if row.entries != nil {
			mapped := make([]string, len(row.entries))
			for j, e := range row.entries {
				mapped[j] = e
				if loc, ok := locations[e]; ok {
					mapped[j] = loc
				}
			}
			child["PYTHONPATH"] = joinPP(mapped...)
		}
		if err := stripIcebathOwnedPythonpath(child); err != nil {
			t.Fatal(err)
		}
		got, present := child["PYTHONPATH"]
		if row.expected == nil {
			if present {
				t.Fatalf("row %d PYTHONPATH %q, want absent", i, got)
			}
		} else if !present || got != joinPP(row.expected...) {
			t.Fatalf("row %d PYTHONPATH %q, want %q", i, got, joinPP(row.expected...))
		}
	}
}

// Paths below the repo root or the running site packages are user entries and stay
func TestPythonpathDescendantsAreNotOwned(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "icebath-agent")
	site := filepath.Join(t.TempDir(), "venv", "lib", "python3.12", "site-packages")
	withRuntimeRepoRoot(t, repo, []string{repo})
	withRunningSitePackages(t, []string{site})
	entries := []string{filepath.Join(site, "user-path"), filepath.Join(repo, "tools"), filepath.Join(repo, "tools", "environments"), "/opt/other-venv/lib/python3.99/site-packages"}
	child := map[string]string{"PYTHONPATH": joinPP(entries...)}
	if err := stripIcebathOwnedPythonpath(child); err != nil {
		t.Fatal(err)
	}
	if child["PYTHONPATH"] != joinPP(entries...) {
		t.Fatalf("PYTHONPATH %q", child["PYTHONPATH"])
	}
}

// Launcher home, repo and unrelated links decide whether the lexical repo spelling is stripped
func TestLauncherAliasProvenance(t *testing.T) {
	for _, linkAt := range []string{"home", "repo", "unrelated"} {
		for _, profile := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%v", linkAt, profile), func(t *testing.T) {
				tmp := t.TempDir()
				physicalHome := filepath.Join(tmp, "physical-home")
				physicalRoot := filepath.Join(physicalHome, "icebath-agent")
				if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				configured := filepath.Join(tmp, "configured-home")
				switch linkAt {
				case "home":
					makeDirectoryLink(t, configured, physicalHome)
				case "repo":
					if err := os.Mkdir(configured, 0o755); err != nil {
						t.Fatal(err)
					}
					makeDirectoryLink(t, filepath.Join(configured, "icebath-agent"), physicalRoot)
				default:
					if err := os.MkdirAll(filepath.Join(configured, "icebath-agent"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Join(configured, "profiles", "coder"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configured, "profiles", "coder", "config.yaml"), []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				unrelated := filepath.Join(tmp, "user-tools", "icebath-agent")
				if err := os.MkdirAll(unrelated, 0o755); err != nil {
					t.Fatal(err)
				}
				lexicalRoot := filepath.Join(configured, "icebath-agent")
				t.Setenv("ICEBATH_HOME", configured)
				activeHome := configured
				if profile {
					activeHome = filepath.Join(configured, "profiles", "coder")
				}
				aliases := buildIcebathRepoRootAliases(resolvedPath(t, physicalRoot), physicalRoot, activeHome)
				withRuntimeRepoRoot(t, physicalRoot, aliases)
				withRunningSitePackages(t, nil)
				entries := []string{lexicalRoot, filepath.Join(lexicalRoot, "user-data"), unrelated, filepath.Join(activeHome, "not-the-repo")}
				child := map[string]string{"PYTHONPATH": joinPP(entries...)}
				if err := stripIcebathOwnedPythonpath(child); err != nil {
					t.Fatal(err)
				}
				want := entries[1:]
				if linkAt == "unrelated" {
					want = entries
				}
				if child["PYTHONPATH"] != joinPP(want...) {
					t.Fatalf("PYTHONPATH %q, want %q", child["PYTHONPATH"], joinPP(want...))
				}
				if profile && slices.Contains(aliases, filepath.Join(activeHome, "icebath-agent")) {
					t.Fatalf("profile repo spelling in aliases %q", aliases)
				}
			})
		}
	}
}

// Writes a pyvenv.cfg naming version and returns the venv's site packages, created on disk
func makeTestVenv(t *testing.T, venv, version string) string {
	t.Helper()
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("version = "+version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	return site
}

// The recorded generation alone proves runtime ownership, neither an alias nor the user VIRTUAL_ENV does
func TestRuntimeProvenanceIsIndependentOfAliasesAndVirtualEnv(t *testing.T) {
	for _, hasFacts := range []bool{true, false} {
		t.Run(fmt.Sprint(hasFacts), func(t *testing.T) {
			env := newTestToolEnv(t)
			tmp := t.TempDir()
			root := filepath.Join(tmp, "icebath-agent")
			if err := os.MkdirAll(filepath.Join(root, "tools"), 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(tmp, "unrelated-repo-alias")
			withRuntimeRepoRoot(t, root, []string{alias})
			withRunningSitePackages(t, nil)
			runtimeVenv := filepath.Join(pm.InstallStateDir(t.Context(), resolvedPath(t, root)), "environments", "candidate", "venv")
			site := makeTestVenv(t, runtimeVenv, "3.14")
			if hasFacts {
				facts := pm.EnvironmentRuntimeFactsPath(t.Context(), resolvedPath(t, root))
				record := fmt.Sprintf(`{"packages": {"venv": {"environment": %s}}}`, strconv.Quote(runtimeVenv))
				if err := os.WriteFile(facts, []byte(record), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			userVenv := filepath.Join(tmp, "user-venv")
			userSite := filepath.Join(userVenv, "Lib", "site-packages")
			if err := os.MkdirAll(userSite, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(userVenv, "pyvenv.cfg"), []byte("version = 3.13\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			base := map[string]string{"VIRTUAL_ENV": userVenv, "PYTHONPATH": joinPP(site, alias, userSite)}
			result, err := SanitizeSubprocessEnv(env, base, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := joinPP(site, userSite)
			if hasFacts {
				want = userSite
			}
			if result["PYTHONPATH"] != want {
				t.Fatalf("PYTHONPATH %q, want %q", result["PYTHONPATH"], want)
			}
			if _, ok := result["VIRTUAL_ENV"]; ok || base["VIRTUAL_ENV"] != userVenv {
				t.Fatalf("VIRTUAL_ENV result %q base %q", result["VIRTUAL_ENV"], base["VIRTUAL_ENV"])
			}
		})
	}
}

// Lays out a sealed payload whose manifest ships venvRel beside the repo and returns the repo root
func makeTestPayload(t *testing.T, payload, venvRel string) string {
	t.Helper()
	root := filepath.Join(payload, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"repo": "repo", "venv": %s}`, strconv.Quote(venvRel))
	if err := os.WriteFile(filepath.Join(payload, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// A sealed payload's shipped venv owns its site packages before any generation is recorded
func TestSealedPayloadVenvIsRuntimeProvenanceWithoutFacts(t *testing.T) {
	env := newTestToolEnv(t)
	payload := filepath.Join(t.TempDir(), "payload")
	root := makeTestPayload(t, payload, "venv")
	shipped := makeTestVenv(t, filepath.Join(payload, "venv"), "3.14")
	withRuntimeRepoRoot(t, root, []string{root})
	withRunningSitePackages(t, nil)
	userSite := filepath.Join(t.TempDir(), "user-site")
	result, err := SanitizeSubprocessEnv(env, map[string]string{"PYTHONPATH": joinPP(shipped, userSite)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["PYTHONPATH"] != userSite {
		t.Fatalf("PYTHONPATH %q", result["PYTHONPATH"])
	}
}

// A payload venv that is missing or escapes its root proves nothing and raises nothing
func TestPayloadVenvThatIsNotRealIsNotProvenance(t *testing.T) {
	for shipped, rel := range map[string]string{"phantom": "no-such-venv", "escapes-root": "../outside/venv"} {
		t.Run(shipped, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			payload := filepath.Join(t.TempDir(), "payload")
			root := makeTestPayload(t, payload, rel)
			if shipped == "escapes-root" {
				makeTestVenv(t, filepath.Join(payload, rel), "3.14")
				if _, err := pm.PayloadVenv(root); err == nil {
					t.Fatal("escaping payload venv accepted")
				}
			}
			withRuntimeRepoRoot(t, root, nil)
			venv, ok, err := ValidatedRuntimeVenv(map[string]string{})
			if err != nil || ok || venv != "" {
				t.Fatalf("venv %q ok %v err %v", venv, ok, err)
			}
		})
	}
}
