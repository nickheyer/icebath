package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const wsReplayBackend = `
from pathlib import Path
from zipfile import ZipFile

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    assert Path("README.md").read_text() == "# fixture readme"
    assert Path("LICENSE").read_text() == "MIT"
    name = "replay_plugin-1.0-py3-none-any.whl"
    dist = "replay_plugin-1.0.dist-info"
    entries = {
        "replay_plugin/__init__.py": Path("replay_plugin/__init__.py").read_bytes(),
        "replay_plugin/values.py": Path("replay_plugin/values.py").read_bytes(),
        dist + "/METADATA": "Metadata-Version: 2.1\nName: replay-plugin\nVersion: 1.0\n",
        dist + "/WHEEL": "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
    entries[dist + "/RECORD"] = "".join(path + ",,\n" for path in entries)
    with ZipFile(Path(wheel_directory) / name, "w") as wheel:
        for path, body in entries.items():
            wheel.writestr(path, body)
    return name

build_editable = build_wheel
`

// Writes a buildable plugin source whose in tree backend reads its readme and license
func wsBuildableSource(t *testing.T, plugin string) {
	t.Helper()
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname=\"replay-plugin\"\nversion=\"1.0\"\nrequires-python=\">=3.11\"\n"+
		"readme=\"README.md\"\nlicense=\"MIT\"\nlicense-files=[\"LICENSE\"]\n[build-system]\nrequires=[]\nbuild-backend=\"backend\"\nbackend-path=[\"build\"]\n")
	peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: replay-plugin\n")
	peWrite(t, filepath.Join(plugin, "replay_plugin", "__init__.py"), "from .values import VALUE\n")
	peWrite(t, filepath.Join(plugin, "replay_plugin", "values.py"), "VALUE = 'recorded plugin bytes'\n")
	peWrite(t, filepath.Join(plugin, "README.md"), "# fixture readme")
	peWrite(t, filepath.Join(plugin, "LICENSE"), "MIT")
	peWrite(t, filepath.Join(plugin, "build", "backend.py"), wsReplayBackend)
}

// Offline isolated uv env with the real toolchain and the repo root pointed at core
func wsCoreEnv(t *testing.T, tmp, core string) map[string]string {
	t.Helper()
	env := peIsolatedEnv(t, tmp)
	config := filepath.Join(tmp, "config")
	os.MkdirAll(config, 0o777)
	env["XDG_CONFIG_HOME"], env["XDG_CONFIG_DIRS"] = config, config
	env["UV_CACHE_DIR"], env["UV_OFFLINE"] = filepath.Join(tmp, "cache"), "1"
	peToolchain(t, peUV(t), peHostPython(t))
	peSwap(t, &repoRoot, func() string { return core })
	wsHost(t, "")
	return env
}

// Stdout of a script run by a venv interpreter in isolated mode
func wsProbe(t *testing.T, venv, cwd, script string) string {
	t.Helper()
	return peRun(t, []string{VenvPython(venv), "-I", "-c", script}, cwd, map[string]string{"PATH": os.Getenv("PATH")})
}

func TestWorkspaceInputsRealBuildInputsStayInGeneratedRoot(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	wsBuildableSource(t, core)
	if err := os.Rename(filepath.Join(core, "build", "backend.py"), filepath.Join(core, "backend.py")); err != nil {
		t.Fatal(err)
	}
	wsEdit(t, filepath.Join(core, "pyproject.toml"), func(s string) string {
		return strings.ReplaceAll(s, "backend-path=[\"build\"]", "backend-path=[\".\"]")
	})
	peWrite(t, filepath.Join(core, ".env"), "must not copy")
	env := wsCoreEnv(t, tmp, core)
	root, venv := filepath.Join(tmp, "staging"), filepath.Join(tmp, "venv")
	if _, err := wsSync(t, env, wsMembers(), []string{}, root, core, venv); err != nil {
		t.Fatal(err)
	}
	if got := wsProbe(t, venv, tmp, "import replay_plugin; print(replay_plugin.VALUE)"); got != "recorded plugin bytes" {
		t.Fatalf("probe %q", got)
	}
	if _, err := os.Lstat(filepath.Join(root, ".env")); err == nil {
		t.Fatal(".env copied into the generated root")
	}
	if eggs, _ := filepath.Glob(filepath.Join(core, "*.egg-info")); len(eggs) != 0 {
		t.Fatalf("build wrote into the original core %v", eggs)
	}
	if _, err := os.Lstat(filepath.Join(core, "uv.lock")); err == nil {
		t.Fatal("core gained a lock")
	}
}

