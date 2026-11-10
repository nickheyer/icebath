package commands

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune child, then installs the secret source doubles the child env scrubber reads
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	os.Exit(m.Run())
}
