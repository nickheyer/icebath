//go:build !windows

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestVenvPythonSymlinkedToTheStoreBinaryStillRelaunches(t *testing.T) {
	tmp, store, run := vsCurrentLaunch(t)
	venvPython := filepath.Join(tmp, "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(venvPython), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, venvPython); err != nil {
		t.Fatal(err)
	}
	target, published := run(store, venvPython)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(tmp, "checkout"))
	if target != store || !reflect.DeepEqual(published, []string{resolved}) {
		t.Fatalf("symlinked interpreter %q %v", target, published)
	}
}

func TestForeignOwnedVenvFileRefusedBeforeSync(t *testing.T) {
	tmp := t.TempDir()
	checkout := filepath.Join(tmp, "checkout")
	installer := filepath.Join(checkout, "venv", "lib", "python3.14", "site-packages", "pkg.dist-info", "INSTALLER")
	vsWrite(t, installer, "pip\n")
	vsSwap(t, &venvSelectedVenv, func(context.Context, string) (string, error) { return filepath.Join(checkout, "venv"), nil })
	foreign := os.Geteuid() + 1
	vsSwap(t, &lstatOwner, func(path string) (int, error) {
		if path == installer {
			return foreign, nil
		}
		return defaultLstatOwner(path)
	})
	err := RefuseForeignOwnedVenv(context.Background(), checkout)
	if err == nil || pm.ErrorClass(err) != "RuntimeError" || !strings.Contains(err.Error(), "INSTALLER is owned by uid "+strconv.Itoa(foreign)) {
		t.Fatalf("foreign owned file %v", err)
	}
}
