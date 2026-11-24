package gateway

import (
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the gateway functions lower packages call through their hook variables
func init() {
	provider.GatewayHooks.ServesHome = providerServesHome
	tools.RegisteredAdapterSecretEnv = registeredAdapterSecretEnv
	tools.RegisteredPlatformNames = registeredPlatformNames
	tools.BundledPlatformNames = bundledPlatformNamesAndAliases
	tools.SupervisedGatewayPid = supervisedGatewayPid
}

// Whether this launch is a supervised gateway and the pid of the live gateway, the lock records left in place
func supervisedGatewayPid() (bool, int, error) {
	if !IsSupervisedGatewayLaunch(nil) {
		return false, 0, nil
	}
	pid, _, err := GetRunningPid("", false)
	return true, pid, err
}

// Whether a live gateway serves home by the uncached ladder scoped to it, false on any error
func providerServesHome(h string) bool {
	return ResolveGatewayLiveness(LivenessOptions{ProfileDir: h, NoCache: true}).Running
}

// Secret named required env of the adapters registered for the env's home plus that home's user plugin secrets
func registeredAdapterSecretEnv(env *tools.Env) map[string]bool {
	h := home.Process()
	if env != nil && env.Home != "" {
		h = env.Home
	}
	out := map[string]bool{}
	for n := range Registry.requiredEnvNamesIn(home.Key(h)) {
		upper := strings.ToUpper(n)
		for _, suffix := range tools.PlatformSecretEnvSuffixes {
			if strings.HasSuffix(upper, suffix) {
				out[upper] = true
				break
			}
		}
	}
	homeNames, err := tools.HomeAdapterSecretEnv(h)
	if err != nil {
		panic(err)
	}
	for n := range homeNames {
		out[n] = true
	}
	return out
}

// Registered platform names of the process home, never loading an adapter
func registeredPlatformNames() []string {
	names := Registry.registeredNamesIn(home.Key(home.Process()))
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
