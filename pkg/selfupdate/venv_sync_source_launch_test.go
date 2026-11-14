package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// User cache directory read before any test moves HOME
var vsCacheRoot, _ = os.UserCacheDir()

// Real uv from PATH or the pinned copy the pm suite caches for this host
func vsRealUV(t *testing.T) string {
	t.Helper()
	if found, err := exec.LookPath("uv"); err == nil {
		return found
	}
	matches, _ := filepath.Glob(filepath.Join(vsCacheRoot, "icebath-pm-tests", "uv-*"))
	for _, root := range matches {
		found := ""
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == "uv" && found == "" {
				found = path
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	t.Fatal("source launch integration requires real uv")
	return ""
}

// Host interpreter the worker builds environments with
func vsHostPython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("source launch integration requires a host Python")
	}
	return python
}

// Replaces tool acquisition with the uv and Python the parent passed down
func vsChildToolchain() {
	uv, python := os.Getenv("VS_UV"), os.Getenv("VS_PYTHON")
	pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	if lock := os.Getenv("VS_TOOL_LOCK"); lock != "" {
		pm.SwapLockfilePath(func() string { return lock })
	}
}

// Links this test binary as the install binary of root, copying where a link would pin the running image
func vsInstallBinary(t *testing.T, root string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "icebath")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	} else if err := os.Link(exe, binary); err == nil {
		return binary
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

// Source install over real PM where only tool acquisition is replaced, returning the checkout and its install binary
func vsSourceLaunch(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	uv, python := vsRealUV(t), vsHostPython(t)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(tmp, "store"))
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
	lock := filepath.Join(tmp, "tool-lock.json")
	vsWrite(t, lock, `{"schema": 1, "packages": {}}`)
	t.Setenv("VS_UV", uv)
	t.Setenv("VS_PYTHON", python)
	t.Setenv("VS_TOOL_LOCK", lock)
	previousLock := pm.SwapLockfilePath(func() string { return lock })
	previousTool := pm.SwapToolchain(func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	t.Cleanup(func() { pm.SwapLockfilePath(previousLock); pm.SwapToolchain(previousTool) })
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	root := filepath.Join(tmp, "source with spaces")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
	t.Setenv("VS_COMPLETION_CALLS", filepath.Join(tmp, "completion-calls"))
	t.Setenv("VS_COMPLETION_EXIT", "0")
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"launch-proof\"\nversion = \"1\"\nrequires-python = \">=3.11\"\n"+
		"[project.optional-dependencies]\nall = []\nlaunch-extra = []\n[tool.uv]\npackage = false\nno-index = true\noffline = true\n")
	if err := pm.LockProject(context.Background(), pm.LockProjectOptions{Source: root, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	vsRecordPython(t, "test", "python-test")
	return tmp, root, vsInstallBinary(t, root)
}

// Recorded venv fact of root, checked against the selected generation it names
func vsFact(t *testing.T, root string) map[string]any {
	t.Helper()
	ctx := context.Background()
	facts, err := pm.NewFacts(pm.EnvironmentRuntimeFactsPath(ctx, root), true)
	if err != nil {
		t.Fatal(err)
	}
	fact, ok := facts.Get("venv")
	if !ok {
		t.Fatal("no venv fact")
	}
	selected, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if pyrepr.Str(fact["environment"]) != selected {
		t.Fatalf("fact environment %v selected %s", fact["environment"], selected)
	}
	if rel, err := filepath.Rel(filepath.Join(pm.InstallStateDir(ctx, root), "environments"), selected); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("selection %s outside the install state", selected)
	}
	if !isRegular(filepath.Join(selected, "pyvenv.cfg")) || !isRegular(pyrepr.Str(fact["resolved_lock"])) {
		t.Fatalf("incomplete generation %s", selected)
	}
	out, err := exec.Command(filepath.Join(selected, "bin", "python"), "-I", "-c", "import json,sys; print(json.dumps(sys.prefix))").Output()
	if err != nil {
		t.Fatal(err)
	}
	var prefix string
	if err := json.Unmarshal(out, &prefix); err != nil || prefix != selected {
		t.Fatalf("generation prefix %q", out)
	}
	return fact
}

// Receipt files the PM wrote under the test home
func vsReceipts(tmp string) map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(home.LogsDir(filepath.Join(tmp, "home")), "update_receipts", "pm_*.json"))
	for _, m := range matches {
		out[m] = true
	}
	return out
}

func vsNewReceipts(before, after map[string]bool) []string {
	var out []string
	for path := range after {
		if !before[path] {
			out = append(out, path)
		}
	}
	return out
}

