package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NousResolveOptions are the per call knobs of the Nous runtime credential resolver
type NousResolveOptions struct {
	Timeout          time.Duration
	Insecure         *bool
	CABundle         string
	ForceRefresh     bool
	StaleAccessToken string
}

func (o NousResolveOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 15 * time.Second
	}
	return o.Timeout
}

// Working set for one resolve call, the token pair and routing that merges and refreshes replace mid flight
type nousRuntimeResolve struct {
	env              PoolEnv
	tx               *ProviderTxn
	state            map[string]any
	forceRefresh     bool
	staleAccessToken string
	timeout          time.Duration
	sequenceID       string
	persistedState   map[string]any
	persistedAny     bool
	accessToken      string
	refreshToken     string
	portalBaseURL    string
	storedInference  string
	inferenceBaseURL string
	clientID         string
}

func nousSequenceID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newNousRuntimeResolve(env PoolEnv, tx *ProviderTxn, state map[string]any, opts NousResolveOptions) *nousRuntimeResolve {
	run := &nousRuntimeResolve{env: env, tx: tx, state: state, forceRefresh: opts.ForceRefresh,
		staleAccessToken: opts.StaleAccessToken, timeout: opts.timeout(), sequenceID: nousSequenceID(),
		persistedState: cloneMap(state)}
	run.accessToken, _ = state["access_token"].(string)
	run.refreshToken, _ = state["refresh_token"].(string)
	run.reloadRouting()
	return run
}

func (run *nousRuntimeResolve) reloadRouting() {
	run.portalBaseURL, run.storedInference, run.inferenceBaseURL, run.clientID = nousEffectiveRouting(run.env, run.state)
}

// Saves the state to its source store unless only derived countdowns changed, mirroring real writes to the shared store
func (run *nousRuntimeResolve) persist(reason string) error {
	if nousStatesEqual(nousEffectiveProviderState(run.state), nousEffectiveProviderState(run.persistedState)) {
		nousOAuthTrace(run.env, "nous_state_persist_skipped", run.sequenceID, map[string]any{"reason": reason})
		return nil
	}
	if err := run.tx.SaveState(run.state); err != nil {
		nousOAuthTrace(run.env, "nous_state_persist_failed", run.sequenceID, map[string]any{"reason": reason, "error_type": fmt.Sprintf("%T", err)})
		return err
	}
	nousOAuthTrace(run.env, "nous_state_persisted", run.sequenceID, map[string]any{"reason": reason,
		"refresh_token_fp": nilIfEmpty(nousTokenFingerprint(run.state["refresh_token"])),
		"access_token_fp":  nilIfEmpty(nousTokenFingerprint(run.state["access_token"]))})
	run.persistedState = cloneMap(run.state)
	run.persistedAny = true
	writeSharedNousState(run.env, run.state, true)
	return nil
}

func (run *nousRuntimeResolve) sharedLock() (func(), error) {
	return nousSharedStoreLock(run.env, nousSharedLockTimeout(run.timeout))
}

func (run *nousRuntimeResolve) hasAccessToken() bool { return run.accessToken != "" }

func (run *nousRuntimeResolve) invokeJWTStatus() string {
	return nousStateInvokeJWTStatus(run.state, run.accessToken)
}

// Adopts fresher shared store tokens under the shared lock, true when merged
func (run *nousRuntimeResolve) mergeShared() bool {
	if !mergeSharedNousOAuthState(run.env, run.state) {
		return false
	}
	run.accessToken, _ = run.state["access_token"].(string)
	run.refreshToken, _ = run.state["refresh_token"].(string)
	run.reloadRouting()
	return true
}

// Drops the forced refresh when the on disk bearer already differs from the failed one and is usable
func (run *nousRuntimeResolve) skipRefreshIfPeerRotated() {
	token := run.accessToken
	if run.forceRefresh && run.staleAccessToken != "" && token != "" && token != run.staleAccessToken && run.invokeJWTStatus() == "" {
		nousOAuthTrace(run.env, "refresh_skipped_peer_rotated", run.sequenceID, map[string]any{"access_token_fp": nilIfEmpty(nousTokenFingerprint(token))})
		run.forceRefresh = false
	}
}

