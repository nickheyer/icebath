package tools

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func init() {
	BundledPlatformSecretEnvs = bundledPlatformManifestSecretEnvs
}

// Bundled platform manifest secrets minus core declared names, an unreadable bundled manifest fails the policy
func bundledPlatformManifestSecretEnvs() []string {
	names, err := config.PlatformManifestSecretEnvs(home.Process(), config.PlatformSourceBundled, true)
	if err != nil {
		panic(fmt.Sprintf("tools: bundled platform manifest secrets: %v", err))
	}
	return slices.Sorted(maps.Keys(names))
}

type homeAdapterSecrets struct {
	stamp    string
	complete bool
	names    map[string]bool
}

var (
	homeAdapterSecretMu    sync.Mutex
	homeAdapterSecretCache = map[string]homeAdapterSecrets{}
)

// Secrets the home's user platform plugins declare, recached when the manifest stamp moves, a partial scan keeps known names
func HomeAdapterSecretEnv(h string) (map[string]bool, error) {
	key, stamp := home.Key(h), config.PlatformManifestStamp(h)
	homeAdapterSecretMu.Lock()
	defer homeAdapterSecretMu.Unlock()
	cached, ok := homeAdapterSecretCache[key]
	if !ok || !cached.complete || cached.stamp != stamp {
		names, complete, err := config.PlatformManifestSecretScan(h)
		if err != nil {
			return nil, err
		}
		for n := range buildAdapterSecretEnv() {
			delete(names, n)
		}
		if !complete && ok {
			maps.Copy(names, cached.names)
		}
		cached = homeAdapterSecrets{stamp: stamp, complete: complete, names: names}
		homeAdapterSecretCache[key] = cached
	}
	return maps.Clone(cached.names), nil
}
