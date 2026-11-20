//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// A venv whose interpreter must never run, holding a module and a pth added directory
func peTrapVenv(t *testing.T, root string) (string, string) {
	t.Helper()
	venv := filepath.Join(root, "venv")
	version := peRun(t, []string{peHostPython(t), "-I", "-c", "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')"}, root, pyEnviron())
	peWrite(t, filepath.Join(venv, "pyvenv.cfg"), "home = /nonexistent\nversion = "+version+".0\n")
	writeFile(t, VenvPython(venv), []byte("#!/bin/sh\nexit 99\n"), 0o755)
	site := filepath.Join(venv, "lib", "python"+version, "site-packages")
	peWrite(t, filepath.Join(site, "venv_only_dep.py"), "VALUE = 'from the venv'\n")
	peWrite(t, filepath.Join(site, "venv_only_cli.py"), "import json, sys, venv_only_dep\nprint(json.dumps([venv_only_dep.VALUE, sys.argv]))\n")
	extra := filepath.Join(root, "pth-added")
	peWrite(t, filepath.Join(extra, "pth_only_dep.py"), "VALUE = 'from a .pth'\n")
	peWrite(t, filepath.Join(site, "extra.pth"), extra+"\n")
	return venv, site
}

// Runs a command expecting success and decodes its JSON output
func peRunJSON(t *testing.T, argv []string, cwd string) any {
	t.Helper()
	env := pyEnviron()
	delete(env, "PYTHONPATH")
	delete(env, "PYTHONHOME")
	var out any
	if err := json.Unmarshal([]byte(peRun(t, argv, cwd, env)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEntryKeepsIsolationAndPMOffTheImportPath(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("platforms linux, macos")
	}
	tmp := isolateHome(t)
	venv, _ := peTrapVenv(t, tmp)
	peWrite(t, filepath.Join(tmp, "venv_only_dep.py"), "VALUE = 'cwd shadow'\n")
	payload := filepath.Join(tmp, "payload")
	repo := svSealedPayload(t, payload, svPayloadStore(t, payload, filepath.Join("tools", "python")))
	prefix, err := VenvCommand(context.Background(), repo, venv, []string{"-I"})
	if err != nil {
		t.Fatal(err)
	}
	state := InstallStateDir(context.Background(), RepoRoot())
	code := "import json, os, sys, venv_only_dep; print(json.dumps([venv_only_dep.VALUE, [p for p in sys.path if p in ('', os.getcwd()) or p.startswith(" +
		pyrepr.StrRepr(state) + ")]]))"
	if got := peRunJSON(t, append(append([]string{}, prefix...), "-c", code), tmp); !reflect.DeepEqual(got, []any{"from the venv", []any{}}) {
		t.Fatalf("got %v", got)
	}
}
