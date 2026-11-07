package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Dispatches the child modes, installs the secret env doubles, then runs the suite
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	dispatchTrajectoryChild()
	dispatchLifecycleChild()
	state.SecretEnvSubstrings = testSecretEnvSubstrings
	state.SecretEnvName = func(name string) bool {
		upper := strings.ToUpper(name)
		for _, s := range testSecretEnvSubstrings {
			if strings.Contains(upper, s) {
				return true
			}
		}
		return false
	}
	provider.GatewayHooks.LegacyJSON = func(string) map[string]any { return nil }
	provider.GatewayHooks.BridgedAllowAllUsers = func() (string, bool) { return "", false }
	provider.GatewayHooks.MergePlatformSections = func(map[string]any, any, map[string]any, []string) map[string]any { return map[string]any{} }
	provider.GatewayHooks.PlatformEnabled = func(map[string]any) (bool, string) { return false, "" }
	provider.GatewayHooks.ServesHome = func(string) bool { return false }
	tools.ProfileSecrets = providerProfileSecrets{}
	realAgentSetupLogging = agentSetupLogging
	agentSetupLogging = func(context.Context, string) error { return nil }
	os.Exit(m.Run())
}

// Real logging setup, kept so a test that drives the agent file handlers can opt back in
var realAgentSetupLogging func(context.Context, string) error

// Provider secret scope bookkeeping the cmd wiring installs as the tools profile secret source
type providerProfileSecrets struct{}

func (providerProfileSecrets) ManagedDotenvKeys() []string   { return provider.ManagedDotenvKeys() }
func (providerProfileSecrets) SourceSuppliedNames() []string { return provider.SourceSuppliedNames() }
func (providerProfileSecrets) BuildProfileSecretScope(h string) map[string]string {
	return provider.BuildProfileSecretScope(h)
}
func (providerProfileSecrets) CurrentSecretScope(ctx context.Context) map[string]string {
	return provider.CurrentSecretScope(ctx)
}
