package specchild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/pm/specprobe"
)

// Runs the find_spec child mode when the test binary is re-executed as one
func TestMain(m *testing.M) {
	DispatchEntry()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Writes text to path, creating its parent directories
func spWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A venv over the host interpreter laid out as uv venv does, returning its interpreter and site-packages
func spVenv(t *testing.T, root string) (string, string) {
	t.Helper()
	var host string
	for _, name := range []string{"python3", "python"} {
		if found, err := exec.LookPath(name); err == nil {
			host = found
			break
		}
	}
	if host == "" {
		t.Fatal("find_spec cases need a host Python")
	}
	real, err := filepath.EvalSymlinks(host)
	if err != nil {
		t.Fatal(err)
	}
	in, err := pyhost.Discover(real)
	if err != nil {
		t.Fatal(err)
	}
	venv := filepath.Join(root, "venv")
	spWrite(t, filepath.Join(venv, "pyvenv.cfg"), fmt.Sprintf("home = %s\nversion = %d.%d.0\n", filepath.Dir(real), in.Major, in.Minor))
	python := filepath.Join(venv, "bin", "python")
	if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, python); err != nil {
		t.Fatal(err)
	}
	return python, filepath.Join(venv, "lib", fmt.Sprintf("python%d.%d%s", in.Major, in.Minor, in.ABIFlags), "site-packages")
}

func TestFindSpecsAnswersLikeHermesImportable(t *testing.T) {
	root := t.TempDir()
	python, site := spVenv(t, root)
	spWrite(t, filepath.Join(site, "probe_dep.py"), "VALUE = 1\n")
	spWrite(t, filepath.Join(site, "probe_pkg", "__init__.py"), "")
	spWrite(t, filepath.Join(site, "probe_pkg", "sub", "__init__.py"), "")
	extra := filepath.Join(root, "pth-added")
	spWrite(t, filepath.Join(extra, "pth_dep.py"), "VALUE = 2\n")
	spWrite(t, filepath.Join(site, "extra.pth"), extra+"\n")
	spWrite(t, filepath.Join(site, "hook.pth"), "import sys, types; sys.modules['only_in_sys_modules'] = types.ModuleType('only_in_sys_modules')\n")
	work := filepath.Join(root, "work")
	spWrite(t, filepath.Join(work, "cwd_only.py"), "VALUE = 3\n")
	modules := []string{"probe_dep", "probe_pkg.sub", "pth_dep", "only_in_sys_modules", "json", "absent_module", "absent_pkg.sub", "", "probe_dep.sub", "cwd_only"}
	found, err := specprobe.FindSpecs(context.Background(), python, work, os.Environ(), modules)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, true, true, true, true, false, false, false, false, false}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("found %v want %v for %q", found, want, modules)
	}
}

func TestFindSpecsRaisesWhatFindSpecRaisesPastImportAndValueErrors(t *testing.T) {
	root := t.TempDir()
	python, site := spVenv(t, root)
	spWrite(t, filepath.Join(site, "boom_pkg", "__init__.py"), "raise RuntimeError('boom')\n")
	_, err := specprobe.FindSpecs(context.Background(), python, root, os.Environ(), []string{"absent_module", "boom_pkg.sub"})
	var raised *specprobe.FindSpecError
	if !errors.As(err, &raised) || raised.Module != "boom_pkg.sub" || raised.Message != "RuntimeError: boom" {
		t.Fatalf("error %v", err)
	}
}

func TestChildRefusesAnInterpreterItCannotHost(t *testing.T) {
	root := t.TempDir()
	_, err := specprobe.FindSpecs(context.Background(), filepath.Join(root, "missing", "python"), root, os.Environ(), []string{"json"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), specprobe.Entry+": ") {
		t.Fatalf("error %v", err)
	}
}
