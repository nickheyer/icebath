package provider

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Endpoints inference-api.nousresearch.com serves, anything else is a 404 so stray clients cannot leak requests upstream
var nousProxyAllowedPaths = map[string]bool{"/chat/completions": true, "/completions": true, "/embeddings": true, "/models": true}

// NousPortalAdapter is the proxy upstream for the Nous Portal inference API
type NousPortalAdapter struct {
	env PoolEnv
	mu  sync.Mutex
}

func newNousPortalAdapter(env PoolEnv) *NousPortalAdapter { return &NousPortalAdapter{env: env} }

func (a *NousPortalAdapter) Name() string { return "nous" }

func (a *NousPortalAdapter) DisplayName() string { return "Nous Portal" }

func (a *NousPortalAdapter) AllowedPaths() map[string]bool { return nousProxyAllowedPaths }

// IsAuthenticated is true with a usable inference JWT or a refresh and access token pair to recover from
func (a *NousPortalAdapter) IsAuthenticated() bool {
	state := a.readState()
	return pyTruthy(state["agent_key"]) || (pyTruthy(state["refresh_token"]) && pyTruthy(state["access_token"]))
}

func (a *NousPortalAdapter) GetCredential() (UpstreamCredential, error) {
	return a.getCredential(false, "")
}

// GetRetryCredential force refreshes the invoke JWT after a 401, no retry for any other status
func (a *NousPortalAdapter) GetRetryCredential(failed UpstreamCredential, statusCode int) (*UpstreamCredential, error) {
	if statusCode != 401 {
		return nil, nil
	}
	a.env.log().Info("proxy: Nous upstream rejected bearer; force-refreshing invoke JWT")
	cred, err := a.getCredential(true, failed.Bearer)
	if err != nil {
		return nil, err
	}
	return &cred, nil
}

// Fresh credential under the adapter lock, a terminal refresh failure quarantining the stored state
func (a *NousPortalAdapter) getCredential(forceRefresh bool, staleAccessToken string) (UpstreamCredential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.readState()
	if state == nil {
		return UpstreamCredential{}, errors.New("Not logged into Nous Portal. Run `icebath auth add nous` first.")
	}
	env := a.env
	env.Ctx = withBackgroundCaller(env.ctx())
	refreshed, err := resolveNousRuntimeCredentials(env, forceRefresh, staleAccessToken)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) && isTerminalNousRefreshError(err) {
			quarantineNousOAuthState(state, err, "proxy_refresh_failure")
			a.saveState(state, err, "proxy_refresh_failure")
		}
		return UpstreamCredential{}, fmt.Errorf("Failed to refresh Nous Portal credentials: %s", pyrepr.ExceptionMessage(err))
	}
	runtimeKey := anyString(refreshed["api_key"])
	if runtimeKey == "" {
		return UpstreamCredential{}, errors.New("Nous Portal refresh did not return a usable inference JWT. " +
			"Try `icebath auth add nous` to re-authenticate.")
	}
	baseURL := nousInferenceEnvOverrideIn(a.env)
	if baseURL == "" {
		baseURL = validateNousInferenceURLFromNetwork(anyString(refreshed["base_url"]))
	}
	if baseURL == "" {
		baseURL = DefaultNousInferenceURL
	}
	return UpstreamCredential{Bearer: runtimeKey, BaseURL: strings.TrimRight(baseURL, "/"), TokenType: "Bearer", ExpiresAt: anyString(refreshed["expires_at"])}, nil
}

// Nous provider state of the auth store, nil when the store cannot be read or holds none
func (a *NousPortalAdapter) readState() map[string]any {
	store, err := loadAuthStoreMaybeLocked(a.env, true)
	if err != nil {
		a.env.log().Warn(fmt.Sprintf("proxy: failed to load auth store: %s", pyrepr.ExceptionMessage(err)))
		return nil
	}
	providers, _ := store["providers"].(map[string]any)
	state, ok := providers["nous"].(map[string]any)
	if !ok {
		return nil
	}
	return maps.Clone(state)
}

// Persists the provider state after quarantining pool entries for a terminal error, then mirrors the shared Nous state
func (a *NousPortalAdapter) saveState(state map[string]any, quarantineErr error, quarantineReason string) {
	err := func() error {
		unlock, err := a.env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		store, err := a.env.Store.Load()
		if err != nil {
			return err
		}
		if quarantineErr != nil && quarantineReason != "" {
			quarantineNousPoolEntries(store, quarantineErr, quarantineReason)
		}
		providers, ok := store["providers"].(map[string]any)
		if !ok {
			if existing, present := store["providers"]; present && existing != nil {
				return fmt.Errorf("'%s' object does not support item assignment", pyTypeName(existing))
			}
			providers = map[string]any{}
			store["providers"] = providers
		}
		providers["nous"] = state
		return a.env.Store.Save(store)
	}()
	if err != nil {
		a.env.log().Warn(fmt.Sprintf("proxy: failed to persist Nous quarantine state: %s", pyrepr.ExceptionMessage(err)))
		return
	}
	writeSharedNousState(a.env, state, true)
}