// Redeems the refresh token then applies and persists the rotated pair, caller holds both locks
func (run *nousRuntimeResolve) refresh(client *http.Client, invokeJWTStatus string) error {
	if run.refreshToken == "" {
		reason := invokeJWTStatus
		if reason == "" {
			reason = "force_refresh"
		}
		return nousUnusableInvokeJWTError(reason, true)
	}
	refreshReason := invokeJWTStatus
	if run.forceRefresh {
		refreshReason = "force_refresh"
	} else if refreshReason == "" {
		refreshReason = "access_unusable"
	}
	nousOAuthTrace(run.env, "refresh_start", run.sequenceID, map[string]any{"reason": refreshReason, "refresh_token_fp": nilIfEmpty(nousTokenFingerprint(run.refreshToken))})
	refreshed, err := refreshNousOrQuarantine(run.env, client, run.tx, run.state, run.portalBaseURL, run.clientID, run.refreshToken,
		"runtime_access_refresh_failure", func() error { return run.persist("terminal_runtime_access_refresh_failure") })
	if err != nil {
		return err
	}
	previousRefreshToken := run.refreshToken
	run.storedInference = nousHealedInferenceURL(run.env, refreshed)
	run.inferenceBaseURL = nousInferenceEnvOverrideIn(run.env)
	if run.inferenceBaseURL == "" {
		run.inferenceBaseURL = run.storedInference
	}
	applyNousRefreshedTokens(run.state, refreshed, run.refreshToken, run.storedInference)
	run.accessToken = anyString(run.state["access_token"])
	run.refreshToken = anyString(run.state["refresh_token"])
	nousOAuthTrace(run.env, "refresh_success", run.sequenceID, map[string]any{"reason": refreshReason,
		"previous_refresh_token_fp": nilIfEmpty(nousTokenFingerprint(previousRefreshToken)),
		"new_refresh_token_fp":      nilIfEmpty(nousTokenFingerprint(run.refreshToken))})
	return run.persist("post_refresh_access_token")
}

