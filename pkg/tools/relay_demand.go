package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reason of the host a turn gets while the runtime is provisioned and nothing consumes Relay
const relayProvisioningReason = "the NeMo Relay runtime is being provisioned and no Relay consumer is configured"

var (
	relayReadinessMu sync.Mutex
	relayReadiness   func() bool
	relayDemandMu    sync.Mutex
	relayDemands     = pydict.New[string, func(ctx context.Context) bool]()
)

// Installs the check whether the registered binding loads without waiting for its runtime to be provisioned
func RegisterNemoRelayReadiness(ready func() bool) {
	relayReadinessMu.Lock()
	relayReadiness = ready
	relayReadinessMu.Unlock()
}

// Whether the binding loads without waiting for a provisioning, true when its loader registered no readiness check
func relayBindingReady() bool {
	relayReadinessMu.Lock()
	ready := relayReadiness
	relayReadinessMu.Unlock()
	return ready == nil || ready()
}

// Registers under name a check that a consumer of profile Relay events exists, a nil check removing it
func RegisterRelayDemand(name string, demanded func(ctx context.Context) bool) {
	relayDemandMu.Lock()
	defer relayDemandMu.Unlock()
	if demanded == nil {
		relayDemands.Delete(name)
		return
	}
	relayDemands.Set(name, demanded)
}

// Whether Relay has a consumer for ctx's profile, a plugin file Relay discovers, a plugin variable, or a registered consumer
func RelayDemanded(ctx context.Context) bool {
	if pystr.Strip(os.Getenv(RelayPluginsConfigEnv)) != "" || len(RelayDiscoveredPluginFiles()) > 0 {
		return true
	}
	environ := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			environ[k] = v
		}
	}
	if len(ConfiguredLegacyRelayEnvVars(environ)) > 0 {
		return true
	}
	relayDemandMu.Lock()
	checks := relayDemands.Values()
	relayDemandMu.Unlock()
	for _, demanded := range checks {
		if demanded(ctx) {
			return true
		}
	}
	return false
}

// Existing plugins.toml files Relay 0.11 discovers for this process, the user file then the system file
func RelayDiscoveredPluginFiles() []string {
	var candidates []string
	if user := relayUserConfigDir(); user != "" {
		candidates = append(candidates, filepath.Join(user, "nemo-relay", "plugins.toml"))
	}
	candidates = append(candidates, filepath.Join(relaySystemConfigDir(), "nemo-relay", "plugins.toml"))
	var found []string
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		}
	}
	return found
}

// Configuration directory Relay reads the system file under, %ProgramData% on Windows and /etc elsewhere, replaced by tests
var relaySystemConfigDir = func() string {
	if runtime.GOOS != "windows" {
		return "/etc"
	}
	if data := os.Getenv("ProgramData"); data != "" {
		return data
	}
	return `C:\ProgramData`
}

// Configuration directory Relay reads the user file under, an absolute XDG_CONFIG_HOME, else .config under HOME or an unset HOME's USERPROFILE
func relayUserConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return xdg
	}
	home, set := os.LookupEnv("HOME")
	if !set {
		home = os.Getenv("USERPROFILE")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config")
}
