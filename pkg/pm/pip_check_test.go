package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const pipCheckLock = `version = 1
requires-python = ">=3.11"

[manifest]
overrides = [
    { name = "Shared_Dep", specifier = "==2.0" },
    { name = "unused", marker = "sys_platform == 'never'" },
]

[[package]]
name = "pinned-parent"
version = "1.0"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "shared-dep"
version = "2.0"
source = { registry = "https://pypi.org/simple" }
`

const pipCheckOverridden = "The package `pinned-parent` requires `shared-dep==1.0`, but `2.0` is installed"

// uv pip check report text for the given incompatibility lines
func pipCheckReport(lines ...string) string {
	plural := "incompatibilities"
	if len(lines) == 1 {
		plural = "incompatibility"
	}
	return fmt.Sprintf("Checked 3 packages in 1ms\nFound %d %s\n", len(lines), plural) + strings.Join(lines, "\n") + "\n"
}

// Lock fixture written into a test directory
func pipCheckLockFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uv.lock")
	peWrite(t, path, pipCheckLock)
	return path
}

func TestPipCheckOverrideResolvedMismatchIsExpected(t *testing.T) {
	got, err := UnexpectedIncompatibilities(pipCheckReport(pipCheckOverridden), pipCheckLockFile(t))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v %v", got, err)
	}
}

func TestPipCheckLinesTheLockDoesNotExplainStayFailures(t *testing.T) {
	lock := pipCheckLockFile(t)
	for _, line := range []string{
		"The package `pinned-parent` requires `other-dep==1.0`, but `2.0` is installed",
		"The package `pinned-parent` requires `shared-dep==1.0`, but `3.0` is installed",
		"The package `pinned-parent` requires `shared-dep==1.0`, but it's not installed",
		"The package `pinned-parent` requires `unused>=1`, but `1.0` is installed",
	} {
		got, err := UnexpectedIncompatibilities(pipCheckReport(pipCheckOverridden, line), lock)
		if err != nil || !reflect.DeepEqual(got, []string{line}) {
			t.Errorf("%s: got %#v %v", line, got, err)
		}
	}
}

func TestPipCheckRequirementMarkersAndNameSpellingDoNotHideTheOverride(t *testing.T) {
	line := "The package `pinned-parent` requires `Shared.Dep==1.0 ; python_full_version >= '3.14'`, but `2.0` is installed"
	got, err := UnexpectedIncompatibilities(pipCheckReport(line), pipCheckLockFile(t))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v %v", got, err)
	}
}

func TestPipCheckWithoutALockEveryLineFails(t *testing.T) {
	for _, lock := range []string{"", filepath.Join(t.TempDir(), "missing.lock")} {
		got, err := UnexpectedIncompatibilities(pipCheckReport(pipCheckOverridden), lock)
		if err != nil || !reflect.DeepEqual(got, []string{pipCheckOverridden}) {
			t.Errorf("%q: got %#v %v", lock, got, err)
		}
	}
}

func TestPipCheckUnrecognisedReportIsNeverReadAsClean(t *testing.T) {
	lock := pipCheckLockFile(t)
	for _, report := range []string{
		"error: failed to inspect the environment\n",
		"Found 2 incompatibilities\n" + pipCheckOverridden + "\nThe package `x` has an unreadable requirement\n",
		pipCheckOverridden + "\n",
	} {
		got, err := UnexpectedIncompatibilities(report, lock)
		if err != nil || got != nil {
			t.Errorf("%q: got %#v %v", report, got, err)
		}
	}
}

func TestPipCheckOverridesRoundTripAsRequirementLines(t *testing.T) {
	got, err := OverrideRequirements(pipCheckLockFile(t))
	if err != nil || !reflect.DeepEqual(got, []string{"Shared_Dep==2.0", "unused ; sys_platform == 'never'"}) {
		t.Fatalf("got %#v %v", got, err)
	}
	unsupported := filepath.Join(t.TempDir(), "uv.lock")
	peWrite(t, unsupported, "[manifest]\noverrides = [{ name = \"x\", url = \"https://x\", specifier = \">1\" }]\n")
	if _, err := OverrideRequirements(unsupported); err == nil || err.Error() != "unsupported uv.lock override: {'name': 'x', 'url': 'https://x', 'specifier': '>1'}" {
		t.Fatalf("unsupported override: %v", err)
	}
}

