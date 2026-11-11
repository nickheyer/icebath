package selfupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Calls the cua refresh fakes saw, the installed lookups, the ensures and the host setups
type cuaRefreshCalls struct {
	lookups    []bool
	ensures    []string
	hostSetups []bool
}

// Isolated home with updates.refresh_cua_driver set, an installed lookup answering installed, and recording fakes
func cuaRefreshFixture(t *testing.T, refresh, installed bool) *cuaRefreshCalls {
	t.Helper()
	h := t.TempDir()
	value := "true"
	if !refresh {
		value = "false"
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("updates:\n  refresh_cua_driver: "+value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_CUA_DRIVER_CMD", "")
	t.Setenv("PATH", "")
	calls := &cuaRefreshCalls{}
	oldInstalled, oldEnsure, oldSetup := cuaRefreshInstalled, pmEnsureFn, installCuaDriver
	t.Cleanup(func() { cuaRefreshInstalled, pmEnsureFn, installCuaDriver = oldInstalled, oldEnsure, oldSetup })
	cuaRefreshInstalled = func(_ context.Context, name string, allowOutdated bool) (*pm.InstalledPackage, error) {
		if name != "cua-driver" {
			t.Fatalf("lookup %q", name)
		}
		calls.lookups = append(calls.lookups, allowOutdated)
		if installed {
			return &pm.InstalledPackage{}, nil
		}
		return nil, nil
	}
	pmEnsureFn = func(_ context.Context, name string) error {
		calls.ensures = append(calls.ensures, name)
		return nil
	}
	installCuaDriver = func(_ context.Context, _ io.Writer, show bool) (bool, error) {
		calls.hostSetups = append(calls.hostSetups, show)
		return true, nil
	}
	return calls
}

func TestCuaRefreshSkipsDisabledMissingOrExternalDriver(t *testing.T) {
	for _, c := range []struct {
		disabled, installed bool
		override            string
	}{{true, true, ""}, {false, false, ""}, {false, true, "custom-cua"}} {
		calls := cuaRefreshFixture(t, !c.disabled, c.installed)
		t.Setenv("ICEBATH_CUA_DRIVER_CMD", c.override)
		if err := refreshCuaDriverAfterUpdate(context.Background(), io.Discard); err != nil {
			t.Fatal(err)
		}
		if len(calls.ensures) != 0 || len(calls.hostSetups) != 0 {
			t.Fatalf("%+v: ensures %v host setups %v", c, calls.ensures, calls.hostSetups)
		}
	}
}