// Merges from the shared store or refreshes until the access token is a usable invoke JWT
func (run *nousRuntimeResolve) ensureUsableAccessToken(client *http.Client) error {
	if isGuestState(run.state) {
		if run.forceRefresh || run.invokeJWTStatus() != "" {
			if _, err := refreshGuestState(run.env, run.state, client, "inference"); err != nil {
				return err
			}
			run.accessToken = anyString(run.state["access_token"])
			if stored := anyString(run.state["inference_base_url"]); stored != "" {
				run.storedInference = stored
			}
			run.inferenceBaseURL = nousInferenceEnvOverrideIn(run.env)
			if run.inferenceBaseURL == "" {
				run.inferenceBaseURL = run.storedInference
			}
			return run.persist("guest_exchange")
		}
		return nil
	}
	if !run.hasAccessToken() {
		err := func() error {
			unlock, err := run.sharedLock()
			if err != nil {
				return err
			}
			defer unlock()
			if run.mergeShared() {
				return run.persist("runtime_shared_merge_missing_access_token")
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	if !run.hasAccessToken() {
		return nousErr("No access token found for Nous Portal login.", "nous_auth_missing_access_token", true)
	}
	invokeJWTStatus := run.invokeJWTStatus()
	run.skipRefreshIfPeerRotated()
	if !(run.forceRefresh || invokeJWTStatus != "") {
		return nil
	}
	unlock, err := run.sharedLock()
	if err != nil {
		return err
	}
	defer unlock()
	if run.mergeShared() {
		invokeJWTStatus = run.invokeJWTStatus()
		if err := run.persist("post_shared_merge_access_unusable"); err != nil {
			return err
		}
		run.skipRefreshIfPeerRotated()
	}
	if run.forceRefresh || invokeJWTStatus != "" {
		return run.refresh(client, invokeJWTStatus)
	}
	return nil
}

// Resolves Nous inference credentials for runtime use, a dead guest credential retired and never replaced here
func resolveNousRuntimeCredentialsWith(env PoolEnv, opts NousResolveOptions) (map[string]any, error) {
	var result map[string]any
	_, err := runWithChallenge(env, func() (string, error) {
		creds, err := resolveNousRuntimeCredentialsLocked(env, opts)
		if err != nil {
			return "", err
		}
		result = creds
		return "", nil
	})
	if err != nil {
		if code, dead := anonCredentialDeadCode(err); dead {
			if code == "" {
				code = "anon_credential_dead"
			}
			deadToken := ""
			if state, stateErr := env.Store.GetProviderAuthState("nous"); stateErr == nil && state != nil {
				deadToken = anyString(state["anon_token"])
			}
			clearDeadGuest(env, code, deadToken)
		}
		return nil, err
	}
	return result, nil
}

func resolveNousRuntimeCredentials(env PoolEnv, forceRefresh bool, staleAccessToken string) (map[string]any, error) {
	return resolveNousRuntimeCredentialsWith(env, NousResolveOptions{ForceRefresh: forceRefresh, StaleAccessToken: staleAccessToken})
}

// Resolves under the auth store lock, skipping the refresh POST when a peer already rotated the grant
func resolveNousRuntimeCredentialsLocked(env PoolEnv, opts NousResolveOptions) (map[string]any, error) {
	var run *nousRuntimeResolve
	var state map[string]any
	statePath := ""
	err := env.Store.ProviderTransaction("nous", AuthLockTimeout, func(tx *ProviderTxn) error {
		state = tx.State
		statePath = tx.SourcePath
		if len(state) == 0 {
			return nousErr("Icebath is not logged into Nous Portal.", "nous_auth_missing", true)
		}
		if guestWithoutFreeTier(state) {
			return nousErr("Icebath is not logged into Nous Portal.", "nous_auth_missing", false)
		}
		run = newNousRuntimeResolve(env, tx, state, opts)
		verify := resolveOAuthVerify(env, opts.Insecure, opts.CABundle, state)
		nousOAuthTrace(env, "nous_runtime_credentials_start", run.sequenceID, map[string]any{"refresh_token_fp": nilIfEmpty(nousTokenFingerprint(state["refresh_token"]))})
		client := nousHTTPClientFn(env, opts.timeout(), verify)
		if err := run.ensureUsableAccessToken(client); err != nil {
			return err
		}
		if err := nousAssertInferenceJWTUsable(state, run.accessToken); err != nil {
			return err
		}
		nousSelectInvokeJWT(env, state, run.accessToken, run.sequenceID)
		state["portal_base_url"] = run.portalBaseURL
		state["client_id"] = run.clientID
		state["inference_base_url"] = run.storedInference
		state["tls"] = tlsStateFromVerify(verify.Insecure, verify.CABundle)
		return run.persist("resolve_nous_runtime_credentials_final")
	})
	if err != nil {
		return nil, err
	}
	if run.persistedAny {
		nousSyncPoolFromAuthStore(env)
	}
	apiKey, _ := state["agent_key"].(string)
	if apiKey == "" {
		return nil, nousErr("Failed to resolve a Nous inference API key", "server_error", false)
	}
	if statePath == "" {
		statePath = env.Store.Path()
	}
	expiresAt := state["agent_key_expires_at"]
	return map[string]any{
		"provider": "nous", "base_url": run.inferenceBaseURL, "api_key": apiKey,
		"key_id": state["agent_key_id"], "expires_at": expiresAt,
		"expires_in": nousRemainingTTL(expiresAt, state["agent_key_expires_in"]),
		"source":     NousAuthPathInvokeJWT, "auth_path": NousAuthPathInvokeJWT, "state_path": statePath,
	}, nil
}

// Pricing and catalog reads resolve the key without opening a free tier challenge
func resolveNousRuntimeCredentialsBackground(ctx context.Context) (apiKey, baseURL string, ok bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	env := modelsPoolEnv(withBackgroundCaller(ctx))
	creds, err := resolveNousRuntimeCredentialsWith(env, NousResolveOptions{})
	if err != nil || len(creds) == 0 {
		return "", "", false
	}
	return anyString(creds["api_key"]), anyString(creds["base_url"]), true
}

// ---- status ----

func nousEmptyAuthStatus() map[string]any {
	return map[string]any{
		"logged_in": false, "portal_base_url": nil, "inference_base_url": nil,
		"access_expires_at": nil, "agent_key_expires_at": nil, "has_refresh_token": false,
		"inference_credential_present": false, "credential_source": nil,
	}
}

// Nil for an empty string so status rows carry null rather than blank fields
func nousNilOrString(s string) any { return nilIfEmpty(s) }

// Status from the credential pool, the fallback when the auth store holds no Portal login
func nousSnapshotPoolStatus(env PoolEnv) (status map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			status = nousEmptyAuthStatus()
		}
	}()
	pool := LoadPool(env, "nous")
	var entries []PooledCredential
	if pool != nil && pool.HasCredentials() {
		for _, e := range pool.Entries() {
			if anyString(e.Extra["auth_method"]) != AnonAccountTier {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 {
		return nousEmptyAuthStatus()
	}
	stamp := func(e PooledCredential) (float64, float64, int) {
		keyExp, _ := ParseISOTimestamp(e.AgentKeyExpiresAt)
		accessExp, _ := ParseISOTimestamp(e.ExpiresAt)
		return keyExp, accessExp, -e.Priority
	}
	entry := entries[0]
	for _, candidate := range entries[1:] {
		ck, ca, cp := stamp(candidate)
		ek, ea, ep := stamp(entry)
		if ck > ek || (ck == ek && (ca > ea || (ca == ea && cp > ep))) {
			entry = candidate
		}
	}
	if entry.RuntimeAPIKey() == "" {
		return nousEmptyAuthStatus()
	}
	authType := strings.ToLower(strings.TrimSpace(entry.AuthType))
	isPortalOAuth := entry.AccessToken != "" && (strings.HasPrefix(authType, "oauth") || entry.RefreshToken != "")
	label := entry.Label
	if label == "" {
		label = "unknown"
	}
	var portal, accessToken any
	if isPortalOAuth {
		portal = anyString(nousOr(entry.Extra["portal_base_url"], DefaultNousPortalURL))
		accessToken = entry.AccessToken
	}
	inference := entry.InferenceBaseURL
	if inference == "" {
		inference = entry.RuntimeBaseURL()
	}
	if inference == "" {
		inference = entry.BaseURL
	}
	return map[string]any{
		"logged_in": isPortalOAuth, "portal_base_url": portal,
		"inference_base_url": nousNilOrString(inference), "access_token": accessToken,
		"access_expires_at": nousNilOrString(entry.ExpiresAt), "agent_key_expires_at": nousNilOrString(entry.AgentKeyExpiresAt),
		"has_refresh_token": entry.RefreshToken != "", "inference_credential_present": true,
		"credential_source": "pool:" + label, "source": "pool:" + label,
	}
}

// Auth store backed Nous status snapshot shared by the live and refresh free variants
func nousStatusFromState(state map[string]any, loggedIn bool, source string) map[string]any {
	accessToken := state["access_token"]
	var accountTier any
	if tier, ok := state["account_tier"].(string); ok {
		accountTier = tier
	}
	return map[string]any{
		"logged_in": loggedIn, "portal_base_url": state["portal_base_url"],
		"inference_base_url": state["inference_base_url"], "access_expires_at": state["expires_at"],
		"agent_key_expires_at": state["agent_key_expires_at"], "has_refresh_token": truthyAny(state["refresh_token"]),
		"access_token": accessToken, "inference_credential_present": truthyAny(accessToken) || truthyAny(state["agent_key"]),
		"credential_source": "auth_store", "source": source, "account_tier": accountTier, "free_tier": isGuestState(state),
	}
}

// Uncached live Nous status, a resolve that must not park on a free tier challenge
func computeNousAuthStatus(env PoolEnv) map[string]any {
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil || len(state) == 0 || guestWithoutFreeTier(state) {
		return nousSnapshotPoolStatus(env)
	}
	base := nousStatusFromState(state, truthyAny(state["access_token"]), "auth_store")
	background := env
	background.Ctx = withBackgroundCaller(env.ctx())
	creds, err := resolveNousRuntimeCredentialsWith(background, NousResolveOptions{})
	if err != nil {
		authErr := nousAuthErrorOf(err)
		base["logged_in"] = false
		base["error"] = authErr.Error()
		base["relogin_required"] = authErr.ReloginRequired
		base["error_code"] = nilIfEmpty(authErr.Code)
		return base
	}
	refreshed, loadErr := env.Store.GetProviderAuthState("nous")
	if loadErr != nil || len(refreshed) == 0 {
		refreshed = state
	}
	base["logged_in"] = true
	base["portal_base_url"] = nousOr(refreshed["portal_base_url"], base["portal_base_url"])
	base["inference_base_url"] = nousOr(creds["base_url"], nousOr(refreshed["inference_base_url"], base["inference_base_url"]))
	base["access_expires_at"] = nousOr(refreshed["expires_at"], base["access_expires_at"])
	base["agent_key_expires_at"] = nousOr(creds["expires_at"], nousOr(refreshed["agent_key_expires_at"], base["agent_key_expires_at"]))
	base["has_refresh_token"] = truthyAny(refreshed["refresh_token"])
	base["inference_credential_present"] = true
	base["credential_source"] = "auth_store"
	base["source"] = "runtime:" + anyString(nousOr(creds["source"], "portal"))
	base["key_id"] = creds["key_id"]
	return base
}

// The persisted last_auth_error when it is a terminal quarantine with no credential left
func nousTerminalQuarantineMarker(state map[string]any) map[string]any {
	lastErr, ok := state["last_auth_error"].(map[string]any)
	if ok && truthyAny(lastErr["relogin_required"]) && !(truthyAny(state["access_token"]) || truthyAny(state["refresh_token"])) {
		return lastErr
	}
	return nil
}

// GetNousAuthStatusLocal is the refresh free Nous snapshot for read only display surfaces
func GetNousAuthStatusLocal(env PoolEnv) map[string]any {
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil {
		state = nil
	}
	if len(state) == 0 || guestWithoutFreeTier(state) {
		return nousSnapshotPoolStatus(env)
	}
	jwtReason := nousStateInvokeJWTStatus(state, state["access_token"])
	lastErr := nousTerminalQuarantineMarker(state)
	loggedIn := jwtReason == "" || (truthyAny(state["refresh_token"]) && lastErr == nil)
	status := nousStatusFromState(state, loggedIn, "auth_store_local")
	if lastErr != nil {
		status["relogin_required"] = true
		status["error_code"] = lastErr["code"]
		status["error"] = nousOr(lastErr["message"], "re-login required")
	}
	return status
}

// Values the dashboard status probe reports as nous_session_valid
const (
	NousSessionValid    = "valid"
	NousSessionTerminal = "terminal"
	NousSessionUnknown  = "unknown"
)

// GetNousSessionValidity classifies the bootstrap session from local state only, terminal solely on a quarantine marker
func GetNousSessionValidity(env PoolEnv) string {
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil || len(state) == 0 {
		return NousSessionUnknown
	}
	if nousTerminalQuarantineMarker(state) != nil {
		return NousSessionTerminal
	}
	if nousStateInvokeJWTStatus(state, state["access_token"]) == "" {
		return NousSessionValid
	}
	return NousSessionUnknown
}

// Hostname of the Portal URL for failure copy, the production host when unparseable
func nousPortalHost(portalURL string) string {
	if portalURL == "" {
		portalURL = DefaultNousPortalURL
	}
	if parsed, err := url.Parse(portalURL); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return "portal.nousresearch.com"
}
