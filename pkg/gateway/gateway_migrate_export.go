package gateway

import (
	"context"
	"sync"
)

var (
	adapterModulesMu sync.RWMutex
	adapterModules   = map[string][]*AdapterClass{}
)

// Records adapter classes under the package of their constructor so a lambda factory's module can be scanned
func RegisterAdapterModule(classes ...*AdapterClass) {
	adapterModulesMu.Lock()
	defer adapterModulesMu.Unlock()
	for _, cls := range classes {
		module := callableModule(cls.New)
		adapterModules[module] = append(adapterModules[module], cls)
	}
}

// Whether any adapter class defined beside the entry's factory declares the trait
func FactoryModuleDeclares(entry *PlatformEntry, declares func(AdapterTraits) bool) bool {
	module := callableModule(entry.AdapterFactory)
	if module == "" {
		return false
	}
	adapterModulesMu.RLock()
	defer adapterModulesMu.RUnlock()
	for _, cls := range adapterModules[module] {
		if declares(cls.Traits) {
			return true
		}
	}
	return false
}

// Account named by the unit's User line, empty when the unit is absent or names none
func ReadSystemdUserFromUnit(unitPath string) (string, error) {
	return readSystemdUserFromUnit(unitPath)
}

// SystemScopeRequiresRootError unless this process runs as root
func RequireRootForSystemService(action string) error { return requireRootForSystemService(action) }

// Account, group, home dir and uid a system unit would run as, the ServiceUserError refusal otherwise
func SystemServiceIdentity(runAsUser string) (string, string, string, int, error) {
	return systemServiceIdentity(runAsUser)
}

// ICEBATH_HOME the unit file pins, empty when absent or unreadable
func HermesHomePinnedByUnit(unitPath string) (string, error) { return hermesHomePinnedByUnit(unitPath) }

// Sets gateway.multiplex_profiles in the default profile config through the guarded config writer
func WriteMultiplexFlag(ctx context.Context, defaultHome string, value bool) error {
	return writeMultiplexFlag(ctx, defaultHome, value)
}

// Owner uid of a live process from proc, ps on macOS, false when unknown
func PidUID(ctx context.Context, pid int) (int, bool) { return pidUID(ctx, pid) }

// Whether the launching home is a named profile that opted out with gateway.standalone
func StandaloneLauncher(ctx context.Context) (bool, error) { return standaloneLauncher(ctx) }