func TestWorkspaceInputsRepairReplaysSavedInTreeBuildBackend(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	core, plugin := filepath.Join(tmp, "core"), filepath.Join(tmp, "plugin")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"replay-core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.uv]\npackage=false\nno-index=true\n")
	wsBuildableSource(t, plugin)
	inputs := wsSnapshot(t, plugin)
	env := wsCoreEnv(t, tmp, core)
	uv, python := peUV(t), peHostPython(t)
	saved, repaired := filepath.Join(tmp, "saved"), filepath.Join(tmp, "repaired")
	initial := &PythonEnvironment{UV: uv, Python: python, Destination: filepath.Join(tmp, "initial-env"), Cache: filepath.Join(tmp, "initial-cache"), Env: env, Offline: true}
	if err := LockAndSync(ctx, wsMembers(plugin), []string{}, LockAndSyncOptions{Root: saved, Source: core, Environment: initial}); err != nil {
		t.Fatal(err)
	}
	members := wsStrings(wsAt(wsTOML(t, filepath.Join(saved, "pyproject.toml")), "tool", "uv", "workspace", "members"))
	if len(members) != 1 {
		t.Fatalf("members %v", members)
	}
	relative := filepath.FromSlash(members[0])
	carries := func(root string) {
		t.Helper()
		for path, data := range inputs {
			if got := peRead(t, filepath.Join(root, relative, path)); got != data {
				t.Fatalf("%s of %s differs", path, root)
			}
		}
	}
	carries(saved)
	savedLock := peRead(t, filepath.Join(saved, "uv.lock"))

	peWrite(t, filepath.Join(core, "pyproject.toml"), "damaged [")
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "damaged [")
	peWrite(t, filepath.Join(plugin, "plugin.yaml"), "damaged [")
	if err := os.Remove(filepath.Join(plugin, "build", "backend.py")); err != nil {
		t.Fatal(err)
	}
	peWrite(t, filepath.Join(plugin, "replay_plugin", "values.py"), "raise RuntimeError('damaged live source')\n")
	repair := &PythonEnvironment{UV: uv, Python: python, Destination: filepath.Join(tmp, "repair-env"), Cache: filepath.Join(tmp, "repair-cache"), Env: env, Offline: true}
	if err := LockAndSync(ctx, wsMembers(plugin), []string{}, LockAndSyncOptions{Root: repaired, Source: core, Replay: saved, Environment: repair}); err != nil {
		t.Fatal(err)
	}
	if peRead(t, filepath.Join(repaired, "uv.lock")) != savedLock || peRead(t, filepath.Join(saved, "uv.lock")) != savedLock {
		t.Fatal("replay changed the recorded lock")
	}
	carries(saved)
	carries(repaired)
	for _, e := range []*PythonEnvironment{initial, repair} {
		if got := wsProbe(t, e.Destination, tmp, "import replay_plugin; print(replay_plugin.VALUE)"); got != "recorded plugin bytes" {
			t.Fatalf("probe of %s %q", e.Destination, got)
		}
	}
	for _, path := range []string{filepath.Join(core, "pyproject.toml"), filepath.Join(plugin, "pyproject.toml"), filepath.Join(plugin, "plugin.yaml")} {
		if peRead(t, path) != "damaged [" {
			t.Fatalf("%s rewritten", path)
		}
	}
	if _, err := os.Lstat(filepath.Join(plugin, "build", "backend.py")); err == nil {
		t.Fatal("live backend restored")
	}
}

func TestWorkspaceInputsSourceRefreshDoesNotNeedMetadataChangeAndRefusesLiveRoot(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"x\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n[tool.uv]\npackage=false\nno-index=true\n")
	peWrite(t, filepath.Join(core, "code.py"), "VALUE = 1\n")
	env := wsCoreEnv(t, tmp, core)
	ctx := context.Background()
	staged := filepath.Join(tmp, "staged")
	if _, err := wsSync(t, env, wsMembers(), []string{}, staged, core, filepath.Join(tmp, "env")); err != nil {
		t.Fatal(err)
	}
	peWrite(t, filepath.Join(core, "code.py"), "VALUE = 2\n")
	fresh := filepath.Join(tmp, "fresh")
	if err := LockAndSync(ctx, wsMembers(), []string{}, LockAndSyncOptions{Root: fresh, Source: core, SeedLock: filepath.Join(staged, "uv.lock"),
		Environment: wsEngine(t, filepath.Join(tmp, "fresh-env"), env)}); err != nil {
		t.Fatal(err)
	}
	if peRead(t, filepath.Join(staged, "code.py")) != "VALUE = 1\n" || peRead(t, filepath.Join(fresh, "code.py")) != "VALUE = 2\n" {
		t.Fatal("generations do not carry their own source bytes")
	}
	before := peRead(t, filepath.Join(core, "code.py"))
	err := LockAndSync(ctx, wsMembers(), []string{}, LockAndSyncOptions{Root: core, Source: core, Environment: wsEngine(t, filepath.Join(tmp, "env"), env)})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("got %v", err)
	}
	if peRead(t, filepath.Join(core, "code.py")) != before {
		t.Fatal("live root mutated")
	}
}

