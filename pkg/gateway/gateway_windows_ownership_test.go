package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestIcebathOwnsWindowsServiceByNameOrBinaryRoot(t *testing.T) {
	roots := []string{`C:\Users\n\AppData\Local\icebath\icebath-agent`, "", `C:\Tools\Bin\`}
	for _, c := range []struct {
		name, binpath string
		want          bool
	}{
		{"Icebath_Gateway", "", true},
		{"ICE-BATH gateway (work)", "", true},
		{"Schedule", `C:\Windows\system32\svchost.exe -k netsvcs -p -s Schedule`, false},
		{"MyGateway", `"C:\users\N\appdata\local\ICEBATH\icebath-agent\venv\Scripts\gateway.exe" run`, true},
		{"MyGateway", `C:/tools/bin/nssm.exe`, true},
		{"MyGateway", `C:\tools\binary\nssm.exe`, false},
		{"MyGateway", `C:\Tools\Bin`, false},
		{"", "", false},
	} {
		if got := IcebathOwnsWindowsService(c.name, c.binpath, roots); got != c.want {
			t.Errorf("owns(%q, %q) = %v want %v", c.name, c.binpath, got, c.want)
		}
	}
	if got := normalizeWindowsPath(`  "C:\A\B\\ `); got != "c:/a/b" {
		t.Errorf("normalized %q", got)
	}
}

func TestIcebathServiceRootsNameTheInstallTreeTheBinaryDirAndTheLauncherDir(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	tree := t.TempDir()
	t.Cleanup(pm.PatchRepoRoot(tree))
	saved := windowsServiceExecutable
	t.Cleanup(func() { windowsServiceExecutable = saved })
	windowsServiceExecutable = func() (string, error) { return filepath.Join(tree, "bin", "icebath.exe"), nil }
	roots := IcebathServiceRoots(context.Background())
	if len(roots) != 3 || roots[0] != tree || roots[1] != filepath.Join(tree, "bin") || roots[2] != filepath.Join(h, "gateway-service") {
		t.Fatalf("roots %v", roots)
	}
}