// The upstream application lock reads the same through this parser as through tomllib in pip_check itself
func TestPipCheckReadsTheRealLockLikeTomllib(t *testing.T) {
	reference := peReferences(t, "uv.lock", "pm/")
	lock := filepath.Join(reference, "uv.lock")
	versions, err := OverriddenVersions(lock)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := OverrideRequirements(lock)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("the reference lock declares overrides; an empty read means the parser missed them")
	}
	script := "import json, sys; sys.path.insert(0, sys.argv[1]); from pathlib import Path\n" +
		"from pm.pip_check import overridden_versions, override_requirements\n" +
		"lock = Path(sys.argv[2])\n" +
		"print(json.dumps([{k: sorted(v) for k, v in overridden_versions(lock).items()}, override_requirements(lock)]))"
	cmd := exec.Command(peHostPython(t), "-I", "-B", "-c", script, reference, lock)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	var python [2]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &python); err != nil {
		t.Fatal(err)
	}
	var pyVersions map[string][]string
	var pyRequirements []string
	json.Unmarshal(python[0], &pyVersions)
	json.Unmarshal(python[1], &pyRequirements)
	goVersions := map[string][]string{}
	for k, set := range versions {
		goVersions[k] = sortedNames(set)
	}
	if !reflect.DeepEqual(goVersions, pyVersions) || !reflect.DeepEqual(requirements, pyRequirements) {
		t.Fatalf("go %v %v python %v %v", goVersions, requirements, pyVersions, pyRequirements)
	}
}

// A real project whose dependency pins shared deps at 1.0, overridden to 2.0, locked offline
func pipCheckOverriddenProject(t *testing.T, root string, overridden []string, extraWheels [][2]any, extraDependencies []string) (string, map[string]string) {
	t.Helper()
	uv, python := peUV(t), peHostPython(t)
	source, wheels := filepath.Join(root, "source"), filepath.Join(root, "wheels")
	var pins, overrides []string
	for _, name := range overridden {
		dashed := strings.ReplaceAll(name, "_", "-")
		pins = append(pins, dashed+"==1.0")
		overrides = append(overrides, dashed+"==2.0")
	}
	peWheel(t, wheels, "pinned_parent", "", pins, nil)
	for _, name := range overridden {
		peWheel(t, wheels, name, "1.0", nil, nil)
		peWheel(t, wheels, name, "2.0", nil, nil)
	}
	for _, w := range extraWheels {
		peWheel(t, wheels, w[0].(string), "", w[1].([]string), nil)
	}
	dependencies, _ := json.Marshal(append([]string{"pinned-parent==1.0"}, extraDependencies...))
	overridesJSON, _ := json.Marshal(overrides)
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[project]\nname=\"override-root\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies="+string(dependencies)+"\n[tool.uv]\npackage=false\nno-index=true\noverride-dependencies="+string(overridesJSON)+"\n"+
		"find-links=["+string(wheelsJSON)+"]\n")
	env := peIsolatedEnv(t, root)
	env["UV_CACHE_DIR"], env["UV_PYTHON"], env["UV_OFFLINE"] = filepath.Join(root, "cache"), python, "1"
	peRun(t, []string{uv, "lock", "--python", python}, source, env)
	return source, env
}

