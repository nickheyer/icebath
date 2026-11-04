package provider

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Waits before each retry of an unexpired Nous key that keeps answering 401
var TransientAuthBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

const (
	// Cap on same entry OAuth refreshes during one persistent auth failure
	maxAuthRefreshAttempts    = 2
	authRecoveryActivityTouch = 30 * time.Second
	// A 401 this long after the episode's last one starts a new episode
	authEpisodeIdle = 120 * time.Second
)

// ProfileCLISelector is the -p name selector pinning copy pasteable guidance to the active named profile
func ProfileCLISelector(h string) string {
	name := home.ProfileNameFor(h)
	if name != "" && name != "default" {
		return "-p " + name + " "
	}
	return ""
}

// OAuthReloginCommand is the exact re-login command for a rejected OAuth grant
func OAuthReloginCommand(provider string) string {
	return oauthReloginCommandIn(home.Process(), provider)
}

func oauthReloginCommandIn(h, provider string) string {
	slug := strings.ToLower(strings.TrimSpace(provider))
	if slug == "nous" {
		return "icebath " + ProfileCLISelector(h) + "portal"
	}
	return "icebath " + ProfileCLISelector(h) + "auth add " + slug + " --type oauth"
}

// ProviderAuthKind is oauth for providers whose credential is an OAuth grant, api_key for everything else
func ProviderAuthKind(provider string) string {
	descriptor, ok := ProviderCatalogBySlug()[strings.ToLower(strings.TrimSpace(provider))]
	if ok && descriptor.Tab == "accounts" {
		return "oauth"
	}
	return "api_key"
}

// ReloginCommandHint is the re-sign-in command for a rejected credential on surfaces that may not know the provider
func ReloginCommandHint(h, provider string) string {
	slug := strings.ToLower(strings.TrimSpace(provider))
	if slug == "" {
		return "icebath " + ProfileCLISelector(h) + "auth add <provider>"
	}
	if ProviderAuthKind(slug) == "oauth" {
		return oauthReloginCommandIn(h, slug)
	}
	return "icebath " + ProfileCLISelector(h) + "auth add " + slug
}

// AuthRecoveryAgent is the live session the credential recovery acts on
type AuthRecoveryAgent interface {
	Provider() string
	APIMode() APIMode
	Interrupted() bool
	TouchActivity(message string)
	Print(message string)
	// Adopts the entry as the live credential, false when its route cannot serve the model
	SwapCredential(entry PooledCredential) bool
	AuthEpisodes() map[string]*AuthEpisode
}

// AuthEpisode is the per turn auth failure state for one pool entry
type AuthEpisode struct {
	Refreshes int
	Waits     int
	Last      time.Time
}

func episodeKey(provider, entryID string) string { return provider + "\x00" + entryID }

// Per turn auth failure state for one pool entry, a new episode after the idle window
func authEpisode(agent AuthRecoveryAgent, provider, entryID string) *AuthEpisode {
	episodes := agent.AuthEpisodes()
	now := nowFunc()
	key := episodeKey(provider, entryID)
	ep := episodes[key]
	if ep == nil || now.Sub(ep.Last) > authEpisodeIdle {
		ep = &AuthEpisode{}
		episodes[key] = ep
	}
	ep.Last = now
	return ep
}

// NousPoolRefreshedThisTurn is true once the pool spent this turn's forced Nous refresh
func NousPoolRefreshedThisTurn(agent AuthRecoveryAgent) bool {
	for key, ep := range agent.AuthEpisodes() {
		if strings.HasPrefix(key, "nous\x00") && ep.Refreshes > 0 {
			return true
		}
	}
	return false
}

// Sleeps in 200ms slices touching activity every 30s, false when the user interrupted
func interruptibleWait(agent AuthRecoveryAgent, wait time.Duration) bool {
	deadline := nowFunc().Add(wait)
	nextTouch := nowFunc().Add(authRecoveryActivityTouch)
	for nowFunc().Before(deadline) {
		if agent.Interrupted() {
			return false
		}
		remaining := deadline.Sub(nowFunc())
		if remaining > 200*time.Millisecond {
			remaining = 200 * time.Millisecond
		}
		if remaining > 0 {
			sleepFunc(remaining)
		}
		if !nowFunc().Before(nextTouch) {
			agent.TouchActivity("waiting out a Nous 401 on a valid key")
			nextTouch = nowFunc().Add(authRecoveryActivityTouch)
		}
	}
	return true
}

func poolEntryFor(pool *Pool, credentialID, apiKeyHint string) (PooledCredential, bool) {
	entries := pool.Entries()
	if credentialID != "" {
		for _, e := range entries {
			if e.ID == credentialID {
				return e, true
			}
		}
	}
	if apiKeyHint != "" {
		for _, e := range entries {
			if e.RuntimeAPIKey() == apiKeyHint {
				return e, true
			}
		}
	}
	return PooledCredential{}, false
}

