package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/builtin/delegationdefault"
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
	dispatchVerifyRunChild()
	dispatchProviderClientSleepChild()
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
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
		tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	}
	if tools.AsyncDelegationOwnerProbes.PidExists == nil {
		tools.AsyncDelegationOwnerProbes.PidExists = func(int) bool { return false }
		tools.AsyncDelegationOwnerProbes.ProcessStartTime = func(int) (int64, bool) { return 0, false }
	}
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{
		CatalogEntry:    func(string) (string, bool) { return "", false },
		Install:         func(context.Context, string, bool) (map[string]any, error) { return nil, nil },
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
		AuxiliaryTasks:  func() []map[string]any { return nil },
	})
	if tools.DelegationRegistryHooks.GetProvider == nil {
		installDefaultDelegationRegistry()
	}
	if SessionSteerAuthority == nil {
		SessionSteerAuthority = func(context.Context, string) (any, any) { return nil, nil }
	}
	realAgentSetupLogging = agentSetupLogging
	agentSetupLogging = func(context.Context, string) error { return nil }
	dispatchDelegationRecoveryChild()
	dispatchAsyncOwnerDeathChild()
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

// Delegation registry double serving only the bundled default backend, as a fresh install does
func installDefaultDelegationRegistry() {
	def := delegationdefault.NewDefaultDelegationProvider()
	tools.DelegationRegistryHooks.GetProvider = func(_ context.Context, name string) tools.DelegationProvider {
		if name == def.Name() {
			return def
		}
		return nil
	}
	tools.DelegationRegistryHooks.DisabledPlugins = func(string) map[string]bool { return map[string]bool{} }
	tools.DelegationRegistryHooks.LoadBundled = func(context.Context, string, func(tools.DelegationProvider)) bool { return false }
	tools.DelegationRegistryHooks.DiscoverPlugins = func(context.Context, bool) {}
	tools.DelegationRegistryHooks.ListProviders = func(context.Context) []tools.DelegationProvider { return []tools.DelegationProvider{def} }
}