func TestWorkspaceInputsLegacyMemberIsGeneratedOnlyInsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	core, plugin, wheels := filepath.Join(tmp, "core"), filepath.Join(tmp, "readonly-plugin"), filepath.Join(tmp, "wheels")
	peWheel(t, wheels, "example", "1.0", nil, nil)
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n"+
		"[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
	manifest := filepath.Join(plugin, "plugin.yaml")
	peWrite(t, manifest, "name: legacy\npython_dependencies: [\"example>=1,<2\"]\n")
	env := wsCoreEnv(t, tmp, core)
	stamp, err := MembersStamp(wsMembers(plugin))
	if err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(tmp, "stage")
	if _, err := wsSync(t, env, wsMembers(plugin), []string{}, generated, core, filepath.Join(tmp, "env")); err != nil {
		t.Fatal(err)
	}
	members := wsStrings(wsAt(wsTOML(t, filepath.Join(generated, "pyproject.toml")), "tool", "uv", "workspace", "members"))
	member := resolvePath(filepath.Join(generated, filepath.FromSlash(members[0])))
	if !pathIsRelativeTo(member, resolvePath(generated)) {
		t.Fatalf("member %s outside %s", member, generated)
	}
	if _, err := os.Lstat(filepath.Join(plugin, "pyproject.toml")); err == nil {
		t.Fatal("legacy plugin gained a pyproject")
	}
	if got := wsStrings(wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "dependencies")); !reflect.DeepEqual(got, []string{"example>=1,<2"}) {
		t.Fatalf("dependencies %v", got)
	}
	peWrite(t, manifest, "name: legacy\npython_dependencies: [\"example>=2,<3\"]\n")
	if changed, err := MembersStamp(wsMembers(plugin)); err != nil || changed == stamp {
		t.Fatalf("stamp %s %v", changed, err)
	}
}

func TestWorkspaceInputsPluginCanMoveCompatibleTransitiveButNotExactRequirement(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "compatible", true: "exact"}[exact], func(t *testing.T) {
			tmp := t.TempDir()
			ctx := context.Background()
			core, wheels := filepath.Join(tmp, "core"), filepath.Join(tmp, "wheels")
			peWheel(t, wheels, "pkga", "1.0", []string{"pkgb>=1.2,<2"}, nil)
			peWheel(t, wheels, "pkgb", "1.2", nil, nil)
			requirement := "[\"pkga==1.0\"]"
			if exact {
				requirement = "[\"pkga==1.0\", \"pkgb==1.2\"]"
			}
			wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
			peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core-proof\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
				"dependencies="+requirement+"\n[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
			env := wsCoreEnv(t, tmp, core)
			baseline := filepath.Join(tmp, "baseline")
			if _, err := wsSync(t, env, wsMembers(), []string{}, baseline, core, filepath.Join(tmp, "first-env")); err != nil {
				t.Fatal(err)
			}
			firstLock := peRead(t, filepath.Join(baseline, "uv.lock"))
			if got := wsLocked(t, filepath.Join(baseline, "uv.lock"))["pkgb"]; got != "1.2" {
				t.Fatalf("baseline pkgb %s", got)
			}
			peWheel(t, wheels, "pkgb", "1.3", nil, nil)
			plugin := filepath.Join(tmp, "plugin")
			peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname=\"plugin-proof\"\nversion=\"1\"\nrequires-python=\">=3.11\"\ndependencies=[\"pkgb==1.3\"]\n")
			extended, candidate := filepath.Join(tmp, "extended"), filepath.Join(tmp, "candidate")
			err := LockAndSync(ctx, wsMembers(plugin), []string{}, LockAndSyncOptions{Root: extended, Source: core, SeedLock: filepath.Join(baseline, "uv.lock"),
				Environment: wsEngine(t, candidate, env)})
			if exact {
				var conflict *ResolutionConflict
				if !errors.As(err, &conflict) {
					t.Fatalf("got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := wsLocked(t, filepath.Join(extended, "uv.lock"))["pkgb"]; got != "1.3" {
					t.Fatalf("extended pkgb %s", got)
				}
				if got := wsProbe(t, candidate, tmp, "import pkga, pkgb; print(pkga.__version__, pkgb.__version__)"); got != "1.0 1.3" {
					t.Fatalf("probe %q", got)
				}
			}
			if peRead(t, filepath.Join(baseline, "uv.lock")) != firstLock {
				t.Fatal("baseline lock changed")
			}
		})
	}
}

