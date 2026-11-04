package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const nousUnusableJWTRelogin = "Re-authenticate with: icebath auth add nous"

// Relogin error for an access token that is not a usable inference JWT, terminal when no refresh token remains
func nousUnusableInvokeJWTError(reason string, noRefreshToken bool) *AuthError {
	detail := ""
	code := reason
	if noRefreshToken {
		detail = " and no refresh token is available"
		code = "nous_auth_missing_refresh_token"
	}
	return nousErr(fmt.Sprintf("Nous Portal access token is not a usable inference JWT (%s)%s. %s", reason, detail, nousUnusableJWTRelogin), code, true)
}

// Short sha256 prefix of a token for telemetry, empty for a blank token
func nousTokenFingerprint(token any) string {
	cleaned := strings.TrimSpace(anyString(token))
	if cleaned == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cleaned))
	return hex.EncodeToString(sum[:])[:12]
}

var nousOAuthTraceEnabled = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_OAUTH_TRACE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Emits one sorted JSON oauth_trace info line when ICEBATH_OAUTH_TRACE is on
func nousOAuthTrace(env PoolEnv, event, sequenceID string, fields map[string]any) {
	if !nousOAuthTraceEnabled() {
		return
	}
	payload := map[string]any{"event": event}
	if sequenceID != "" {
		payload["sequence_id"] = sequenceID
	}
	for k, v := range fields {
		payload[k] = v
	}
	raw, _ := json.Marshal(payload)
	env.log().Info("oauth_trace " + string(raw))
}

