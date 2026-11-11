package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Fake core locked by real uv with the running tree on it and a home enabling no plugin
func vsAdmissionEnv(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, h)
	config.ResetCache()
	core := filepath.Join(tmp, "core")
	vsWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname = \"fake-core\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\ndependencies = []\n")
	uv, python := vsRealUV(t), vsHostPython(t)
	isolated := filepath.Join(tmp, "isolated-home")
	if err := os.MkdirAll(isolated, 0o777); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(uv, "lock", "--python", python)
	cmd.Dir = core
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "UV_") && !strings.HasPrefix(kv, "PYTHON") && !strings.HasPrefix(kv, "VIRTUAL_ENV=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+isolated, "USERPROFILE="+isolated, "ICEBATH_HOME="+filepath.Join(isolated, ".icebath"), "UV_CACHE_DIR="+filepath.Join(tmp, "uv-cache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v %s", err, out)
	}
	vsWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n")
	t.Cleanup(pm.PatchRepoRoot(core))
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(tmp, "tools"))
	previous := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapToolchain(previous) })
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	return tmp, h
}

// Reports a venv problem among the engine's install checks
func vsVenvProblem(t *testing.T) bool {
	t.Helper()
	problems, err := pm.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		if strings.HasPrefix(problem, "venv:") {
			return true
		}
	}
	return false
}

func TestCheckUsesRealPMSelectionAndKeepsInvalidEvidence(t *testing.T) {
	root, h := vsAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(root, "core")
	pinPath := filepath.Join(root, "pins.json")
	vsWrite(t, pinPath, `{"schema": 1, "packages": {}}`)
	pins := pm.NewLockfile(pinPath)
	pin := func(sha string) {
		pins.SetPin("python", "1.0", map[string]any{"any": map[string]any{"url": "https://example.invalid/python", "sha256": strings.Repeat(sha, 64)}})
		if err := pins.Save(); err != nil {
			t.Fatal(err)
		}
	}
	pin("a")
	previousLock := pm.SwapLockfilePath(func() string { return pinPath })
	t.Cleanup(func() { pm.SwapLockfilePath(previousLock) })
	oldStamp := filepath.Join(core, ".icebath-runtime", "cache", "venv-sync.json")
	vsWrite(t, oldStamp, `{"lockDigest": "old-bootstrap-stamp"}`)
	cached := vsRead(t, oldStamp)
	check := func(expected string, code int) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		result := VenvSyncMain(ctx, []string{"--project-root", core, "--check", "--json"}, &stdout, &stderr)
		var output map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("output %q %s", stdout.String(), stderr.String())
		}
		if result != code || output["state"] != expected {
			t.Fatalf("check %d %v, want %s %d", result, output, expected, code)
		}
		if vsRead(t, oldStamp) != cached {
			t.Fatal("a historical stamp changed")
		}
		return output
	}
	check("would-sync", 0)
	if err := pm.SyncVenv(ctx, pm.SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	factsPath := pm.RuntimeFactsPath(ctx)
	pristine := vsRead(t, factsPath)
	selected, err := pm.SelectedVenv(ctx, core)
	if err != nil || !strings.HasPrefix(selected, filepath.Join(pm.InstallStateDir(ctx, core), "environments")+string(filepath.Separator)) {
		t.Fatalf("selection %s %v", selected, err)
	}
	check("current", 0)
	if vsVenvProblem(t) {
		t.Fatal("a current venv reported a problem")
	}

	config := filepath.Join(h, "config.yaml")
	oldConfig := vsRead(t, config)
	member := filepath.Join(h, "plugins", "extra-member")
	vsWrite(t, filepath.Join(member, "plugin.yaml"), "name: extra-member\npython_dependencies: [\"example-dep==1\"]\n")
	vsWrite(t, config, "plugins:\n  enabled:\n  - extra-member\n")
	check("would-sync", 0)
	if !vsVenvProblem(t) {
		t.Fatal("a new member was not a venv problem")
	}
	vsWrite(t, config, oldConfig)
	check("current", 0)

	pyproject := filepath.Join(core, "pyproject.toml")
	vsAppend(t, pyproject, "[project.optional-dependencies]\nchanged-extra = []\n")
	var altered map[string]any
	json.Unmarshal([]byte(pristine), &altered)
	altered["packages"].(map[string]any)["venv"].(map[string]any)["extras"] = []string{"changed-extra"}
	encoded, _ := json.Marshal(altered)
	vsWrite(t, factsPath, string(encoded))
	check("would-sync", 0)
	if !vsVenvProblem(t) {
		t.Fatal("a recorded extra outside the tree was not a venv problem")
	}
	vsWrite(t, factsPath, pristine)

	pin("b")
	check("would-sync", 0)
	if !vsVenvProblem(t) {
		t.Fatal("a python pin change was not a venv problem")
	}
	pin("a")

	marker := filepath.Join(selected, "pyvenv.cfg")
	markerBytes := vsRead(t, marker)
	os.Remove(marker)
	check("would-sync", 0)
	if !vsVenvProblem(t) {
		t.Fatal("a missing pyvenv.cfg was not a venv problem")
	}
	vsWrite(t, marker, markerBytes)
	check("current", 0)

	outside := filepath.Join(root, "foreign-environment")
	vsWrite(t, filepath.Join(outside, "pyvenv.cfg"), markerBytes)
	json.Unmarshal([]byte(pristine), &altered)
	altered["packages"].(map[string]any)["venv"].(map[string]any)["environment"] = outside
	encoded, _ = json.Marshal(altered)
	vsWrite(t, factsPath, string(encoded))
	check("would-sync", 0)
	if !vsVenvProblem(t) {
		t.Fatal("a foreign environment was not a venv problem")
	}

	for _, invalid := range []string{"not JSON", `{"schema":1,"packages":{"venv":[]}}`} {
		vsWrite(t, factsPath, invalid)
		if output := check("failed", 1); output["detail"] == nil || output["detail"] == "" {
			t.Fatalf("failed check without detail %v", output)
		}
		if !vsVenvProblem(t) || vsRead(t, factsPath) != invalid || existsFollow(strings.TrimSuffix(factsPath, filepath.Ext(factsPath))+".corrupt") {
			t.Fatal("invalid facts were not kept as evidence")
		}
	}
	os.Remove(factsPath)
	check("would-sync", 0)
	if existsFollow(factsPath) {
		t.Fatal("a passive check wrote facts")
	}
}

