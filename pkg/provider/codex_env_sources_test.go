package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the gateway owned env blocklist sources the subprocess env scrubber requires
func installCodexEnvSources(t *testing.T) {
	t.Helper()
	prevC, prevR := tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
	if tools.RegisteredAdapterSecretEnv == nil {
		tools.RegisteredAdapterSecretEnv = func(env *tools.Env) map[string]bool { return map[string]bool{} }
	}
	t.Cleanup(func() { tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv = prevC, prevR })
}
