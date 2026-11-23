package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Installs the lower package hooks cmd/icebath assigns at startup so a real agent builds and runs under test
func init() {
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
	gateway.ErrorSurfaceFromResult = agent.BuildErrorSurfaceFromResult
}
