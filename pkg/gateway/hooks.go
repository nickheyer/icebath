package gateway

import (
	"context"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the gateway functions lower packages call through their hook variables
func init() {
	provider.GatewayHooks.LegacyJSON = func(h string) map[string]any {
		m, _ := LoadLegacyGatewayJSON(h).(map[string]any)
		return m
	}
	provider.GatewayHooks.BridgedAllowAllUsers = BridgedAllowAllUsers
	provider.GatewayHooks.MergePlatformSections = providerMergePlatformSections
	provider.GatewayHooks.PlatformEnabled = providerPlatformEnabled
	provider.GatewayHooks.ServesHome = providerServesHome
	tools.RegisteredAdapterSecretEnv = registeredAdapterSecretEnv
	tools.RegisteredPlatformNames = registeredPlatformNames
}

// Whether a live gateway serves home by the uncached ladder scoped to it, false on any error
func providerServesHome(h string) bool {
	return ResolveGatewayLiveness(LivenessOptions{ProfileDir: h, NoCache: true}).Running
}

// Merged platform blocks for provider, an exception of the merge raised as a panic
func providerMergePlatformSections(yamlCfg map[string]any, gatewayBlock any, gwData map[string]any, also []string) map[string]any {
	set := map[string]bool{}
	for _, n := range also {
		set[n] = true
	}
	out, err := MergePlatformSections(context.Background(), yamlCfg, gatewayBlock, gwData, set)
	if err != nil {
		panic(err)
	}
	return out
}

// Enabled flag and token text of a block, an exception of the parse raised as a panic
func providerPlatformEnabled(block map[string]any) (bool, string) {
	pc, err := PlatformConfigFromDict(context.Background(), block)
	if err != nil {
		panic(err)
	}
	token := pc.Token
	if !pyrepr.Truthy(token) {
		token = ""
	}
	return pc.Enabled, pyrepr.Str(token)
}

// Secret named required env of the adapters registered for the env's home plus that home's user plugin secrets
func registeredAdapterSecretEnv(env *tools.Env) map[string]bool {
	h := home.Process()
	if env != nil && env.Home != "" {
		h = env.Home
	}
	out := map[string]bool{}
	for n := range Registry.requiredEnvNamesIn(home.Key(h)) {
		upper := pystr.Upper(n)
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
