package commands

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune child, then installs the secret source doubles the child env scrubber reads
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry(context.Background())
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	os.Exit(runSandboxed(m))
}

// Runs the cases under a session home so discovery a case reaches never touches the real home
func runSandboxed(m *testing.M) int {
	dir, err := os.MkdirTemp("", "commands-home-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("ICEBATH_HOME", dir)
	return m.Run()
}
