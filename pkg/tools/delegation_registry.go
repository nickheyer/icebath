package tools

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Backend a blank or unset delegation.backend selects
const DefaultDelegationBackend = "default"

var delegationRegistryLog = slog.Default().With("logger", "agent.delegation_registry")

var bundledDelegationNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Wiring the provider and plugins lanes install, each panics when unassigned
var DelegationRegistryHooks struct {
	GetProvider     func(ctx context.Context, name string) DelegationProvider
	DisabledPlugins func(h string) map[string]bool
	LoadBundled     func(ctx context.Context, name string, register func(DelegationProvider)) bool
	DiscoverPlugins func(ctx context.Context, force bool)
	ListProviders   func(ctx context.Context) []DelegationProvider
}

var (
	delegationFirstUseMu sync.Mutex
	delegationFirstUse   = map[string]DelegationProvider{}
)

// Lowercased stripped delegation.backend, default when blank or not a string
func ConfiguredBackend(config map[string]any) string {
	if raw, ok := config["backend"].(string); ok && pystr.Strip(raw) != "" {
		return pystr.Lower(pystr.Strip(raw))
	}
	return DefaultDelegationBackend
}

func registeredDelegationProvider(ctx context.Context, name string) DelegationProvider {
	return DelegationRegistryHooks.GetProvider(ctx, name)
}

func bundledDelegationDisabled(ctx context.Context, name string) bool {
	disabled := DelegationRegistryHooks.DisabledPlugins(home.Dir(ctx))
	return disabled[name] || disabled["delegation/"+name]
}

// The one registrar method a bundled strategy's register call reaches, keeping the provider outside the plugin registry
type bundledRegistrar struct{ name string }

// Records the bundled provider as the first use strategy of its name
func (r bundledRegistrar) RegisterDelegationProvider(p DelegationProvider) {
	delegationFirstUseMu.Lock()
	delegationFirstUse[r.name] = p
	delegationFirstUseMu.Unlock()
}

// Bundled strategy of the name on first use, disabled plugins winning on every call
func loadBundledDelegation(ctx context.Context, name string) DelegationProvider {
	if !bundledDelegationNameRE.MatchString(name) || bundledDelegationDisabled(ctx, name) {
		return nil
	}
	delegationFirstUseMu.Lock()
	cached, seen := delegationFirstUse[name]
	delegationFirstUseMu.Unlock()
	if seen {
		return cached
	}
	if !DelegationRegistryHooks.LoadBundled(ctx, name, bundledRegistrar{name}.RegisterDelegationProvider) {
		return nil
	}
	delegationFirstUseMu.Lock()
	defer delegationFirstUseMu.Unlock()
	return delegationFirstUse[name]
}

// Exported loadBundledDelegation
func LoadBundledDelegation(ctx context.Context, name string) DelegationProvider {
	return loadBundledDelegation(ctx, name)
}

// Forgets every first use bundled strategy, the hermes _first_use reset of tests
func ResetDelegationFirstUse() {
	delegationFirstUseMu.Lock()
	delegationFirstUse = map[string]DelegationProvider{}
	delegationFirstUseMu.Unlock()
}

// Provider availability with a panicking provider reported unavailable at debug level
func delegationAvailable(p DelegationProvider) (available bool) {
	if p == nil {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			delegationRegistryLog.Debug(fmt.Sprintf("delegation provider %s.is_available() raised %v", p.Name(), rec))
			available = false
		}
	}()
	return p.IsAvailable()
}

// Available provider for the name from the registry, then the bundled strategy, then one discovery sweep
func usableDelegationProvider(ctx context.Context, name string) DelegationProvider {
	registered := registeredDelegationProvider(ctx, name)
	if delegationAvailable(registered) {
		return registered
	}
	bundled := loadBundledDelegation(ctx, name)
	if delegationAvailable(bundled) {
		return bundled
	}
	if registered == nil && bundled == nil {
		DelegationRegistryHooks.DiscoverPlugins(ctx, false)
		registered = registeredDelegationProvider(ctx, name)
		if delegationAvailable(registered) {
			return registered
		}
	}
	return nil
}

// Provider and empty reason when the backend runs here, else the provider or nil with the user facing fix
func DelegationBackendStatus(ctx context.Context, name string) (DelegationProvider, string) {
	if p := usableDelegationProvider(ctx, name); p != nil {
		return p, ""
	}
	registered := registeredDelegationProvider(ctx, name)
	if registered == nil && name == DefaultDelegationBackend {
		return nil, "No default delegation strategy is available: the bundled plugin delegation/default is " +
			"disabled. Remove it from plugins.disabled in config.yaml."
	}
	if registered == nil {
		return nil, fmt.Sprintf("Delegation backend '%s' is not installed: no enabled plugin provides it. Install or enable the "+
			"plugin that does (`icebath plugins list`), or switch back to the default backend "+
			"(`icebath config set delegation.backend default`).", name)
	}
	return registered, fmt.Sprintf("Delegation backend '%s' is unavailable here (its plugin reports it cannot run). Fix its setup, or "+
		"switch back to the default backend: `/delegation default` for this session, or "+
		"`icebath config set delegation.backend default`.", name)
}

// Provider the delegation section selects, a route error naming the backend and the fix otherwise
func GetActiveDelegationProvider(ctx context.Context, config map[string]any) (DelegationProvider, error) {
	p, reason := DelegationBackendStatus(ctx, ConfiguredBackend(config))
	if reason != "" || p == nil {
		return nil, &DelegationRouteError{Message: reason + " No subagent was started."}
	}
	return p, nil
}
