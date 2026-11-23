package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Dispatches the scratch prune child, blocks external network, installs the process hooks and runs the suite under a private home
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry(context.Background())
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	installAgentWiring()
	dir, err := os.MkdirTemp("", "app-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv(home.EnvHome, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Installs the lower package hooks cmd/icebath assigns at startup so an in process agent turn runs under test
func installAgentWiring() {
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
	tools.ProfileSecrets = provider.ProfileSecretSource()
	tools.RuntimeProviderCredentials = provider.ToolRuntimeCredentials
	tools.VisionCapabilities = provider.VisionCapabilities{}
	tools.CachedContextLength = func(ctx context.Context, modelID, baseURL string) (int, bool) {
		return provider.GetCachedContextLength(ctx, modelID, baseURL, false)
	}
	tools.ModelContextLength = provider.ToolModelContextLength
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	tools.AsyncDelegationOwnerProbes.PidExists = func(int) bool { return false }
	tools.AsyncDelegationOwnerProbes.ProcessStartTime = func(int) (int64, bool) { return 0, false }
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{
		CatalogEntry:    func(string) (string, bool) { return "", false },
		Install:         func(context.Context, string, bool) (map[string]any, error) { return nil, nil },
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
	})
	agent.InstallDelegationRegistryHooks()
	agent.SessionSteerAuthority = func(context.Context, string) (any, any) { return nil, nil }
}
