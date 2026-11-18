package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Credential pool reads of the tool layer provider key resolver, each over the tool env's profile home
type poolKeySource struct{}

// Pool key reader the tool layer installs as tools.PoolKeys
func PoolKeySource() tools.PoolKeySource { return poolKeySource{} }

// Whether auth remove recorded the source as suppressed for the provider, false when the store is unreadable
func (poolKeySource) SourceSuppressed(env *tools.Env, providerID, source string) bool {
	pe := toolsPoolEnv(env)
	if pe.Store == nil {
		return false
	}
	return pe.Store.IsSourceSuppressed(providerID, source)
}

// Runtime key else access token of the first pooled credential of the pool key, empty when none
func (poolKeySource) PoolAPIKey(env *tools.Env, providerID string) string {
	pool := LoadPool(toolsPoolEnv(env), providerID)
	if pool == nil || !pool.HasCredentials() {
		return ""
	}
	entry, found := pool.Peek()
	if !found {
		return ""
	}
	if key := strings.TrimSpace(entry.RuntimeAPIKey()); key != "" {
		return key
	}
	return strings.TrimSpace(entry.AccessToken)
}
