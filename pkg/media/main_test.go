package media

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune child, then installs the secret source doubles the child env scrubber reads
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	dispatchWakeLockHolder()
	home.DispatchScratchPruneEntry(context.Background())
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	if provider.GatewayHooks.BridgedAllowAllUsers == nil {
		provider.GatewayHooks.BridgedAllowAllUsers = func() (string, bool) { return "", false }
	}
	os.Exit(m.Run())
}
