package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/builtin/delegationdefault"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Dispatches the child modes, installs the secret env doubles, then runs the suite in a sandbox home
func TestMain(m *testing.M) {
	relayhost.DispatchRelayHostEntry(context.Background())
	relaytest.Provision()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	dispatchLoopbackOwnerChild()
	home.DispatchScratchPruneEntry(context.Background())
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
	dispatchPluginResumeChild()
	DispatchManagedWorkerEntry(os.Exit)
	// Sandbox home for the whole run so a case that sets none never resolves the real default home
	dir, err := os.MkdirTemp("", "agent-home-")
	if err != nil {
		panic(err)
	}
	sandboxHome = dir
	os.Setenv("ICEBATH_HOME", dir)
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, joinTitleUpgrades, joinBackgroundJobs)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Home every case shares until it sets its own, which no case writes settings into
var sandboxHome string

// Joins the title upgrades a case left running so none resolves the home of a later case
func joinTitleUpgrades(t *testing.T) func() {
	return func() {
		waitForTitleUpgrades(30 * time.Second)
		titleUpgradeMu.Lock()
		defer titleUpgradeMu.Unlock()
		for wp := range titleUpgradeThreads {
			if thread := wp.Value(); thread != nil && thread.Alive() {
				t.Errorf("title upgrade %q was still running 30s after the test", thread.Name)
			}
		}
	}
}

// Joins the compression workers a case abandoned on timeout and the delegations it left dispatched, so none resolves the home of a later case
func joinBackgroundJobs(t *testing.T) func() {
	return func() {
		running := func() (int, int) {
			compressAdmissionLock.Lock()
			defer compressAdmissionLock.Unlock()
			return compressAdmittedCount, tools.AsyncDelegationActiveCount()
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			compressions, delegations := running()
			if compressions == 0 && delegations == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d compression worker(s) and %d delegation(s) were still running 30s after the test", compressions, delegations)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
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
