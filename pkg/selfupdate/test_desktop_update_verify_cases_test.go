package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Root as the optional caller argument the verify takes
func dvRoot(root string) *string { return &root }

func TestDefaultRootIsTheImportedCheckoutNotCwd(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	prev := verifyPackagedExecutable
	t.Cleanup(func() { verifyPackagedExecutable = prev })
	var seen []string
	verifyPackagedExecutable = func(desktop string) (string, bool) {
		seen = append(seen, desktop)
		return "", false
	}
	err := verifyWindowsDesktopUpdate(context.Background(), nil)
	var py *pm.PyError
	if !errors.As(err, &py) || py.Class != "RuntimeError" || !strings.Contains(py.Message, "executable is missing") {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 1 || seen[0] != filepath.Join(checkoutRoot(), "apps", "desktop") {
		t.Fatalf("looked under %v", seen)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if info, err := os.Stat(filepath.Join(checkoutRoot(), filepath.Base(exe))); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("checkout root %s does not hold the running binary", checkoutRoot())
	}
	if checkoutRoot() == tmp {
		t.Fatal("checkout root is the cwd")
	}
}

// Packaged app lookup that finds an app under root and nowhere else
func dvAppOnlyUnder(root string) func(string) (string, bool) {
	desktop, _ := filepath.Abs(filepath.Join(root, "apps", "desktop"))
	return func(candidate string) (string, bool) {
		if abs, _ := filepath.Abs(candidate); abs == desktop {
			return filepath.Join(desktop, "release", "fixture", "Icebath.exe"), true
		}
		return "", false
	}
}