func vsReceiptOutcome(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Loads(string(data))
	obj, ok := doc.(*pyjson.Object)
	if err != nil || !ok {
		t.Fatalf("receipt %s", data)
	}
	outcome, _ := obj.Get("outcome")
	return outcome
}

func vsRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func vsGenerations(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(pm.InstallStateDir(context.Background(), root), "environments"))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

func vsCurrent(t *testing.T, root string) bool {
	t.Helper()
	current, err := pm.VenvIsCurrent(context.Background(), pm.VenvIsCurrentOptions{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func vsSync(t *testing.T, root string, extras []string) {
	t.Helper()
	if err := pm.SyncVenv(context.Background(), pm.SyncVenvOptions{Extras: extras, Explicit: true, ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}
}

func vsPrepare(t *testing.T, root string) string {
	t.Helper()
	target, err := PrepareLaunch(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func vsSelectedSite(t *testing.T, root string) string {
	t.Helper()
	selected, err := pm.SelectedVenv(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(selected)
	if err != nil {
		t.Fatal(err)
	}
	return site
}

func vsAppend(t *testing.T, path, text string) {
	t.Helper()
	vsWrite(t, path, vsRead(t, path)+text)
}

func vsStrings(v any) []string {
	var out []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			out = append(out, pyrepr.Str(item))
		}
	case []string:
		out = list
	}
	return out
}

// Records a store Python entry linked to the host interpreter as the python fact
func vsRecordPython(t *testing.T, version, entryName string) string {
	t.Helper()
	ctx := context.Background()
	store, err := pm.StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(store, entryName)
	python := filepath.Join(entry, "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
		t.Fatal(err)
	}
	host, _ := filepath.EvalSymlinks(vsHostPython(t))
	if err := os.Symlink(host, python); err != nil {
		t.Fatal(err)
	}
	factsPath, err := pm.FactsPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := pm.NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := pm.CurrentTarget()
	digest, err := pm.TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record("python", version, entryName, pm.EnvDiff{"PATH": []string{filepath.Dir(python)}}, store, pm.FactRecord{Target: &target, Digest: &digest}); err != nil {
		t.Fatal(err)
	}
	return python
}

// Runs the install command the launcher persisted, reporting the binary that ran and the selected probe value
func vsRunInstallCommand(t *testing.T, command []string) map[string]any {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), pm.EnvDisableLazyInstalls+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install command %v: %v", command, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("install command output %q", out)
	}
	return doc
}

// Probe launch of root through the boot path, run as a child of this test
func vsRunProbe(t *testing.T, root, probe string, argv []string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], argv...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VS_BOOT_PROBE="+probe, "VS_BOOT_ROOT="+root)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stdout.String(), stderr.String()
}

// Relaunches into the install binary once, then reports what the relaunched program saw
func vsRelaunchProbe(args []string, probe string) int {
	if !Relaunched() {
		binary := pm.ResolveInstallBinary(os.Getenv("VS_BOOT_ROOT"))
		code, err := relaunchExec(binary, RelaunchCommand(binary, args))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return code
	}
	report := []any{pm.RepoRoot(), args[1:]}
	if probe == "relaunch-options" {
		exe, _ := os.Executable()
		report = []any{args[1:], runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1), exe, Relaunched()}
	}
	encoded, _ := json.Marshal(report)
	fmt.Println(string(encoded))
	return 0
}

// The boot path of a probe launch, then what the launched program saw
func vsBootProbe(ctx context.Context, args []string, probe string) int {
	if strings.HasPrefix(probe, "relaunch-") {
		return vsRelaunchProbe(args, probe)
	}
	root := os.Getenv("VS_BOOT_ROOT")
	pm.PatchRepoRoot(root)
	vsChildToolchain()
	if probe == "activate" {
		if err := pm.ActivateDependencies(ctx, root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		vsProcessSites()
		return 0
	}
	if code, stop := BootLaunch(ctx, root, args); stop {
		return code
	}
	vsProcessSites()
	report := map[string]any{"executable": args[0], "args": args[1:]}
	if probe == "full" {
		selected, _ := pm.SelectedVenv(ctx, root)
		site, _ := pm.SitePackages(selected)
		binary, err := PrepareLaunch(ctx, root, args[1:])
		report["site"], report["path"], report["launch_current"] = site, pm.ImportPathEntries(), err == nil && binary == ""
	}
	encoded, _ := json.Marshal(report)
	fmt.Println(string(encoded))
	return 0
}

// Hands each activated dependency site to a Python child the way a launched interpreter processes it
func vsProcessSites() {
	for _, entry := range pm.ImportPathEntries() {
		if filepath.Base(entry) == "site-packages" {
			exec.Command(os.Getenv("VS_PYTHON"), "-I", "-c", "import site, sys; site.addsitedir(sys.argv[1])", entry).Run()
		}
	}
}