func TestPipCheckBuildAcceptsTheOverrideAndCheckStillRejectsWhatItDoesNotCover(t *testing.T) {
	for _, color := range []map[string]string{{}, {"FORCE_COLOR": "1", "CLICOLOR_FORCE": "1"}} {
		t.Run(fmt.Sprint(len(color)), func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			uv, python := peUV(t), peHostPython(t)
			peToolchain(t, uv, python)
			source, env := pipCheckOverriddenProject(t, root, []string{"shared_dep"}, nil, nil)
			for k, v := range color {
				env[k] = v
			}
			ctx := context.Background()
			out, cache := filepath.Join(root, "environment"), filepath.Join(root, "cache")
			executable, err := buildEnvironment(ctx, BuildEnvironmentOptions{Explicit: true, Source: source, Python: python, Out: out,
				Cache: cache, Env: env, Offline: true, Frozen: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := peRun(t, []string{executable, "-I", "-c", "import shared_dep; print(shared_dep.__version__)"}, root, env); got != "2.0" {
				t.Fatalf("installed %q", got)
			}
			environment, err := ManagedEnvironment(ctx, out, ManagedEnvironmentOptions{Python: python, Cache: cache, Env: env, Offline: true, Explicit: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := environment.Check(ctx, filepath.Join(source, "uv.lock")); err != nil {
				t.Fatal(err)
			}
			if err := environment.Check(ctx, ""); err == nil || !strings.Contains(err.Error(), "requires `shared-dep==1.0`, but `2.0` is installed") {
				t.Fatalf("check without the lock: %v", err)
			}
			stale := filepath.Join(root, "stale.lock")
			peWrite(t, stale, strings.Replace(peRead(t, filepath.Join(source, "uv.lock")), "name = \"shared-dep\"\nversion = \"2.0\"", "name = \"shared-dep\"\nversion = \"1.0\"", 1))
			versions, err := OverriddenVersions(stale)
			if err != nil || !reflect.DeepEqual(versions, map[string]map[string]bool{"shared-dep": {"1.0": true}}) {
				t.Fatalf("stale versions %v %v", versions, err)
			}
			var ie *InstallError
			if err := environment.Check(ctx, stale); !errors.As(err, &ie) || !strings.Contains(err.Error(), "dependency validation failed") {
				t.Fatalf("stale check: %v", err)
			}
		})
	}
}

func TestPipCheckRequirementsBuildsFromAnOverriddenLockApplyItsOverrides(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	uv, python := peUV(t), peHostPython(t)
	peToolchain(t, uv, python)
	source, env := pipCheckOverriddenProject(t, root, []string{"shared_dep"}, nil, nil)
	ctx := context.Background()
	wheels, cache := filepath.Join(root, "wheels"), filepath.Join(root, "cache")
	exported := filepath.Join(root, "requirements.txt")
	if _, err := exportRequirements(ctx, ExportRequirementsOptions{Source: source, Out: exported, Cache: cache, Env: env, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	requirements := pystr.Splitlines(peRead(t, exported), false)
	if !containsString(requirements, "shared-dep==2.0") {
		t.Fatalf("export %q", requirements)
	}
	build := func(out, lock string) (string, error) {
		return buildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: requirements, Out: filepath.Join(root, out),
			Python: python, Wheelhouse: wheels, Cache: cache, Env: env, Offline: true, Explicit: true, Lock: lock})
	}
	var conflict *ResolutionConflict
	if _, err := build("without-lock", ""); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "shared-dep") {
		t.Fatalf("without the lock: %v", err)
	}
	executable, err := build("with-lock", filepath.Join(source, "uv.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if got := peRun(t, []string{executable, "-I", "-c", "import shared_dep; print(shared_dep.__version__)"}, root, env); got != "2.0" {
		t.Fatalf("installed %q", got)
	}
	environment, err := ManagedEnvironment(ctx, filepath.Join(root, "wheelhouse-install"), ManagedEnvironmentOptions{Python: python, Cache: cache, Env: env, Offline: true, Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := environment.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := environment.InstallWheelhouse(ctx, source, wheels, 0); err != nil {
		t.Fatal(err)
	}
}

func TestPipCheckStreamedCheckStillSeesFailuresAheadOfALongExpectedReport(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	uv, python := peUV(t), peHostPython(t)
	peToolchain(t, uv, python)
	var overridden []string
	for i := 0; i < 30; i++ {
		overridden = append(overridden, fmt.Sprintf("shared_dep_%02d", i))
	}
	source, env := pipCheckOverriddenProject(t, root, overridden, [][2]any{{"aa_parent", []string{"aa-dep==1.0"}}, {"aa_dep", []string{}}}, []string{"aa-parent==1.0"})
	ctx := context.Background()
	out, cache := filepath.Join(root, "environment"), filepath.Join(root, "cache")
	if _, err := buildEnvironment(ctx, BuildEnvironmentOptions{Explicit: true, Source: source, Python: python, Out: out, Cache: cache,
		Env: env, Offline: true, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	infos, _ := filepath.Glob(filepath.Join(out, "lib", "python*", "site-packages", "aa_dep-1.0.dist-info"))
	if len(infos) != 1 {
		t.Fatalf("dist-info %v", infos)
	}
	os.RemoveAll(infos[0])
	environment, err := ManagedEnvironment(ctx, out, ManagedEnvironmentOptions{Python: python, Cache: cache, Env: env, Offline: true,
		Explicit: true, Output: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := environment.Check(ctx, filepath.Join(source, "uv.lock")); err == nil || !strings.Contains(err.Error(), "requires `aa-dep==1.0`, but it's not installed") {
		t.Fatalf("check: %v", err)
	}
}
