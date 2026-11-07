package media

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the gateway and platform registry secret source doubles the child env scrubber reads
func TestMain(m *testing.M) {
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	os.Exit(m.Run())
}
