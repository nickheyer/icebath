package provider

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

const xaiProxyPoolProvider = "xai-oauth"

// OpenAI compatible endpoints plus /responses because the native xAI runtime uses codex_responses
var xaiProxyAllowedPaths = map[string]bool{"/responses": true, "/chat/completions": true, "/completions": true, "/embeddings": true, "/models": true}

// XAIGrokAdapter is the proxy upstream for xAI Grok via Icebath managed OAuth credentials
type XAIGrokAdapter struct {
	env  PoolEnv
	mu   sync.Mutex
	pool *Pool
}

func newXAIGrokAdapter(env PoolEnv) *XAIGrokAdapter { return &XAIGrokAdapter{env: env} }

// AuthHint is the login command proxy start prints when no credential is available
func (a *XAIGrokAdapter) AuthHint() string { return "icebath auth add xai-oauth --type oauth" }

func (a *XAIGrokAdapter) Name() string { return "xai" }

func (a *XAIGrokAdapter) DisplayName() string { return "xAI Grok OAuth" }

func (a *XAIGrokAdapter) AllowedPaths() map[string]bool { return xaiProxyAllowedPaths }

func (a *XAIGrokAdapter) IsAuthenticated() bool {
	pool := a.loadPool()
	return pool != nil && pool.HasAvailable("")
}

// GetCredential selects a pool entry under the adapter lock
func (a *XAIGrokAdapter) GetCredential() (UpstreamCredential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pool := a.loadPool()
	if pool == nil || !pool.HasCredentials() {
		return UpstreamCredential{}, errors.New("No xAI OAuth credentials found. Run `icebath auth add xai-oauth --type oauth` first.")
	}
	entry, ok := pool.Select("")
	if !ok {
		return UpstreamCredential{}, errors.New("No available xAI OAuth credentials found. Run " +
			"`icebath auth reset xai-oauth` or re-authenticate with `icebath auth add xai-oauth --type oauth`.")
	}
	a.pool = pool
	return a.credentialFromEntry(entry)
}

// GetRetryCredential refreshes the key after a 401, rotates after a 429, nil when the pool has nothing else
func (a *XAIGrokAdapter) GetRetryCredential(failed UpstreamCredential, statusCode int) (*UpstreamCredential, error) {
	if statusCode != 401 && statusCode != 429 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	pool := a.pool
	if pool == nil {
		pool = a.loadPool()
	}
	if pool == nil {
		return nil, nil
	}
	var refreshed PooledCredential
	ok := false
	if statusCode == 401 {
		refreshed, ok = pool.TryRefreshCurrent()
	}
	if !ok {
		refreshed, ok = pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: statusCode})
	}
	if !ok {
		return nil, nil
	}
	retry, err := a.credentialFromEntry(refreshed)
	if err != nil {
		return nil, err
	}
	if retry.Bearer == failed.Bearer {
		return nil, nil
	}
	a.env.log().Info(fmt.Sprintf("proxy: xAI upstream returned %d; retrying with rotated pool credential", statusCode))
	return &retry, nil
}

func (a *XAIGrokAdapter) loadPool() *Pool {
	return LoadPool(a.env, xaiProxyPoolProvider)
}

// Upstream credential of a pool entry, its runtime key and endpoint with the OAuth default base URL
func (a *XAIGrokAdapter) credentialFromEntry(entry PooledCredential) (UpstreamCredential, error) {
	bearer := entry.RuntimeAPIKey()
	if bearer == "" {
		bearer = entry.AccessToken
	}
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return UpstreamCredential{}, errors.New("xAI OAuth credential pool entry did not contain an access token. " +
			"Re-authenticate with `icebath auth add xai-oauth --type oauth`.")
	}
	baseURL := entry.RuntimeBaseURL()
	if baseURL == "" {
		baseURL = entry.BaseURL
	}
	if baseURL == "" {
		baseURL = DefaultXAIOAuthBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultXAIOAuthBaseURL
	}
	return UpstreamCredential{Bearer: bearer, BaseURL: baseURL, TokenType: "Bearer", ExpiresAt: entry.ExpiresAt}, nil
}
