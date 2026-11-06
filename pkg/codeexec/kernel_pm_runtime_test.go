package codeexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runtime generation selected by the install facts, its site packages on PYTHONPATH beside a user lib
func runtimeDependency(t *testing.T, mode string) (*tools.Env, interpreter) {
	t.Helper()
	env := kernelEnv(t, map[string]any{"mode": mode, "timeout": 15})
	rt := hostRuntime(t)
	out, err := exec.Command(rt.Python, "-c", "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := pm.RepoRoot()
	generation := filepath.Join(pm.InstallStateDir(context.Background(), root), "environments", "fixture", "venv")
	if err := os.MkdirAll(generation, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "version = " + strings.TrimSpace(string(out)) + ".0\n"
	if err := os.WriteFile(filepath.Join(generation, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	packages, err := pm.SitePackages(generation)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(packages, "pm_fixture_dependency.py"), "VALUE = 'runtime-dependency'\n")
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": generation}}})
	writeFile(t, pm.EnvironmentRuntimeFactsPath(context.Background(), root), string(facts))
	userLib := filepath.Join(t.TempDir(), "user-lib")
	writeFile(t, filepath.Join(userLib, "user_fixture_dependency.py"), "VALUE = 'user-dependency'\n")
	t.Setenv("PYTHONPATH", strings.Join([]string{root, packages, userLib}, string(os.PathListSeparator)))
	t.Setenv("TERMINAL_ENV", "local")
	unsetEnv(t, "VIRTUAL_ENV")
	unsetEnv(t, "CONDA_PREFIX")
	return env, rt
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Removes a variable for the test, restoring its prior value afterwards
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDependenciesImportInPersistentKernel(t *testing.T) {
	for _, mode := range []string{"strict", "project"} {
		t.Run(mode, func(t *testing.T) {
			env, _ := runtimeDependency(t, mode)
			first := decodeResult(t, executeCode(t.Context(), env,
				"import pm_fixture_dependency as dep\nimport user_fixture_dependency as user\nprint(dep.VALUE, user.VALUE)",
				"pm-runtime-import", nil, false))
			wantStatus(t, first, "success")
			if !strings.Contains(outputOf(first), "runtime-dependency user-dependency") {
				t.Fatalf("first cell %v", first)
			}
			second := decodeResult(t, executeCode(t.Context(), env, "print(dep.VALUE)", "pm-runtime-import", nil, false))
			wantStatus(t, second, "success")
			if kernelField(second, "reused") != true || !strings.Contains(outputOf(second), "runtime-dependency") {
				t.Fatalf("second cell %v", second)
			}
		})
	}
}

func TestProjectInterpreterKeepsRuntimeDependenciesOut(t *testing.T) {
	env, rt := runtimeDependency(t, "project")
	project := filepath.Join(t.TempDir(), "project-env")
	if out, err := exec.Command(rt.Python, "-m", "venv", "--without-pip", project).CombinedOutput(); err != nil {
		t.Fatalf("venv: %v %s", err, out)
	}
	t.Setenv("VIRTUAL_ENV", project)
	if got, err := resolveChildPython(t.Context(), env, "project"); err != nil || got != pm.VenvPython(project) {
		t.Fatalf("child python %q err %v, want %q", got, err, pm.VenvPython(project))
	}
	result := decodeResult(t, executeCode(t.Context(), env,
		"import importlib.util\nimport user_fixture_dependency as user\n"+
			"print(importlib.util.find_spec('pm_fixture_dependency') is None, user.VALUE)",
		"pm-project-isolation", nil, false))
	wantStatus(t, result, "success")
	if !strings.Contains(outputOf(result), "True user-dependency") {
		t.Fatalf("project result %v", result)
	}
}
