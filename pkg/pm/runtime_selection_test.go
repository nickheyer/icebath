package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Writes the install's runtime facts selecting environment
func svFacts(t *testing.T, state string, venv map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"schema": 1, "packages": map[string]any{"venv": venv}})
	writeFile(t, filepath.Join(state, "facts.json"), data, 0o666)
}

func TestInstallRuntimeSelectionIsScopedAndReadOnly(t *testing.T) {
	tmp := isolateHome(t)
	homeDir := filepath.Join(tmp, "home")
	t.Setenv(home.EnvHome, homeDir)
	ctx := context.Background()
	first, second := filepath.Join(tmp, "first"), filepath.Join(tmp, "second")
	sttMkdir(t, filepath.Join(first, ".venv"))
	sttMkdir(t, filepath.Join(second, ".venv"))
	if got, err := SelectedVenv(ctx, first); err != nil || got != filepath.Join(resolvePath(first), ".venv") {
		t.Fatalf("in tree selection %q %v", got, err)
	}
	if _, err := os.Stat(homeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading the selection created the home")
	}
	state := InstallStateDir(ctx, first)
	if state == InstallStateDir(ctx, second) {
		t.Fatal("two installs share one state dir")
	}
	generation := sttMkdir(t, filepath.Join(state, "environments", "candidate", "venv"))
	writeFile(t, filepath.Join(generation, "pyvenv.cfg"), []byte("home = test\n"), 0o666)
	svFacts(t, state, map[string]any{"environment": generation, "stamp": "verified"})
	if got, _ := SelectedVenv(ctx, first); got != resolvePath(generation) {
		t.Fatalf("recorded selection %q", got)
	}
	if got, _ := SelectedVenv(ctx, second); got != filepath.Join(resolvePath(second), ".venv") {
		t.Fatalf("other install %q", got)
	}
	t.Setenv(home.EnvHome, filepath.Join(homeDir, "profiles", "work"))
	if got, _ := SelectedVenv(ctx, first); got != resolvePath(generation) {
		t.Fatalf("profile home selection %q", got)
	}
}

// Venv with pyvenv.cfg and its site packages, returning the site
func svSite(t *testing.T, venv string) string {
	t.Helper()
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("home = test"), 0o666)
	site, err := SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	return sttMkdir(t, site)
}

func TestBootUsesOneSelectedDependencyTreeInFreshProcess(t *testing.T) {
	tmp := isolateHome(t)
	svKeepProcess(t)
	ctx := context.Background()
	root := filepath.Join(tmp, "repo")
	state := InstallStateDir(ctx, root)
	base, selected := filepath.Join(root, "venv"), filepath.Join(state, "environments", "new", "venv")
	baseSite := svSite(t, base)
	writeFile(t, filepath.Join(baseSite, "probe_package.py"), []byte("version = 'old'"), 0o666)
	writeFile(t, filepath.Join(baseSite, "base_only.py"), []byte("version = 'must-not-leak'"), 0o666)
	writeFile(t, filepath.Join(svSite(t, selected), "probe_package.py"), []byte("version = 'new'"), 0o666)
	svFacts(t, state, map[string]any{"environment": selected})
	importPath.Lock()
	importPath.entries = append([]string{baseSite}, importPath.entries...)
	importPath.Unlock()
	t.Setenv("PYTHONPATH", baseSite)
	if err := ActivateDependencies(ctx, root); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(importPathEntries(), baseSite) {
		t.Fatalf("base site stayed on the import path %v", importPathEntries())
	}
	code := "import probe_package, importlib.util; print(probe_package.version); print(importlib.util.find_spec('base_only') is None)"
	out, err := exec.Command(peHostPython(t), "-c", code).CombinedOutput()
	if err != nil || strings.Fields(string(out))[0] != "new" || strings.Fields(string(out))[1] != "True" {
		t.Fatalf("child saw %q %v", out, err)
	}
}

