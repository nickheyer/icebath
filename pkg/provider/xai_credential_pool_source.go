package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Credential pool answers the direct xAI HTTP tools read, each over the tool env's profile home
type xaiCredentialPoolSource struct{}

// Pool source the tool layer installs as tools.CredentialPools
func XAICredentialPoolSource() tools.CredentialPoolSource { return xaiCredentialPoolSource{} }

// Pool of a provider for a tool env, the process home when no env is bound
func toolEnvPool(env *tools.Env, providerID string) *Pool {
	return LoadPool(toolsPoolEnv(env), providerID)
}

// Runtime bearer then access token, and runtime base url then stored base url, of one entry
func pooledBearer(entry PooledCredential) (string, string) {
	token := strings.TrimSpace(entry.RuntimeAPIKey())
	if token == "" {
		token = strings.TrimSpace(entry.AccessToken)
	}
	base := entry.RuntimeBaseURL()
	if base == "" {
		base = entry.BaseURL
	}
	return token, base
}

// Selected entry of the provider pool, expiring rows refreshed, ok false on no entry or any failure
func (xaiCredentialPoolSource) SelectPooled(env *tools.Env, providerID string) (token, baseURL string, ok bool) {
	defer func() {
		if recover() != nil {
			token, baseURL, ok = "", "", false
		}
	}()
	pool := toolEnvPool(env, providerID)
	if pool == nil {
		return "", "", false
	}
	entry, found := pool.Select("")
	if !found {
		return "", "", false
	}
	token, baseURL = pooledBearer(entry)
	return token, baseURL, true
}

// Force refreshed entry that issued the rejected bearer, ok false when none matches or on any failure
func (xaiCredentialPoolSource) RefreshMatching(env *tools.Env, providerID, apiKeyHint string) (token, baseURL string, ok bool) {
	defer func() {
		if recover() != nil {
			token, baseURL, ok = "", "", false
		}
	}()
	pool := toolEnvPool(env, providerID)
	if pool == nil {
		return "", "", false
	}
	entry, found := pool.TryRefreshMatching(apiKeyHint, "")
	if !found {
		return "", "", false
	}
	token, baseURL = pooledBearer(entry)
	return token, baseURL, true
}