// Python isoformat of a UTC instant, microseconds only when they are not zero
func nousISO(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

// Python isoformat of epoch seconds in UTC
func nousEpochISO(epoch float64) string {
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return nousISO(time.Unix(sec, nsec))
}

// ISO timestamp ttl seconds after now in UTC
func nousISOAfter(now time.Time, ttl int) string {
	return nousISO(now.Add(time.Duration(ttl) * time.Second))
}

// True for a non blank string value
func nousNonempty(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// Nous agent key slots, a fresh login persists them as nil and quarantine strips them
var nousEmptyAgentKeyFields = map[string]any{
	"agent_key": nil, "agent_key_id": nil, "agent_key_expires_at": nil,
	"agent_key_expires_in": nil, "agent_key_reused": nil, "agent_key_obtained_at": nil,
}

// Pool env over the process home for the contract entry points that carry no env
func nousProcessEnv() PoolEnv {
	env := modelsPoolEnv(context.Background())
	env.Log = nil
	return env
}

// Portal entitlement notice for the capability from fresh account data, empty when unavailable
func nousPortalEntitlementMessageFor(env PoolEnv, capability string) string {
	message, err := nousPortalEntitlementMessage(env, capability)
	if err != nil {
		return ""
	}
	return message
}

func formatNousEntitlementAuthError(err *AuthError) string {
	if message := nousPortalEntitlementMessageFor(nousProcessEnv(), "Nous model access"); message != "" {
		return message
	}
	return err.Error() + " Check credits or billing in Nous Portal, then retry."
}

// Hosts the Portal proxy may forward inference JWTs to, the env override bypasses this list
var nousAllowedInferenceHosts = map[string]bool{"inference-api.nousresearch.com": true, "welcome-api.nousresearch.com": true}

// Portal hosts a persisted portal_base_url may name
var nousPortalAllowedHosts = map[string]bool{"portal.nousresearch.com": true, "localhost": true, "127.0.0.1": true}

// Production hosts always, otherwise only the host the operator named in NOUS_INFERENCE_BASE_URL
func nousInferenceHostAllowed(env PoolEnv, hostname string) bool {
	if nousAllowedInferenceHosts[hostname] {
		return true
	}
	if hostname == "" {
		return false
	}
	override := nousInferenceEnvOverrideIn(env)
	if override == "" {
		return false
	}
	parsed, err := url.Parse(override)
	return err == nil && parsed.Hostname() == hostname
}

// Validates a Portal returned inference URL against the host allowlist, empty when refused
func validateNousInferenceURLFromNetworkIn(env PoolEnv, raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	parsed, err := url.Parse(cleaned)
	if err != nil {
		return ""
	}
	if parsed.Scheme != "https" {
		env.log().Warn(fmt.Sprintf("nous: refusing non-https inference URL scheme %q from Portal response", parsed.Scheme))
		return ""
	}
	if !nousInferenceHostAllowed(env, parsed.Hostname()) {
		env.log().Warn(fmt.Sprintf("nous: refusing inference URL host %q from Portal response (not in allowlist); falling back to default", parsed.Hostname()))
		return ""
	}
	return strings.TrimRight(cleaned, "/")
}

func validateNousInferenceURLFromNetwork(raw string) string {
	return validateNousInferenceURLFromNetworkIn(nousProcessEnv(), raw)
}

// First set operator routing override among names through the profile secret scope, absent when the scope is lost
func nousScopedOperatorOverride(env PoolEnv, names ...string) string {
	for _, name := range names {
		value, _, err := env.scope().Secret(name)
		if err != nil {
			var unscoped *UnscopedSecretError
			if errors.As(err, &unscoped) {
				env.log().Warn(fmt.Sprintf("nous: %s unreadable — no profile secret scope on a multiplexed call; treating the override as absent (default routing applies). The caller needs a profile scope binding.", strings.Join(names, "/")))
				return ""
			}
			return ""
		}
		if value != "" {
			return value
		}
	}
	return ""
}

// Operator NOUS_INFERENCE_BASE_URL override with the trailing slash stripped, never allowlist gated
func nousInferenceEnvOverrideIn(env PoolEnv) string {
	return optionalBaseURLValue(nousScopedOperatorOverride(env, "NOUS_INFERENCE_BASE_URL"))
}

func nousInferenceEnvOverride() string {
	return nousInferenceEnvOverrideIn(nousProcessEnv())
}

// Operator ICEBATH_PORTAL_BASE_URL or NOUS_PORTAL_BASE_URL override, trusted so never allowlist gated
func nousPortalEnvOverride(env PoolEnv) string {
	return optionalBaseURLValue(nousScopedOperatorOverride(env, "ICEBATH_PORTAL_BASE_URL", "NOUS_PORTAL_BASE_URL"))
}

// Portal base URL, the operator override outright else the stored or default value gated by the host allowlist
func nousPortalBaseURLIn(env PoolEnv, state map[string]any) string {
	if override := nousPortalEnvOverride(env); override != "" {
		return strings.TrimRight(override, "/")
	}
	portal := optionalBaseURL(state["portal_base_url"])
	if portal == "" {
		portal = DefaultNousPortalURL
	}
	portal = strings.TrimRight(portal, "/")
	host := ""
	if parsed, err := url.Parse(portal); err == nil {
		host = parsed.Hostname()
	}
	if host != "" && !nousPortalAllowedHosts[host] {
		env.log().Warn(fmt.Sprintf("auth: ignoring invalid portal_base_url %q (host %q not in allowlist), using default", portal, host))
		return DefaultNousPortalURL
	}
	return portal
}

func nousPortalBaseURL(state map[string]any) string {
	return nousPortalBaseURLIn(nousProcessEnv(), state)
}

// Invoke JWT refusal reason for the token using the state's scope and expiry, empty when usable
func nousStateInvokeJWTStatus(state map[string]any, token any) string {
	return NousInvokeJWTStatus(anyString(token), state["scope"], state["expires_at"], NousInvokeJWTMinTTL)
}

// Error when the state's access token, or the given one, is not a usable invoke JWT
func nousAssertInferenceJWTUsable(state map[string]any, accessToken any) error {
	token := accessToken
	if token == nil {
		token = state["access_token"]
	}
	if reason := nousStateInvokeJWTStatus(state, token); reason != "" {
		return nousUnusableInvokeJWTError(reason, false)
	}
	return nil
}

// Seconds until the ISO expiry, else the fallback expires_in coerced to a TTL
func nousRemainingTTL(expiresAt any, fallbackExpiresIn any) int {
	if epoch, ok := ParseISOTimestamp(expiresAt); ok {
		remaining := int(epoch - nowEpoch())
		if remaining < 0 {
			return 0
		}
		return remaining
	}
	return coerceTTLSeconds(fallbackExpiresIn)
}

// ISO expiry from the JWT exp claim, else the fallback when it is a string
func nousJWTExpiresAt(token any, fallbackExpiresAt any) string {
	claims := DecodeJWTClaims(anyString(token))
	if exp, ok := anyFloat(claims["exp"]); ok {
		return nousEpochISO(exp)
	}
	s, _ := fallbackExpiresAt.(string)
	return s
}

// Copies the access token into the agent key slots with its JWT derived expiry
func nousSetAgentKeyFromInvokeJWT(state map[string]any, obtainedAt string) {
	accessToken := state["access_token"]
	if !nousNonempty(accessToken) {
		return
	}
	existingObtainedAt := state["agent_key_obtained_at"]
	if obtainedAt == "" {
		if anyString(state["agent_key"]) == anyString(accessToken) && nousNonempty(existingObtainedAt) {
			obtainedAt = anyString(existingObtainedAt)
		} else {
			obtainedAt = nousISO(nowFunc())
		}
	}
	expiresAt := nousJWTExpiresAt(accessToken, state["expires_at"])
	expiresIn := nousRemainingTTL(expiresAt, state["expires_in"])
	if expiresAt != "" {
		state["expires_at"] = expiresAt
		state["expires_in"] = expiresIn
	}
	var expiresAtAny any
	if expiresAt != "" {
		expiresAtAny = expiresAt
	}
	state["agent_key"] = accessToken
	state["agent_key_id"] = nil
	state["agent_key_expires_at"] = expiresAtAny
	state["agent_key_expires_in"] = expiresIn
	state["agent_key_reused"] = false
	state["agent_key_obtained_at"] = obtainedAt
}

// Selects the NAS invoke JWT as the inference credential, replacing the access token when one is given
func nousSelectInvokeJWT(env PoolEnv, state map[string]any, accessToken any, sequenceID string) {
	if nousNonempty(accessToken) {
		state["access_token"] = accessToken
	}
	nousSetAgentKeyFromInvokeJWT(state, "")
	env.log().Debug("Nous inference auth: using NAS invoke JWT")
	nousOAuthTrace(env, "nous_invoke_jwt_selected", sequenceID, map[string]any{"access_token_fp": nilIfEmpty(nousTokenFingerprint(state["access_token"]))})
}

// Derived countdown keys whose change alone never warrants an auth.json write
var nousEffectiveStateIgnoredKeys = map[string]bool{"expires_in": true, "agent_key_expires_in": true}

func nousEffectiveProviderState(state map[string]any) map[string]any {
	out := make(map[string]any, len(state))
	for k, v := range state {
		if !nousEffectiveStateIgnoredKeys[k] {
			out[k] = v
		}
	}
	return out
}

// JSON level equality of two provider states
func nousStatesEqual(a, b map[string]any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}

// ---- shared cross profile store ----

const NousSharedStoreFilename = "nous_auth.json"

// Directory of the shared Nous token store, ICEBATH_SHARED_AUTH_DIR or root/shared outside any named profile
func nousSharedAuthDir(env PoolEnv) string {
	if override := strings.TrimSpace(os.Getenv("ICEBATH_SHARED_AUTH_DIR")); override != "" {
		if strings.HasPrefix(override, "~") {
			if userHome, err := os.UserHomeDir(); err == nil {
				override = userHome + override[1:]
			}
		}
		return override
	}
	return filepath.Join(home.RootOf(envHome(env)), "shared")
}

func nousSharedStorePath(env PoolEnv) string {
	return filepath.Join(nousSharedAuthDir(env), NousSharedStoreFilename)
}

// Store the shared lock is taken through, the process home when the env carries none
func nousSharedLockStore(env PoolEnv) *Store {
	if env.Store != nil {
		return env.Store
	}
	return NewStore(home.Process())
}

// Cross profile lock for the shared Nous store, taken after the auth store lock whenever both are held
func nousSharedStoreLock(env PoolEnv, timeout time.Duration) (func(), error) {
	path := nousSharedStorePath(env)
	unlock, err := nousSharedLockStore(env).fileLock(path, timeout)
	if err != nil {
		var timedOut *LockTimeoutError
		if errors.As(err, &timedOut) {
			lockPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".lock"
			return nil, &LockTimeoutError{LockPath: lockPath, Hint: "Timed out waiting for shared Nous auth lock (" + lockPath + ")"}
		}
		return nil, err
	}
	return unlock, nil
}

func nousSharedLockTimeout(timeout time.Duration) time.Duration {
	if timeout+5*time.Second > AuthLockTimeout {
		return timeout + 5*time.Second
	}
	return AuthLockTimeout
}

// OAuth fields mirrored between a profile's Nous state and the shared cross profile store
var nousSharedStateKeys = []string{
	"access_token", "refresh_token", "token_type", "scope", "client_id", "portal_base_url",
	"inference_base_url", "obtained_at", "expires_at",
	"auth_method", "account_tier", "anon_token", "user_id", "org_id",
}

const nousSharedRenewalKey = "renewal"

// JWT iat, local issue stamp and expiry epoch seconds of an OAuth pair
func oauthStamps(entry map[string]any) (iat, local, expires float64, hasIat, hasLocal, hasExpires bool) {
	claims := DecodeJWTClaims(anyString(entry["access_token"]))
	first := func(values ...any) (float64, bool) {
		for _, v := range values {
			if ts, ok := ParseAbsoluteTimestamp(v); ok && ts != 0 {
				return ts, true
			}
		}
		return 0, false
	}
	iat, hasIat = first(claims["iat"])
	local, hasLocal = first(entry["last_refresh"], entry["obtained_at"])
	expires, hasExpires = first(claims["exp"], entry["expires_at_ms"], entry["expires_at"])
	return
}

// Order of two stamps when both are known
func oauthStampSign(x float64, hasX bool, y float64, hasY bool) (int, bool) {
	if !hasX || !hasY {
		return 0, false
	}
	switch {
	case x > y:
		return 1, true
	case x < y:
		return -1, true
	}
	return 0, true
}

// 1 when a holds the later issued OAuth pair than b, -1 the earlier, 0 tied, false when neither is stamped
func oauthPairOrder(a, b map[string]any) (int, bool) {
	ia, la, ea, hasIa, hasLa, hasEa := oauthStamps(a)
	ib, lb, eb, hasIb, hasLb, hasEb := oauthStamps(b)
	if hasIa && hasIb {
		if s, _ := oauthStampSign(ia, true, ib, true); s != 0 {
			return s, true
		}
		if s, ok := oauthStampSign(la, hasLa, lb, hasLb); ok && s != 0 {
			return s, true
		}
		return 0, true
	}
	issuedA, hasIssuedA := ia, hasIa
	if !hasIssuedA {
		issuedA, hasIssuedA = la, hasLa
	}
	issuedB, hasIssuedB := ib, hasIb
	if !hasIssuedB {
		issuedB, hasIssuedB = lb, hasLb
	}
	if s, ok := oauthStampSign(issuedA, hasIssuedA, issuedB, hasIssuedB); ok {
		return s, true
	}
	if s, ok := oauthStampSign(ea, hasEa, eb, hasEb); ok && s != 0 {
		return s, true
	}
	aAny := hasIa || hasLa || hasEa
	bAny := hasIb || hasLb || hasEb
	if !aAny && !bAny {
		return 0, false
	}
	if aAny == bAny {
		return 0, true
	}
	if aAny {
		return 1, true
	}
	return -1, true
}

// Issue order of two Nous states, false when neither carries a stamp
func nousStateOrder(a, b map[string]any) (int, bool) {
	return oauthPairOrder(a, b)
}

type nousIdentityKey struct{ kind, id string }

// Who a Nous state is, the guest's anon credential else the JWT subject which survives a refresh
func nousIdentity(state map[string]any) nousIdentityKey {
	if isGuestState(state) {
		return nousIdentityKey{kind: "guest", id: anyString(state["anon_token"])}
	}
	return nousIdentityKey{kind: "account", id: anyString(DecodeJWTClaims(anyString(state["access_token"]))["sub"])}
}

// True when both states are provably the same identity, an unknown identity is never the same
func nousSameKnownIdentity(a, b map[string]any) bool {
	identity := nousIdentity(a)
	return identity.id != "" && identity == nousIdentity(b)
}

// Adopts the shared store's OAuth tokens into a profile local state when they are newer, caller holds the shared lock
func mergeSharedNousOAuthState(env PoolEnv, state map[string]any) bool {
	shared, _ := readSharedNousState(env)
	if shared == nil {
		shared = map[string]any{}
	}
	if renewed, ok := shared[nousSharedRenewalKey].(map[string]any); ok && nousIdentity(renewed) == nousIdentity(state) {
		shared = renewed
	}
	sharedRefresh := shared["refresh_token"]
	if !nousNonempty(sharedRefresh) {
		return false
	}
	refreshChanged := strings.TrimSpace(anyString(sharedRefresh)) != strings.TrimSpace(anyString(state["refresh_token"]))
	order, ordered := nousStateOrder(shared, state)
	knownSame := nousSameKnownIdentity(shared, state)
	same := knownSame || !nousNonempty(state["access_token"])
	if !(same && ((ordered && order == 1) || (!ordered && refreshChanged))) {
		if ordered && order == -1 && refreshChanged && knownSame {
			nousOAuthTrace(env, "nous_shared_store_older_than_profile", "", map[string]any{
				"shared_refresh_token_fp": nilIfEmpty(nousTokenFingerprint(sharedRefresh)),
				"refresh_token_fp":        nilIfEmpty(nousTokenFingerprint(state["refresh_token"]))})
			writeSharedNousState(env, state, true)
		}
		return false
	}
	for _, key := range nousSharedStateKeys {
		value := shared[key]
		if value == nil {
			continue
		}
		if s, ok := value.(string); ok && s == "" {
			continue
		}
		state[key] = value
	}
	return true
}

// Value or the default when the value is nil or an empty string
func nousOr(value any, def any) any {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok && s == "" {
		return def
	}
	if b, ok := value.(bool); ok && !b {
		return def
	}
	return value
}

// The defaulted OAuth core shared across profiles, tokens plus routing plus expiry plus guest identity
func nousSharedShape(src map[string]any) map[string]any {
	defaultInference := DefaultNousInferenceURL
	if anyString(src["auth_method"]) == AnonAccountTier {
		defaultInference = DefaultNousWelcomeURL
	}
	out := map[string]any{
		"access_token": src["access_token"], "refresh_token": src["refresh_token"],
		"token_type":         nousOr(src["token_type"], "Bearer"),
		"scope":              nousOr(src["scope"], DefaultNousScope),
		"client_id":          nousOr(src["client_id"], DefaultNousClientID),
		"portal_base_url":    nousOr(src["portal_base_url"], DefaultNousPortalURL),
		"inference_base_url": nousOr(src["inference_base_url"], defaultInference),
		"obtained_at":        src["obtained_at"], "expires_at": src["expires_at"],
	}
	for _, k := range []string{"auth_method", "account_tier", "anon_token", "user_id", "org_id", "connectors_token", WelcomeClaimFlag} {
		v, present := src[k]
		if !present || v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// True when the shared store already holds a newer copy of the state's account
func nousSharedCopyIsNewer(held map[string]any, state map[string]any, same bool) bool {
	if held == nil || isGuestState(state) {
		return false
	}
	copyState := held
	if !same {
		copyState, _ = held[nousSharedRenewalKey].(map[string]any)
	}
	if copyState == nil || !nousSameKnownIdentity(copyState, state) {
		return false
	}
	order, ordered := nousStateOrder(copyState, state)
	return ordered && order == 1
}

// Persists a minimal copy of the Nous OAuth state to the shared store, a renewal of another identity kept beside it
func writeSharedNousState(env PoolEnv, state map[string]any, renewal bool) {
	refreshToken := state["refresh_token"]
	isGuest := nousNonempty(state["anon_token"])
	if !isGuest && !(nousNonempty(refreshToken) && nousNonempty(state["access_token"])) {
		return
	}
	shared := nousSharedShape(state)
	shared["_schema"] = 1
	shared["updated_at"] = nousISO(nowFunc())
	path := nousSharedStorePath(env)
	err := func() error {
		unlock, err := nousSharedStoreLock(env, AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		held, _ := readSharedNousState(env)
		same := held != nil && nousIdentity(held) == nousIdentity(state)
		if renewal && nousSharedCopyIsNewer(held, state, same) {
			nousOAuthTrace(env, "nous_shared_store_write_skipped_older", "", map[string]any{"path": path, "refresh_token_fp": nilIfEmpty(nousTokenFingerprint(refreshToken))})
			return nil
		}
		if renewal && held != nil && !same {
			merged := cloneMap(held)
			merged[nousSharedRenewalKey] = nousSharedShape(state)
			shared = merged
		} else {
			guestHandoffNoteReplacement(env, held, state)
			if same {
				if renewed, present := held[nousSharedRenewalKey]; present {
					shared[nousSharedRenewalKey] = renewed
				}
			}
		}
		return savePrivateJSON(path, shared)
	}()
	if err != nil {
		env.log().Debug(fmt.Sprintf("Failed to write shared Nous auth store: %v", err))
		return
	}
	nousOAuthTrace(env, "nous_shared_store_written", "", map[string]any{"path": path, "refresh_token_fp": nilIfEmpty(nousTokenFingerprint(refreshToken))})
}

// Shared Nous OAuth state when present and well formed with tokens, else nil
func readSharedNousState(env PoolEnv) (map[string]any, bool) {
	path := nousSharedStorePath(env)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Shared Nous auth store at %s is unreadable: %v", path, err))
		return nil, false
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		env.log().Debug(fmt.Sprintf("Shared Nous auth store at %s is unreadable: %v", path, err))
		return nil, false
	}
	obj, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	hasTokens := nousNonempty(obj["anon_token"]) || (nousNonempty(obj["access_token"]) && nousNonempty(obj["refresh_token"]))
	if !hasTokens {
		return nil, false
	}
	return obj, true
}

// Removes the shared Nous OAuth store after a terminal token failure
func clearSharedNousState(env PoolEnv, reason string) {
	err := func() error {
		unlock, err := nousSharedStoreLock(env, AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		if err := os.Remove(nousSharedStorePath(env)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}()
	if err != nil {
		env.log().Debug(fmt.Sprintf("Failed to clear shared Nous auth store: %v", err))
		return
	}
	nousOAuthTrace(env, "nous_shared_store_cleared", "", map[string]any{"reason": reason})
}

// Redaction safe forensic record for a quarantine, fingerprints sizes and booleans only
func nousQuarantineForensics(env PoolEnv, state map[string]any, err *AuthError, reason string) map[string]any {
	forensic := map[string]any{
		"reason": reason, "error_code": nilIfEmpty(err.Code), "client_id": state["client_id"],
		"agent_key_id": state["agent_key_id"], "refresh_token_fp": nilIfEmpty(nousTokenFingerprint(state["refresh_token"])),
	}
	authPath := home.AuthPath(envHome(env))
	forensic["auth_json_path"] = authPath
	if st, statErr := os.Stat(authPath); statErr == nil {
		forensic["auth_json_size"] = st.Size()
		forensic["auth_json_mtime"] = float64(st.ModTime().UnixNano()) / 1e9
		forensic["auth_json_exists"] = true
	} else if errors.Is(statErr, os.ErrNotExist) {
		forensic["auth_json_exists"] = false
	} else {
		forensic["auth_json_stat_error"] = statErr.Error()
	}
	var alreadyExpired any
	if raw, ok := state["expires_at"].(string); ok && raw != "" {
		if parsed, ok := parseISO(raw); ok {
			alreadyExpired = parsed.Before(nowFunc())
		}
	}
	forensic["token_already_expired"] = alreadyExpired
	return forensic
}

// The structured auth error behind any error, a code free Nous error for a plain one
func nousAuthErrorOf(err error) *AuthError {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return authErr
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	return &AuthError{Message: message, Provider: "nous", Err: err}
}

// Keeps routing metadata but removes dead OAuth material so it is never replayed
func quarantineNousOAuthStateIn(env PoolEnv, state map[string]any, cause error, reason string) {
	authErr := nousAuthErrorOf(cause)
	raw, _ := json.Marshal(nousQuarantineForensics(env, state, authErr, reason))
	env.log().Warn("Nous OAuth state quarantined (terminal auth death): " + string(raw))
	for _, key := range flatOAuthTokenKeys {
		delete(state, key)
	}
	for key := range nousEmptyAgentKeyFields {
		delete(state, key)
	}
	state["last_auth_error"] = lastAuthErrorMarker("nous", authErr, reason, "")
	clearSharedNousState(env, reason)
	InvalidateNousAuthStatusCache()
}

func quarantineNousOAuthState(state map[string]any, cause error, reason string) {
	quarantineNousOAuthStateIn(nousProcessEnv(), state, cause, reason)
}

// The credential_pool.nous rows of a store document, false when the list is absent
func nousPoolEntriesIn(data AuthData) ([]map[string]any, bool) {
	pool, _ := data["credential_pool"].(map[string]any)
	if pool == nil {
		return nil, false
	}
	list, ok := pool["nous"].([]any)
	if !ok {
		return nil, false
	}
	return poolEntries(list), true
}

// Removes singleton seeded Nous pool rows that contain dead OAuth state, true when any were dropped
func quarantineNousPoolEntriesIn(env PoolEnv, data AuthData, cause error, reason string) bool {
	pool, _ := data["credential_pool"].(map[string]any)
	if pool == nil {
		return false
	}
	list, ok := pool["nous"].([]any)
	if !ok {
		return false
	}
	retained := make([]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			source := anyString(row["source"])
			if source == NousDeviceCodeSource || source == "manual:"+NousDeviceCodeSource {
				continue
			}
		}
		retained = append(retained, item)
	}
	removed := len(retained) != len(list)
	if removed {
		pool["nous"] = retained
		nousOAuthTrace(env, "nous_pool_device_code_quarantined", "", map[string]any{"reason": reason, "error_code": nilIfEmpty(nousAuthErrorOf(cause).Code)})
	}
	return removed
}

func quarantineNousPoolEntries(data AuthData, cause error, reason string) bool {
	return quarantineNousPoolEntriesIn(nousProcessEnv(), data, cause, reason)
}

// Rehydrates Nous OAuth state from the shared store through a forced refresh, false on any failure
func tryImportSharedNousState(env PoolEnv, timeout time.Duration) (map[string]any, bool) {
	var refreshed map[string]any
	err := func() error {
		unlock, err := nousSharedStoreLock(env, nousSharedLockTimeout(timeout))
		if err != nil {
			return err
		}
		defer unlock()
		shared, ok := readSharedNousState(env)
		if !ok {
			return errNoSharedNousState
		}
		state := nousSharedShape(shared)
		state["agent_key"] = nil
		state["agent_key_expires_at"] = nil
		state["tls"] = map[string]any{"insecure": false, "ca_bundle": nil}
		refreshed, err = refreshNousOAuthFromStateFn(env, state, timeout, true, func(updated map[string]any, _ string) {
			writeSharedNousState(env, updated, true)
		})
		if err != nil {
			return err
		}
		writeSharedNousState(env, refreshed, true)
		return nil
	}()
	if err == nil {
		return refreshed, true
	}
	if errors.Is(err, errNoSharedNousState) {
		return nil, false
	}
	var authErr *AuthError
	isAuth := errors.As(err, &authErr)
	fields := map[string]any{"error_type": fmt.Sprintf("%T", err)}
	if isAuth {
		fields["error_type"] = "AuthError"
		fields["error_code"] = nilIfEmpty(authErr.Code)
	}
	nousOAuthTrace(env, "nous_shared_import_failed", "", fields)
	if isAuth && isTerminalNousRefreshError(err) {
		clearSharedNousState(env, "shared_import_terminal_refresh_failure")
	}
	env.log().Debug(fmt.Sprintf("Shared Nous import failed: %v", err))
	return nil, false
}

var errNoSharedNousState = errors.New("no shared nous state")

// ---- Portal refresh ----

// Redeems a refresh token at the Portal token endpoint, grant death terminal and edge refusals retryable
func nousRefreshAccessToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
	resp, err := oauthPostForm(ctx, client, portalBaseURL+"/api/oauth/token",
		url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}},
		map[string]string{"x-nous-refresh-token": refreshToken})
	if err != nil {
		return nil, err
	}
	if resp.Status == 200 {
		payload, err := resp.JSONObject()
		if err != nil {
			return nil, err
		}
		if _, ok := payload["access_token"]; !ok {
			return nil, nousErr("Refresh response missing access_token", "invalid_token", true)
		}
		return payload, nil
	}
	retryable := true
	if resp.Status >= 500 && resp.Status <= 599 {
		return nil, &AuthError{Message: fmt.Sprintf("Nous Portal is temporarily unavailable (HTTP %d).", resp.Status),
			Provider: "nous", Code: "temporarily_unavailable", Retryable: &retryable}
	}
	if resp.Status == 403 || resp.Status == 429 {
		if mitigated := resp.Header.Get("x-vercel-mitigated"); mitigated != "" {
			authErr := &AuthError{
				Message:  fmt.Sprintf("Nous Portal's edge firewall challenged the token refresh (HTTP %d, x-vercel-mitigated=%s). Credentials kept; try again shortly.", resp.Status, mitigated),
				Provider: "nous", Code: "upstream_blocked", Retryable: &retryable}
			if seconds, ok := parseRetryAfterSeconds(resp.Header); ok {
				authErr.RetryAfter = time.Duration(seconds) * time.Second
				authErr.HasRetryAfter = true
			}
			return nil, authErr
		}
	}
	errorPayload := map[string]any{}
	if payload, err := resp.JSON(); err == nil {
		if obj, ok := payload.(map[string]any); ok {
			errorPayload = obj
		}
	}
	rawCode, hasCode := errorPayload["error"]
	if (!hasCode || rawCode == nil) && (resp.Status == 401 || resp.Status == 403) {
		rawCode, hasCode = "invalid_grant", true
	}
	code := ""
	if hasCode && rawCode != nil {
		code = stringifyAny(rawCode)
	}
	description := stringifyAny(errorPayload["error_description"])
	if !nousNonempty(errorPayload["error_description"]) {
		description = "Refresh token exchange failed"
	}
	relogin := code != "" && oauthGrantDeadCodes[code]
	if code == "refresh_token_reused" || strings.Contains(strings.ToLower(description), "reuse") {
		description = "Nous Portal detected refresh-token reuse and revoked this session.\n" +
			"This usually means an external process (monitoring script, " +
			"custom self-heal hook, or another Icebath install sharing " +
			"~/.icebath/auth.json) called POST /api/oauth/token with Icebath's " +
			"refresh token without persisting the rotated token back.\n" +
			"Nous refresh tokens are single-use — only Icebath may call the " +
			"refresh endpoint. For health checks, use `icebath auth status` " +
			"instead.\n" +
			"Re-authenticate with: icebath auth add nous"
		relogin = true
	}
	return nil, nousErr(description, code, relogin)
}

// Portal refresh POST the runtime paths call, swappable so tests can stand in for the Portal
var nousRefreshAccessTokenFn = nousRefreshAccessToken

// OAuth client builder the Nous paths use, swappable so tests can script the transport
var nousHTTPClientFn = oauthHTTPClient

// State refresh the shared import runs, swappable so tests can stand in for the Portal
var refreshNousOAuthFromStateFn = refreshNousOAuthFromState

// Redeems the refresh token, on terminal failure quarantines state plus pool rows, persists and returns the error
func refreshNousOrQuarantine(env PoolEnv, client *http.Client, tx *ProviderTxn, state map[string]any, portalBaseURL, clientID, refreshToken, reason string, persist func() error) (map[string]any, error) {
	refreshed, err := nousRefreshAccessTokenFn(env.ctx(), client, portalBaseURL, clientID, refreshToken)
	if err == nil {
		return refreshed, nil
	}
	var authErr *AuthError
	if errors.As(err, &authErr) && isTerminalNousRefreshError(err) {
		quarantineNousOAuthStateIn(env, state, authErr, reason)
		if tx != nil {
			quarantineNousPoolEntriesIn(env, tx.Data, authErr, reason)
		}
		if persistErr := persist(); persistErr != nil {
			return nil, persistErr
		}
	}
	return nil, err
}

// Writes a successful Portal refresh payload into the state, the healed inference URL persisted when given
func applyNousRefreshedTokens(state, refreshed map[string]any, refreshToken, inferenceBaseURL string) {
	now := nowFunc()
	accessTTL := coerceTTLSeconds(refreshed["expires_in"])
	state["access_token"] = refreshed["access_token"]
	state["refresh_token"] = nousOr(refreshed["refresh_token"], refreshToken)
	state["token_type"] = nousOr(refreshed["token_type"], nousOr(state["token_type"], "Bearer"))
	state["scope"] = nousOr(refreshed["scope"], state["scope"])
	if inferenceBaseURL != "" {
		state["inference_base_url"] = inferenceBaseURL
	}
	state["obtained_at"] = nousISO(now)
	state["expires_in"] = accessTTL
	state["expires_at"] = nousISOAfter(now, accessTTL)
}

// Validated network provenance inference URL from a refresh payload, healed to the production default
func nousHealedInferenceURL(env PoolEnv, refreshed map[string]any) string {
	if u := validateNousInferenceURLFromNetworkIn(env, anyString(refreshed["inference_base_url"])); u != "" {
		return u
	}
	return DefaultNousInferenceURL
}

// Sort rank for model ids, opus then pro then haiku or flash then sonnet
func nousModelPriority(id string) int {
	low := strings.ToLower(id)
	switch {
	case strings.Contains(low, "opus"):
		return 0
	case strings.Contains(low, "pro") && !strings.Contains(low, "sonnet"):
		return 1
	case strings.Contains(low, "sonnet"):
		return 3
	}
	return 2
}

// Available model ids from the Nous inference API, hermes models dropped and best models first
func fetchNousModelsWith(ctx context.Context, client *http.Client, apiKey, inferenceBaseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(inferenceBaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := oauthDo(client, req, 0, nil)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		description := fmt.Sprintf("/models request failed with status %d", resp.Status)
		if payload, err := resp.JSONObject(); err == nil {
			if d := stringifyAny(nousOr(payload["error_description"], payload["error"])); d != "" {
				description = d
			}
		}
		return nil, nousErr(description, "models_fetch_failed", false)
	}
	payload, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	data, ok := payload["data"].([]any)
	if !ok {
		return []string{}, nil
	}
	var ids []string
	for _, item := range data {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := row["id"]
		if nousNonempty(id) && !strings.Contains(strings.ToLower(anyString(id)), "hermes") {
			ids = append(ids, strings.TrimSpace(anyString(id)))
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		ri, rj := nousModelPriority(ids[i]), nousModelPriority(ids[j])
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// Live Nous model ids for the credential, empty on any failure
func fetchNousModels(ctx context.Context, apiKey, inferenceBaseURL string) []string {
	env := modelsPoolEnv(ctx)
	client := nousHTTPClientFn(env, 15*time.Second, defaultOAuthVerify())
	ids, err := fetchNousModelsWith(ctx, client, apiKey, inferenceBaseURL)
	if err != nil {
		env.log().Debug(fmt.Sprintf("nous: /models fetch failed: %v", err))
		return nil
	}
	return ids
}

// True when the stored agent key is a usable invoke JWT for at least the minimum TTL
func nousAgentKeyIsUsable(state map[string]any, minTTL time.Duration) bool {
	key := state["agent_key"]
	if !nousNonempty(key) {
		return false
	}
	if minTTL < 0 {
		minTTL = 0
	}
	return NousInvokeJWTStatus(anyString(key), state["scope"], state["agent_key_expires_at"], minTTL) == ""
}

// NousRefreshInput is the explicit token pair and routing for a refresh that never touches auth.json
type NousRefreshInput struct {
	AccessToken, RefreshToken, ClientID, PortalBaseURL, InferenceBaseURL string
	TokenType, Scope, ObtainedAt, ExpiresAt, AgentKey, AgentKeyExpiresAt string
	Timeout                                                              time.Duration
	Insecure                                                             *bool
	CABundle                                                             string
	ForceRefresh                                                         bool
	OnStateUpdate                                                        func(state map[string]any, reason string)
}

// Refreshes Nous OAuth state from explicit fields without mutating auth.json
func refreshNousOAuthPure(env PoolEnv, in NousRefreshInput) (map[string]any, error) {
	tokenType := in.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	scope := in.Scope
	if scope == "" {
		scope = DefaultNousScope
	}
	var insecure any
	if in.Insecure != nil {
		insecure = *in.Insecure
	}
	src := map[string]any{
		"access_token": in.AccessToken, "refresh_token": in.RefreshToken, "client_id": in.ClientID,
		"portal_base_url": in.PortalBaseURL, "inference_base_url": in.InferenceBaseURL,
		"token_type": tokenType, "scope": scope, "obtained_at": nilIfEmpty(in.ObtainedAt),
		"expires_at": nilIfEmpty(in.ExpiresAt), "agent_key": nilIfEmpty(in.AgentKey),
		"agent_key_expires_at": nilIfEmpty(in.AgentKeyExpiresAt),
		"tls":                  map[string]any{"insecure": insecure, "ca_bundle": nilIfEmpty(in.CABundle)},
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return refreshNousOAuthFromState(env, src, timeout, in.ForceRefresh, in.OnStateUpdate)
}

// Refreshes Nous OAuth from a state map with defaults filled in, without mutating auth.json
func refreshNousOAuthFromState(env PoolEnv, src map[string]any, timeout time.Duration, forceRefresh bool, onStateUpdate func(map[string]any, string)) (map[string]any, error) {
	tlsBlock, _ := src["tls"].(map[string]any)
	var insecurePtr *bool
	caBundle := ""
	if tlsBlock != nil {
		if v, present := tlsBlock["insecure"]; present && v != nil {
			b := truthyAny(v)
			insecurePtr = &b
		}
		caBundle = anyString(tlsBlock["ca_bundle"])
	}
	accessToken, _ := src["access_token"].(string)
	refreshToken, _ := src["refresh_token"].(string)
	state := map[string]any{
		"access_token": accessToken, "refresh_token": refreshToken,
		"client_id":          nousOr(src["client_id"], DefaultNousClientID),
		"portal_base_url":    strings.TrimRight(anyString(nousOr(src["portal_base_url"], DefaultNousPortalURL)), "/"),
		"inference_base_url": strings.TrimRight(anyString(nousOr(src["inference_base_url"], DefaultNousInferenceURL)), "/"),
		"token_type":         nousOr(src["token_type"], "Bearer"),
		"scope":              nousOr(src["scope"], DefaultNousScope),
		"obtained_at":        src["obtained_at"], "expires_at": src["expires_at"],
		"agent_key": src["agent_key"], "agent_key_expires_at": src["agent_key_expires_at"],
		"tls": map[string]any{"insecure": insecurePtr != nil && *insecurePtr, "ca_bundle": nilIfEmpty(caBundle)},
	}
	verify := resolveOAuthVerify(env, insecurePtr, caBundle, state)
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := nousHTTPClientFn(env, timeout, verify)
	currentStatus := nousStateInvokeJWTStatus(state, state["access_token"])
	if forceRefresh || currentStatus != "" {
		refreshTokenValue, _ := state["refresh_token"].(string)
		if refreshTokenValue == "" {
			if currentStatus != "" {
				return nil, nousUnusableInvokeJWTError(currentStatus, true)
			}
			return nil, nousErr("No refresh token is available for Nous Portal.", "nous_auth_missing_refresh_token", true)
		}
		refreshed, err := nousRefreshAccessTokenFn(env.ctx(), client, anyString(state["portal_base_url"]), anyString(state["client_id"]), refreshTokenValue)
		if err != nil {
			return nil, err
		}
		applyNousRefreshedTokens(state, refreshed, refreshTokenValue, nousHealedInferenceURL(env, refreshed))
		if onStateUpdate != nil {
			onStateUpdate(cloneMap(state), "post_refresh_access_token")
		}
	}
	if err := nousAssertInferenceJWTUsable(state, nil); err != nil {
		return nil, err
	}
	nousSelectInvokeJWT(env, state, nil, "")
	return state, nil
}

// Persists Nous OAuth credentials as the singleton, mirrors the shared store and reseeds the pool
func persistNousCredentials(env PoolEnv, creds map[string]any, label string) (PooledCredential, bool, error) {
	state := cloneMap(creds)
	if strings.TrimSpace(label) != "" {
		state["label"] = strings.TrimSpace(label)
	}
	if _, err := env.Store.SaveActiveProviderState("nous", state); err != nil {
		return PooledCredential{}, false, err
	}
	writeSharedNousState(env, state, false)
	pool := LoadPool(env, "nous")
	for _, entry := range pool.Entries() {
		if entry.Source == NousDeviceCodeSource {
			return entry, true, nil
		}
	}
	return PooledCredential{}, false, nil
}

// Reseeds the Nous pool after providers.nous changed, never failing the caller
func nousSyncPoolFromAuthStore(env PoolEnv) {
	defer func() {
		if r := recover(); r != nil {
			env.log().Debug(fmt.Sprintf("Failed to sync Nous credential pool from auth store: %v", r))
		}
	}()
	LoadPool(env, "nous")
}

// Portal URL, stored inference URL, effective inference URL with the env overlay, and client id from state
func nousEffectiveRouting(env PoolEnv, state map[string]any) (portalURL, storedInferenceURL, effectiveInferenceURL, clientID string) {
	portalURL = optionalBaseURL(state["portal_base_url"])
	if portalURL == "" {
		portalURL = DefaultNousPortalURL
	}
	portalURL = strings.TrimRight(portalURL, "/")
	if override := nousPortalEnvOverride(env); override != "" {
		portalURL = strings.TrimRight(override, "/")
	} else {
		host, scheme := "", ""
		if parsed, err := url.Parse(portalURL); err == nil {
			host, scheme = parsed.Hostname(), parsed.Scheme
		}
		trustedScheme := scheme == "https" || (scheme == "http" && (host == "localhost" || host == "127.0.0.1"))
		if host == "" || !nousPortalAllowedHosts[host] || !trustedScheme {
			env.log().Warn(fmt.Sprintf("auth: ignoring invalid portal_base_url %q (host %q or scheme not allowed), using default", portalURL, host))
			portalURL = DefaultNousPortalURL
		}
	}
	storedInferenceURL = validateNousInferenceURLFromNetworkIn(env, optionalBaseURL(state["inference_base_url"]))
	if storedInferenceURL == "" {
		if isGuestState(state) {
			storedInferenceURL = DefaultNousWelcomeURL
		} else {
			storedInferenceURL = DefaultNousInferenceURL
		}
	}
	effectiveInferenceURL = nousInferenceEnvOverrideIn(env)
	if effectiveInferenceURL == "" {
		effectiveInferenceURL = storedInferenceURL
	}
	clientID = anyString(nousOr(state["client_id"], DefaultNousClientID))
	return
}
