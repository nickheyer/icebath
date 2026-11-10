package browser

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child modes the platform specific test files register before TestMain dispatches
var extraChildModes = map[string]func() int{}

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry(context.Background())
	if mode := os.Getenv(childModeEnv); mode != "" {
		os.Unsetenv(childModeEnv)
		if err := exitWithRoot(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if run, ok := extraChildModes[mode]; ok {
			os.Exit(run())
		}
		os.Exit(runChildMode(mode))
	}
	removeRoot := openRootPipe()
	claimPrivateDir = func(string) bool { return false }
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
	if tools.RegisteredAdapterSecretEnv == nil {
		tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	}
	code := m.Run()
	removeRoot()
	os.Exit(code)
}

// Swaps a package seam for the duration of one test
func pin[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

func realClaimPrivateDir(t *testing.T) func(string) bool {
	t.Helper()
	pin(t, &claimPrivateDir, realClaim)
	return realClaim
}

func testEnv(t *testing.T) *tools.Env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	return &tools.Env{Home: dir}
}
