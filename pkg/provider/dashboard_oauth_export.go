package provider

import (
	"context"
	"net/http"
	"time"
)

// Fully read response of one OAuth endpoint call
type OAuthHTTPResponse = oauthHTTPResponse

// Saves Codex OAuth tokens to the auth store the grant was resolved from
func SaveCodexTokens(env PoolEnv, tokens map[string]any, lastRefresh, label string, setActive, writeThrough bool) error {
	return saveCodexTokens(env, tokens, lastRefresh, label, setActive, writeThrough)
}

// Whether an error is a transport failure the Codex login may retry
func CodexIsTransientTransportError(err error) bool { return codexIsTransientTransportError(err) }

// Fifteen second client of the Codex device login
func CodexLoginClient() *http.Client { return codexHTTPClientFn(15 * time.Second) }

// One POST of the Codex device login with the response body cap
func CodexLoginSend(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) (*OAuthHTTPResponse, error) {
	return codexPost(ctx, client, endpoint, body, headers)
}

// Auth error of the Codex provider carrying only a message
func CodexAuthError(message string) *AuthError { return codexErr(message, "", false) }

// OIDC discovery document of xAI
func XAIOAuthDiscovery(ctx context.Context, timeout time.Duration) (map[string]string, error) {
	return xaiOAuthDiscovery(ctx, timeout)
}

// Client of the xAI device login
func XAIHTTPClient(timeout time.Duration) *http.Client { return xaiHTTPClientFn(timeout) }

// Device code request of xAI with the default scope
func XAIOAuthRequestDeviceCode(ctx context.Context, client *http.Client) (map[string]any, error) {
	return xaiOAuthRequestDeviceCode(ctx, client, XAIOAuthScope)
}

// Polls the xAI token endpoint until the device code is approved or runs out
func XAIOAuthPollDeviceToken(ctx context.Context, client *http.Client, tokenEndpoint, deviceCode string, expiresIn, pollInterval int) (map[string]any, error) {
	return xaiOAuthPollDeviceToken(ctx, client, tokenEndpoint, deviceCode, expiresIn, pollInterval)
}

// Persists device code tokens of xAI without making the provider active
func SaveXAIOAuthDeviceTokens(env PoolEnv, tokens, discovery map[string]any, lastRefresh string) error {
	return saveXAIOAuthTokens(env, tokens, xaiSaveOptions{Discovery: discovery, AuthMode: "oauth_device_code", LastRefresh: lastRefresh})
}

// Verifier, challenge and state of one MiniMax login
func MinimaxPKCEPair() (string, string, string) { return minimaxPKCEPair() }

// Client of the MiniMax login
func MinimaxHTTPClient(timeout time.Duration) *http.Client { return minimaxHTTPClientFn(timeout) }

// User code request of the MiniMax login
func MinimaxRequestUserCode(ctx context.Context, client *http.Client, portalBaseURL, clientID, codeChallenge, state string) (map[string]any, error) {
	return minimaxRequestUserCode(ctx, client, portalBaseURL, clientID, codeChallenge, state)
}

// Polls the MiniMax token endpoint until the user code is approved or runs out
func MinimaxPollToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, userCode, codeVerifier string, expiredIn int64, intervalMS *int) (map[string]any, error) {
	return minimaxPollToken(ctx, client, portalBaseURL, clientID, userCode, codeVerifier, expiredIn, intervalMS)
}

// Unix expiry of a MiniMax token from its overloaded expired_in value
func MinimaxResolveTokenExpiryUnix(expiredIn int64, now time.Time) float64 {
	return minimaxResolveTokenExpiryUnix(expiredIn, now)
}

// Persists MiniMax OAuth state to the auth store
func MinimaxSaveAuthState(env PoolEnv, authState map[string]any, setActive bool) error {
	return minimaxSaveAuthState(env, authState, setActive)
}

// Auth registry row of a provider id, nil when the registry has none
func AuthRegistryGet(ctx context.Context, key string) *Profile { return authRegistryGet(ctx, key) }

// Auth type the auth registry lists a profile under
func RegistryAuthType(p Profile) string { return registryAuthType(p) }

// Latest free tier handoff of the profile, the idle state when none
func GuestHandoffStatus(env PoolEnv) (map[string]any, error) { return guestHandoffStatus(env) }

// Handoff status with its user copy lines added
func GuestHandoffWireStatus(status map[string]any) map[string]any {
	return guestHandoffWireStatus(status)
}
