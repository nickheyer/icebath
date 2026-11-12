package browser

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/browser/harnesshost"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child modes the platform specific test files register before TestMain dispatches
var extraChildModes = map[string]func() int{}

func TestMain(m *testing.M) {
	harnesshost.DispatchEntry()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	removeRoot := startChildGuard()
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

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
