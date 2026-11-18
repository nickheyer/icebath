package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Reason of the host a turn gets while the runtime is unprovisioned and no Relay plugin is configured
const relayUnconfiguredReason = "the NeMo Relay runtime is not provisioned and no Relay plugin is configured"

var (
	relayReadinessMu sync.Mutex
	relayReadiness   func() bool
)

// Installs the check whether the registered binding finds its runtime provisioned
func RegisterNemoRelayReadiness(ready func() bool) {
	relayReadinessMu.Lock()
	relayReadiness = ready
	relayReadinessMu.Unlock()
}

// Whether the binding finds its runtime provisioned, true when its loader registered no readiness check
func relayBindingReady() bool {
	relayReadinessMu.Lock()
	ready := relayReadiness
	relayReadinessMu.Unlock()
	return ready == nil || ready()
}

// Whether Relay has a consumer for this process, a plugin file Relay discovers, the plugins variable or a legacy exporter variable
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
	return len(ConfiguredLegacyRelayEnvVars(environ)) > 0
}

// Warning for a session whose configured Relay plugins wait on an unprovisioned runtime, empty when Relay runs or has no consumer
func RelayUnprovisionedNotice(ctx context.Context) string {
	target, err := pm.CurrentTarget()
	if err != nil {
		return ""
	}
	if _, ok := pm.NemoRelayWheel(target); !ok || relayBindingReady() || !RelayDemanded(ctx) {
		return ""
	}
	return "Relay plugins are configured but the NeMo Relay runtime is not provisioned, so this session runs without them. Run `icebath pm install --extra nemo-relay` to provision it."
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