func TestWorkspaceInputsMemberUVLockTravelsWithItsMember(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.setuptools.packages.find]\ninclude=[\"pm\"]\n")
	peWrite(t, filepath.Join(core, "uv.lock"), "version = 1\n# root lock\n")
	peWrite(t, filepath.Join(core, "pm", "pyproject.toml"), "[project]\nname=\"pm\"\nversion=\"1\"\n")
	peWrite(t, filepath.Join(core, "pm", "uv.lock"), "version = 1\n# member lock\n")
	destination := filepath.Join(tmp, "stage")
	if err := copyCoreInputs(core, destination); err != nil {
		t.Fatal(err)
	}
	if got := peRead(t, filepath.Join(destination, "pm", "uv.lock")); got != "version = 1\n# member lock\n" {
		t.Fatalf("member lock %q", got)
	}
	if _, err := os.Lstat(filepath.Join(destination, "uv.lock")); err == nil {
		t.Fatal("the root lock was copied")
	}
}

func TestWorkspaceInputsNestedDistTravelsButRootDistStaysOut(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.setuptools.packages.find]\ninclude=[\"*\"]\n")
	peWrite(t, filepath.Join(core, "plugins", "kanban", "dashboard", "dist", "index.js"), "ENTRY\n")
	peWrite(t, filepath.Join(core, "dist", "core-1.whl"), "OUTPUT\n")
	destination := filepath.Join(tmp, "stage")
	if err := copyCoreInputs(core, destination); err != nil {
		t.Fatal(err)
	}
	if got := peRead(t, filepath.Join(destination, "plugins", "kanban", "dashboard", "dist", "index.js")); got != "ENTRY\n" {
		t.Fatalf("bundle %q", got)
	}
	if _, err := os.Lstat(filepath.Join(destination, "dist")); err == nil {
		t.Fatal("root build output entered the snapshot")
	}
}

func TestWorkspaceInputsToolStoreInsideAPackageStaysOutOfTheSnapshot(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	tools := filepath.Join(core, "tools")
	entries := []string{"python-3.14.7-linux-x64", "chromium-1208", "fetch-abc123"}
	for _, entry := range entries {
		peWrite(t, filepath.Join(tools, entry, "bin", "payload"), "BINARY\n")
	}
	peWrite(t, filepath.Join(tools, "__init__.py"), "")
	peWrite(t, filepath.Join(tools, "environments", "__init__.py"), "")
	peWrite(t, filepath.Join(tools, "wakewords", "hey.onnx"), "DATA\n")
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{
		"python": map[string]any{"entry": "python-3.14.7-linux-x64"}, "chromium": map[string]any{"entry": "chromium-1208"},
		"venv": map[string]any{"entry": nil}, "escape": map[string]any{"entry": "../environments"}}})
	peWrite(t, filepath.Join(tools, "facts.json"), string(facts))
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.setuptools.packages.find]\ninclude=[\"tools\", \"tools.*\"]\n")
	destination := filepath.Join(tmp, "stage")
	if err := copyCoreInputs(core, destination); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(filepath.Join(destination, "tools", "environments", "__init__.py")) {
		t.Fatal("tools/environments package missing")
	}
	if got := peRead(t, filepath.Join(destination, "tools", "wakewords", "hey.onnx")); got != "DATA\n" {
		t.Fatalf("wakewords %q", got)
	}
	for _, entry := range entries {
		if _, err := os.Lstat(filepath.Join(destination, "tools", entry)); err == nil {
			t.Fatalf("store entry %s copied into the snapshot", entry)
		}
	}
}

func TestWorkspaceInputsAFactsFileWithoutPublishedEntriesHidesNothing(t *testing.T) {
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	peWrite(t, filepath.Join(core, "agent", "chromium-1208", "kept.txt"), "KEPT\n")
	peWrite(t, filepath.Join(core, "agent", "facts.json"), "{\"schema\": 1}")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.setuptools.packages.find]\ninclude=[\"agent\"]\n")
	destination := filepath.Join(tmp, "stage")
	if err := copyCoreInputs(core, destination); err != nil {
		t.Fatal(err)
	}
	if got := peRead(t, filepath.Join(destination, "agent", "chromium-1208", "kept.txt")); got != "KEPT\n" {
		t.Fatalf("kept %q", got)
	}
}