func TestBootPutsTheCheckoutLauncherAheadOfTheVenvsOwnConsoleScript(t *testing.T) {
	tmp := isolateHome(t)
	svKeepProcess(t)
	ctx := context.Background()
	root := filepath.Join(tmp, "repo")
	state := InstallStateDir(ctx, root)
	selected := filepath.Join(state, "environments", "new", "venv")
	svSite(t, selected)
	venvBin := sttMkdir(t, VenvBinDir(selected, runtime.GOOS == "windows"))
	checkoutBin := sttMkdir(t, filepath.Join(root, ".icebath", "bin"))
	svFacts(t, state, map[string]any{"environment": selected})
	if err := ActivateDependencies(ctx, root); err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	launcher, script := slices.Index(entries, resolvePath(checkoutBin)), slices.Index(entries, resolvePath(venvBin))
	if launcher < 0 || script < 0 || launcher > script {
		t.Fatalf("PATH %v", entries)
	}
}

func TestBootNeverActivatesThePrePMVenv(t *testing.T) {
	for _, interpreter := range []string{"store", "venv"} {
		for _, withState := range []bool{true, false} {
			t.Run(interpreter+map[bool]string{true: "+state", false: ""}[withState], func(t *testing.T) {
				tmp := isolateHome(t)
				svKeepProcess(t)
				ctx := context.Background()
				storeDir := sttMkdir(t, filepath.Join(tmp, "runtime"))
				t.Setenv(EnvRuntimeDir, storeDir)
				prefix := sttMkdir(t, filepath.Join(storeDir, "python"))
				if interpreter == "venv" {
					prefix = sttMkdir(t, filepath.Join(tmp, "dev", "bin"))
				}
				peSwap(t, &processPrefix, func() (string, error) { return prefix, nil })
				root := filepath.Join(tmp, "repo")
				legacy := svSite(t, filepath.Join(root, "venv"))
				writeFile(t, filepath.Join(legacy, "legacy_only.py"), nil, 0o666)
				if withState {
					sttMkdir(t, InstallStateDir(ctx, root))
				}
				err := ActivateDependencies(ctx, root)
				if interpreter == "store" {
					var py *PyError
					if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.HasPrefix(err.Error(), "no dependency environment is committed") {
						t.Fatalf("store interpreter gave %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command(peHostPython(t), "-c", "import importlib.util; print(importlib.util.find_spec('legacy_only') is not None)").CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "False" || slices.Contains(importPathEntries(), legacy) {
					t.Fatalf("legacy importable %q %v, import path %v", out, err, importPathEntries())
				}
			})
		}
	}
}

func TestMalformedSelectionHasActionableError(t *testing.T) {
	for _, data := range []string{`[]`, `{"packages": []}`, `{"packages": {"venv": []}}`} {
		t.Run(data, func(t *testing.T) {
			tmp := isolateHome(t)
			ctx := context.Background()
			repo := filepath.Join(tmp, "repo")
			record := EnvironmentRuntimeFactsPath(ctx, repo)
			sttMkdir(t, filepath.Dir(record))
			writeFile(t, record, []byte(data), 0o666)
			_, err := SelectedVenv(ctx, repo)
			var py *PyError
			if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.Contains(err.Error(), "dependency environment") {
				t.Fatalf("malformed %s gave %v", data, err)
			}
		})
	}
}

func TestInvalidSelectedEnvironmentNeverSilentlyFallsBack(t *testing.T) {
	for _, bad := range []string{"outside", "missing"} {
		t.Run(bad, func(t *testing.T) {
			tmp := isolateHome(t)
			ctx := context.Background()
			root := filepath.Join(tmp, "repo")
			sttMkdir(t, filepath.Join(root, "venv"))
			state := sttMkdir(t, InstallStateDir(ctx, root))
			candidate := filepath.Join(state, "environments", "missing")
			if bad == "outside" {
				candidate = sttMkdir(t, filepath.Join(tmp, "outside"))
				writeFile(t, filepath.Join(candidate, "pyvenv.cfg"), []byte("home = test\n"), 0o666)
			}
			svFacts(t, state, map[string]any{"environment": candidate})
			_, err := SelectedVenv(ctx, root)
			var py *PyError
			if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.Contains(err.Error(), "environment") {
				t.Fatalf("%s selection gave %v", bad, err)
			}
		})
	}
}
