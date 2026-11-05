package provider

import (
	"fmt"
	"strings"
)

// Profile then root Nous state, a guest hidden while guests are off and any read failure reading as none
func readNousProviderState(env PoolEnv) map[string]any {
	if env.Store == nil {
		return nil
	}
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil || state == nil {
		return nil
	}
	if isGuestState(state) && !guestAllowed(env) {
		return nil
	}
	return state
}

// PeekNousAccessToken returns the cached connectors capable bearer with no expiry check and no network
func PeekNousAccessToken(env PoolEnv) string {
	return strings.TrimSpace(heldConnectorsTokenFor(readNousProviderState(env), 0, false))
}

// ReadNousAccessToken returns a live connectors bearer, refreshing it and replacing a retired guest once
func ReadNousAccessToken(env PoolEnv) (string, error) {
	state := readNousProviderState(env)
	if len(state) == 0 {
		return "", nil
	}
	if fresh := strings.TrimSpace(heldConnectorsToken(state, AccessTokenRefreshSkew)); fresh != "" {
		return fresh, nil
	}
	cached := PeekNousAccessToken(env)
	refreshed, err := ResolveNousAccessToken(env, NousAccessTokenOptions{RefreshSkew: AccessTokenRefreshSkew})
	if err == nil {
		if refreshed = strings.TrimSpace(refreshed); refreshed != "" {
			return refreshed, nil
		}
		return cached, nil
	}
	if code, dead := anonCredentialDeadCode(err); dead {
		if code == "" {
			code = AnonCredentialDead
		}
		return replaceDeadGuestToken(env, state, code)
	}
	env.log().Debug(fmt.Sprintf("Nous access token refresh failed: %v", err))
	return cached, nil
}

// Retires the dead guest and mints one replacement bearer, a locked account retired and never replaced
func replaceDeadGuestToken(env PoolEnv, deadState map[string]any, code string) (string, error) {
	if err := clearDeadGuest(env, code, anyString(deadState["anon_token"])); err != nil {
		return "", err
	}
	if code == AnonAccountLocked {
		return "", nil
	}
	identity, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err == nil && identity == nil {
		return "", nil
	}
	var token string
	if err == nil {
		token, err = ResolveNousAccessToken(env, NousAccessTokenOptions{RefreshSkew: AccessTokenRefreshSkew})
	}
	if err != nil {
		env.log().Debug(fmt.Sprintf("Nous free tier replacement after a retired credential failed: %v", err))
		return "", nil
	}
	return strings.TrimSpace(token), nil
}