// IsEntitlement403 is a 403 that looks like an auth failure but refresh cannot fix
func IsEntitlement403(agent AuthRecoveryAgent, statusCode int, errorContext map[string]any) bool {
	if IsEntitlementFailure(errorContext, statusCode) {
		return true
	}
	if statusCode != 403 {
		return false
	}
	var parts []string
	for _, k := range []string{"message", "reason", "code", "error"} {
		parts = append(parts, strings.ToLower(anyString(errorContext[k])))
	}
	haystack := strings.Join(parts, " ")
	if strings.Contains(haystack, "oauth authentication is currently not allowed for this organization") {
		return true
	}
	provider := agent.Provider()
	if provider == "anthropic" && agent.APIMode() == ModeAnthropicMessages {
		return true
	}
	if provider == "xai-oauth" {
		return !(strings.Contains(haystack, "[wke=unauthenticated:") || strings.Contains(haystack, "oauth2 access token could not be validated"))
	}
	return false
}

// Waits out a server side 401 on an unexpired key then retries with the store's current key
func retryAfterBackoff(agent AuthRecoveryAgent, pool *Pool, ep *AuthEpisode, failedKey, entryID, credentialID string, hasRetried429 bool) (bool, bool) {
	log := pool.env.log()
	step := ep.Waits
	if step >= len(TransientAuthBackoff) {
		log.Warn(fmt.Sprintf("Credential auth failure persists for pool entry %s after one refresh and %d backoff retries — treating as unrecoverable and allowing fallback to activate.", entryID, step))
		return false, hasRetried429
	}
	wait := TransientAuthBackoff[step]
	ep.Waits = step + 1
	log.Info(fmt.Sprintf("Credential auth failure on an unexpired key for pool entry %s — transient 401, retrying in %.0fs (backoff %d/%d, no further refresh)", entryID, wait.Seconds(), step+1, len(TransientAuthBackoff)))
	agent.Print(fmt.Sprintf("🔐 Nous rejected a still-valid key (transient); retrying in %.0fs...", wait.Seconds()))
	if !interruptibleWait(agent, wait) {
		return false, hasRetried429
	}
	lookup := credentialID
	if lookup == "" {
		lookup = entryID
	}
	if entry, ok := poolEntryFor(pool, lookup, failedKey); ok {
		if !agent.SwapCredential(pool.syncNousEntryFromAuthStore(entry, false)) {
			return false, hasRetried429
		}
	}
	return true, hasRetried429
}

// AuthRecoveryRequest carries the failed request identity into the recovery
type AuthRecoveryRequest struct {
	StatusCode    int
	HasRetried429 bool
	ErrorContext  map[string]any
	APIKeyHint    string
	CredentialID  string
	// Rotates away from the failed credential, true when a new entry was swapped in
	RotateAndSwap func(defaultStatus int, label string) bool
}

// RecoverAuthFailure refreshes or rotates the dispatched credential after a 401 or 403, returning recovered and the 429 flag
func RecoverAuthFailure(agent AuthRecoveryAgent, pool *Pool, req AuthRecoveryRequest) (bool, bool) {
	log := pool.env.log()
	if IsEntitlement403(agent, req.StatusCode, req.ErrorContext) {
		status := "auth"
		if req.StatusCode != 0 {
			status = fmt.Sprint(req.StatusCode)
		}
		provider := agent.Provider()
		if provider == "" {
			provider = "provider"
		}
		log.Info(fmt.Sprintf("Credential %s — entitlement-shaped 403 from %s; skipping pool refresh (account lacks subscription, not a transient auth failure).", status, provider))
		return false, req.HasRetried429
	}
	provider := agent.Provider()
	nous := provider == "nous" && req.StatusCode == 401
	entryID := req.CredentialID
	if nous {
		if found, ok := poolEntryFor(pool, req.CredentialID, req.APIKeyHint); ok && found.ID != "" {
			entryID = found.ID
		}
		ep := authEpisode(agent, provider, entryID)
		if ep.Refreshes >= 1 {
			return retryAfterBackoff(agent, pool, ep, req.APIKeyHint, entryID, req.CredentialID, req.HasRetried429)
		}
	}
	refreshed, ok := pool.TryRefreshMatching(req.APIKeyHint, req.CredentialID)
	if !ok {
		if req.RotateAndSwap(401, "auth refresh failed") {
			return true, false
		}
		return false, req.HasRetried429
	}
	refreshedID := refreshed.ID
	if nous {
		key := refreshedID
		if key == "" {
			key = entryID
		}
		ep := authEpisode(agent, provider, key)
		ep.Refreshes++
		if req.APIKeyHint != "" && refreshed.RuntimeAPIKey() == req.APIKeyHint {
			return retryAfterBackoff(agent, pool, ep, req.APIKeyHint, refreshedID, req.CredentialID, req.HasRetried429)
		}
	} else if refreshedID != "" {
		ep := authEpisode(agent, provider, refreshedID)
		ep.Refreshes++
		if ep.Refreshes > maxAuthRefreshAttempts {
			log.Warn(fmt.Sprintf("Credential auth failure persists after %d refreshes for pool entry %s — treating as unrecoverable and allowing fallback to activate.", ep.Refreshes-1, refreshedID))
			return false, req.HasRetried429
		}
	}
	shown := refreshedID
	if shown == "" {
		shown = "?"
	}
	log.Info(fmt.Sprintf("Credential auth failure — refreshed pool entry %s", shown))
	if !agent.SwapCredential(refreshed) {
		return false, req.HasRetried429
	}
	return true, req.HasRetried429
}