func TestOwnTreeSyncReusesPMWithoutWritingAnExtraStamp(t *testing.T) {
	root, h := vsAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(root, "core")
	stamp := filepath.Join(core, ".icebath-runtime", "cache", "venv-sync.json")
	if existsFollow(stamp) {
		t.Fatal("fixture already has a stamp")
	}
	if result, err := SyncTree(ctx, core, false); err != nil || result != (VenvSyncResult{State: "synced", OK: true}) {
		t.Fatalf("sync %+v %v", result, err)
	}
	environment, err := pm.SelectedVenv(ctx, core)
	if err != nil {
		t.Fatal(err)
	}
	saved := vsRead(t, pm.RuntimeFactsPath(ctx))
	if existsFollow(stamp) {
		t.Fatal("the sync wrote an extra stamp")
	}
	if result, err := SyncTree(ctx, core, false); err != nil || result != (VenvSyncResult{State: "current", OK: true}) {
		t.Fatalf("second sync %+v %v", result, err)
	}
	if vsRead(t, pm.RuntimeFactsPath(ctx)) != saved {
		t.Fatal("a current sync rewrote the facts")
	}
	python := filepath.Join(environment, "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(environment, "Scripts", "python.exe")
	}
	probe := exec.Command(python, "-I", "-c", "import sys; print(sys.prefix)")
	probe.Dir = h
	out, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(environment)
	if prefix, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out))); prefix != resolved {
		t.Fatalf("prefix %s", out)
	}
	marker := filepath.Join(environment, "pyvenv.cfg")
	markerBytes := vsRead(t, marker)
	os.Remove(marker)
	if failure, err := SyncTree(ctx, core, false); err != nil || failure.State != "failed" || !strings.Contains(failure.Detail, "dependency environment is missing") {
		t.Fatalf("failure %+v %v", failure, err)
	}
	vsWrite(t, marker, markerBytes)
	if result, err := SyncTree(ctx, core, false); err != nil || result != (VenvSyncResult{State: "current", OK: true}) {
		t.Fatalf("restored %+v %v", result, err)
	}
	if existsFollow(stamp) {
		t.Fatal("the sync wrote an extra stamp")
	}
}
