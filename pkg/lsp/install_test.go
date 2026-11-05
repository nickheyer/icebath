package lsp

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func swapRecipes(t *testing.T, recipes map[string]Recipe) {
	t.Helper()
	prev := InstallRecipes
	InstallRecipes = recipes
	installMeta.Lock()
	prevResults := installResults
	installResults = map[string]installResult{}
	installMeta.Unlock()
	t.Cleanup(func() {
		InstallRecipes = prev
		installMeta.Lock()
		installResults = prevResults
		installMeta.Unlock()
	})
}

func TestInstallPythonServerUsesPMToolEnvironment(t *testing.T) {
	rt, _, _ := testRuntime(t)
	binary := filepath.Join(t.TempDir(), "environment", "fake-language-server")
	type call struct {
		name, executable string
		requirements     []string
		timeout          time.Duration
	}
	var calls []call
	selected := ""
	rt.EnsurePythonTool = func(name string, requirements []string, executable string, timeout time.Duration) (string, error) {
		calls = append(calls, call{name, executable, requirements, timeout})
		selected = binary
		return binary, nil
	}
	rt.PythonTool = func(name, bin string) (string, bool, error) { return selected, selected != "", nil }
	swapRecipes(t, map[string]Recipe{"fake-lsp": {Strategy: "pip", Pkg: "fake-lsp==1.0", Bin: "fake-language-server"}})
	t.Setenv("PATH", "")
	got, err := TryInstall(rt, "fake-lsp", "auto")
	must(t, err)
	if got != binary {
		t.Fatalf("installed %q", got)
	}
	want := []call{{"lsp-fake-language-server", "fake-language-server", []string{"fake-lsp==1.0"}, 300 * time.Second}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
	if status, err := DetectStatus(rt, "fake-lsp"); err != nil || status != "installed" {
		t.Fatalf("status %q %v", status, err)
	}
}

func TestLSPPackageManagerConfigSelectsInstallerArgvAndNeverFallsBackSilently(t *testing.T) {
	rt, h, _ := testRuntime(t)
	staging := filepath.Join(rt.Home, "lsp")
	cfg := map[string]any{}
	rt.Config = func() map[string]any { return cfg }
	var runs [][]string
	prevRun, prevFind := runInstallCommand, findNodeExecutable
	t.Cleanup(func() { runInstallCommand, findNodeExecutable = prevRun, prevFind })
	runInstallCommand = func(argv []string, _ time.Duration, _ map[string]string) (*installRun, error) {
		runs = append(runs, argv)
		return &installRun{}, nil
	}
	present := map[string]string{"npm": "/usr/bin/npm", "pnpm": "/usr/bin/pnpm", "yarn": "/usr/bin/yarn"}
	findNodeExecutable = func(_ *Runtime, name string) (string, error) { return present[name], nil }

	cfg["package_manager"] = "pnpm"
	installNpm(rt, "pyright", "pyright-langserver", nil)
	if !reflect.DeepEqual(runs[len(runs)-1], []string{"/usr/bin/pnpm", "add", "--dir", staging, "pyright"}) {
		t.Fatalf("pnpm argv %v", runs[len(runs)-1])
	}
	cfg["package_manager"] = "yarn"
	installNpm(rt, "pyright", "pyright-langserver", nil)
	if !reflect.DeepEqual(runs[len(runs)-1], []string{"/usr/bin/yarn", "--cwd", staging, "add", "pyright"}) {
		t.Fatalf("yarn argv %v", runs[len(runs)-1])
	}
	cfg["package_manager"] = "pnmp"
	if got, err := installNpm(rt, "pyright", "pyright-langserver", nil); got != "" || err != nil {
		t.Fatalf("unknown manager must fail closed, got %q %v", got, err)
	}
	if len(h.matching("lsp.package_manager='pnmp' is not one of ['npm', 'pnpm', 'yarn']")) != 1 {
		t.Fatal("unknown manager warning")
	}
	delete(present, "yarn")
	cfg["package_manager"] = "yarn"
	if got, err := installNpm(rt, "pyright", "pyright-langserver", nil); got != "" || err != nil {
		t.Fatalf("absent manager must not install, got %q %v", got, err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs %v", runs)
	}
}

func TestNativeBinaryCandidatesPreferWrappersOnWindows(t *testing.T) {
	if got := nativeBinaryCandidates("/b/tool", true); !reflect.DeepEqual(got, []string{"/b/tool.cmd", "/b/tool.exe", "/b/tool.bat", "/b/tool"}) {
		t.Fatal(got)
	}
	if got := nativeBinaryCandidates("/b/tool", false); !reflect.DeepEqual(got, []string{"/b/tool"}) {
		t.Fatal(got)
	}
	if got := nativeBinaryCandidates("/b/T.CMD", true); len(got) != 4 {
		t.Fatal(got)
	}
}

func TestManualStrategyOnlyProbes(t *testing.T) {
	rt, _, _ := testRuntime(t)
	t.Setenv("PATH", "")
	called := false
	prevRun := runInstallCommand
	runInstallCommand = func([]string, time.Duration, map[string]string) (*installRun, error) {
		called = true
		return &installRun{}, nil
	}
	t.Cleanup(func() { runInstallCommand = prevRun })
	if got, err := TryInstall(rt, "pyright", "manual"); got != "" || err != nil || called {
		t.Fatalf("manual probed to %q %v, ran installer %v", got, err, called)
	}
	if status, _ := DetectStatus(rt, "clangd"); status != "manual-only" {
		t.Fatalf("clangd %q", status)
	}
	if status, _ := DetectStatus(rt, "pyright"); status != "missing" {
		t.Fatalf("pyright %q", status)
	}
}
