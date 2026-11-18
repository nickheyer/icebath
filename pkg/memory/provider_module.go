package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Synthetic parent package of non bundled provider packages in the plugin host
const userMemoryNamespace = "_icebath_user_memory"

// Module name a provider package loads under, plugins.memory for bundled, the synthetic user namespace otherwise
func providerModuleName(r *ProviderRegistration) string {
	if r.Source == SourceBundled {
		return "plugins.memory." + r.Name
	}
	resolved, err := pypath.Realpath(r.Dir, pypath.Lax)
	if err != nil {
		resolved = r.Dir
	}
	sum := sha256.Sum256([]byte(resolved))
	return fmt.Sprintf("%s.%s__source_%s", userMemoryNamespace, r.Name, hex.EncodeToString(sum[:])[:16])
}
