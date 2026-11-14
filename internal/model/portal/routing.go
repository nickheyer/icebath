package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const unusableJWTRelogin = "Re-authenticate with: icebath auth add portal"

var (
	allowedInferenceHosts = []string{"inference-api.icebath.app", "welcome-api.icebath.app"}
	allowedPortalHosts    = []string{"portal.icebath.app", "localhost", "127.0.0.1"}
	stalePortalHosts      = []string{"api.icebath.app"}
	emptyAgentKeyFields   = []string{"agent_key", "agent_key_id", "agent_key_expires_at", "agent_key_expires_in", "agent_key_reused", "agent_key_obtained_at"}
	flatOAuthTokenKeys    = []string{"access_token", "refresh_token", "expires_at", "expires_in", "obtained_at"}
	effectiveIgnoredKeys  = []string{"expires_in", "agent_key_expires_in"}
)

func unusableInvokeJWTError(reason string, noRefreshToken bool) *auth.Error {
	detail, code := "", reason
	if noRefreshToken {
		detail, code = " and no refresh token is available", "portal_auth_missing_refresh_token"
	}
	return portalErr(fmt.Sprintf("Icebath Portal access token is not a usable inference JWT (%s)%s. %s", reason, detail, unusableJWTRelogin), code, true)
}

func tokenFingerprint(token any) any {
	s, _ := token.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func oauthTrace(event, sequenceID string, fields map[string]any) {
	if !envTruthy(EnvOAuthTrace) {
		return
	}
	payload := map[string]any{"event": event}
	if sequenceID != "" {
		payload["sequence_id"] = sequenceID
	}
	for k, v := range fields {
		payload[k] = v
	}
	slog.Info("oauth_trace " + sortedSpacedJSON(payload))
}

func isoAfter(now time.Time, ttl int64) string {
	return isoFromEpoch(epochOf(now) + float64(ttl))
}

func parsedURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	return u, true
}

func hostOf(raw string) string {
	u, ok := parsedURL(raw)
	if !ok {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func optionalBaseURL(v any) string {
	s, _ := v.(string)
	return auth.OptionalBaseURL(s)
}

func migrateStalePortalURL(state *object) bool {
	stored := strings.TrimSpace(state.str("portal_base_url"))
	if stored == "" || !slices.Contains(stalePortalHosts, hostOf(stored)) {
		return false
	}
	slog.Warn(fmt.Sprintf("auth: migrating stale portal portal_base_url %s -> %s", stored, auth.DefaultPortalURL))
	state.set("portal_base_url", auth.DefaultPortalURL)
	return true
}

func (a *Account) scopedOverride(ctx context.Context, names ...string) string {
	ctx = a.bind(ctx)
	for _, name := range names {
		value, ok, err := a.secrets.Lookup(ctx, name)
		if err != nil {
			slog.Warn(fmt.Sprintf("portal: %s unreadable — no profile secret scope on a multiplexed call; treating the "+
				"override as absent (default routing applies). The caller needs a profile scope binding.", strings.Join(names, "/")))
			return ""
		}
		if ok && value != "" {
			return value
		}
	}
	return ""
}

func (a *Account) inferenceOverride(ctx context.Context) string {
	return auth.OptionalBaseURL(a.scopedOverride(ctx, EnvInferenceBaseURL))
}

func (a *Account) portalOverride(ctx context.Context) string {
	return auth.OptionalBaseURL(a.scopedOverride(ctx, EnvPortalBaseURL, EnvPortalBaseURLAlias))
}

func (a *Account) inferenceHostAllowed(ctx context.Context, host string) bool {
	if slices.Contains(allowedInferenceHosts, host) {
		return true
	}
	if host == "" {
		return false
	}
	override := a.inferenceOverride(ctx)
	return override != "" && hostOf(override) == host
}

func (a *Account) validateInferenceURL(ctx context.Context, raw any) string {
	s, _ := raw.(string)
	cleaned := strings.TrimSpace(s)
	if cleaned == "" {
		return ""
	}
	u, ok := parsedURL(cleaned)
	if !ok {
		return ""
	}
	if u.Scheme != "https" {
		slog.Warn(fmt.Sprintf("portal: refusing non-https inference URL scheme %q from Portal response", u.Scheme))
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !a.inferenceHostAllowed(ctx, host) {
		slog.Warn(fmt.Sprintf("portal: refusing inference URL host %q from Portal response (not in allowlist); falling back to default", host))
		return ""
	}
	return strings.TrimRight(cleaned, "/")
}

func (a *Account) InferenceBaseURL(ctx context.Context, fromNetwork string) string {
	ctx = a.bind(ctx)
	base := a.inferenceOverride(ctx)
	if base == "" {
		base = a.validateInferenceURL(ctx, fromNetwork)
	}
	if base == "" {
		base = auth.DefaultPortalInferenceURL
	}
	return strings.TrimRight(base, "/")
}

func (a *Account) portalBaseURL(ctx context.Context, state *object) string {
	if override := a.portalOverride(ctx); override != "" {
		return strings.TrimRight(override, "/")
	}
	base := optionalBaseURL(state.get("portal_base_url"))
	if base == "" {
		base = auth.DefaultPortalURL
	}
	base = strings.TrimRight(base, "/")
	if host := hostOf(base); host != "" && !slices.Contains(allowedPortalHosts, host) {
		slog.Warn(fmt.Sprintf("auth: ignoring invalid portal_base_url %q (host %q not in allowlist), using default", base, host))
		return auth.DefaultPortalURL
	}
	return base
}

type routing struct {
	portalURL       string
	storedInference string
	inference       string
	clientID        string
}

func (a *Account) effectiveRouting(ctx context.Context, state *object) routing {
	portalURL := optionalBaseURL(state.get("portal_base_url"))
	if portalURL == "" {
		portalURL = auth.DefaultPortalURL
	}
	portalURL = strings.TrimRight(portalURL, "/")
	if override := a.portalOverride(ctx); override != "" {
		portalURL = strings.TrimRight(override, "/")
	} else {
		host, scheme := "", ""
		if u, ok := parsedURL(portalURL); ok {
			host, scheme = strings.ToLower(u.Hostname()), u.Scheme
		}
		trusted := scheme == "https" || (scheme == "http" && (host == "localhost" || host == "127.0.0.1"))
		if host == "" || !slices.Contains(allowedPortalHosts, host) || !trusted {
			slog.Warn(fmt.Sprintf("auth: ignoring invalid portal_base_url %q (host %q or scheme not allowed), using default", portalURL, host))
			portalURL = auth.DefaultPortalURL
		}
	}
	stored := a.validateInferenceURL(ctx, optionalBaseURL(state.get("inference_base_url")))
	if stored == "" {
		stored = auth.DefaultPortalInferenceURL
		if isGuestState(state) {
			stored = auth.DefaultPortalWelcomeURL
		}
	}
	effective := a.inferenceOverride(ctx)
	if effective == "" {
		effective = stored
	}
	clientID := textual(state.get("client_id"))
	if !truthy(state.get("client_id")) {
		clientID = auth.DefaultPortalClientID
	}
	return routing{portalURL: portalURL, storedInference: stored, inference: effective, clientID: clientID}
}

func claimsOf(token any) *object {
	s, ok := token.(string)
	if !ok {
		return newObject()
	}
	raw, ok := auth.JWTClaims(s)
	if !ok {
		return newObject()
	}
	return objectOrEmpty(raw)
}

func scopeValues(raw any) map[string]bool {
	out := map[string]bool{}
	switch t := raw.(type) {
	case string:
		for _, part := range strings.Fields(strings.ReplaceAll(t, ",", " ")) {
			out[part] = true
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				for k := range scopeValues(s) {
					out[k] = true
				}
			}
		}
	}
	return out
}

func claimNumber(v any) (float64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	return numeric(v)
}

func isExpiring(expiresAt any, skew time.Duration) bool {
	s, ok := expiresAt.(string)
	if !ok {
		return true
	}
	return auth.IsExpiring(s, skew)
}

func invokeJWTStatus(token, scope, expiresAt any, minTTL time.Duration) string {
	claims := claimsOf(token)
	if claims.size() == 0 {
		return "access_token_not_jwt"
	}
	scopes := scopeValues(scope)
	for _, k := range []string{"scope", "scp"} {
		for s := range scopeValues(claims.get(k)) {
			scopes[s] = true
		}
	}
	if !scopes[auth.PortalInferenceInvokeScope] {
		return "missing_inference_invoke_scope"
	}
	skew := max(0, minTTL.Truncate(time.Second))
	if exp, ok := claimNumber(claims.get("exp")); ok {
		if exp <= epochOf(time.Now())+skew.Seconds() {
			return "invoke_jwt_expiring"
		}
		return ""
	}
	if isExpiring(expiresAt, skew) {
		return "invoke_jwt_expiry_unknown_or_expiring"
	}
	return ""
}

func invokeJWTUsable(token, scope, expiresAt any, minTTL time.Duration) bool {
	return invokeJWTStatus(token, scope, expiresAt, minTTL) == ""
}

func stateInvokeJWTStatus(state *object, token any) string {
	return invokeJWTStatus(token, state.get("scope"), state.get("expires_at"), auth.PortalInvokeJWTMinTTL)
}

func assertInferenceJWTUsable(state *object, token any) error {
	if reason := stateInvokeJWTStatus(state, token); reason != "" {
		return unusableInvokeJWTError(reason, false)
	}
	return nil
}

func intOf(v any) (int64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, ok := numeric(t); ok {
			return int64(math.Trunc(f)), true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func coerceTTL(v any) int64 {
	n, _ := intOf(v)
	return max(0, n)
}

func remainingTTL(expiresAt, fallbackExpiresIn any) int64 {
	if s, ok := expiresAt.(string); ok {
		if when, ok := auth.ParseISOTimestamp(s); ok {
			return max(0, int64(math.Trunc(epochOf(when)-epochOf(time.Now()))))
		}
	}
	return coerceTTL(fallbackExpiresIn)
}

func jwtExpiresAt(token, fallback any) any {
	if exp, ok := claimNumber(claimsOf(token).get("exp")); ok && exp >= -62135596800 && exp < 253402300800 {
		return isoFromEpoch(exp)
	}
	if s, ok := fallback.(string); ok {
		return s
	}
	return nil
}

func setAgentKeyFromInvokeJWT(state *object) {
	accessToken := state.get("access_token")
	if !nonEmpty(accessToken) {
		return
	}
	existing := state.get("agent_key_obtained_at")
	var obtainedAt any = isoUTC(time.Now())
	if sameValue(state.get("agent_key"), accessToken) && nonEmpty(existing) {
		obtainedAt = existing
	}
	expiresAt := jwtExpiresAt(accessToken, state.get("expires_at"))
	expiresIn := remainingTTL(expiresAt, state.get("expires_in"))
	if truthy(expiresAt) {
		state.set("expires_at", expiresAt)
		state.set("expires_in", expiresIn)
	}
	state.set("agent_key", accessToken)
	state.set("agent_key_id", nil)
	state.set("agent_key_expires_at", expiresAt)
	state.set("agent_key_expires_in", expiresIn)
	state.set("agent_key_reused", false)
	state.set("agent_key_obtained_at", obtainedAt)
}

func selectInvokeJWT(state *object, accessToken any, sequenceID string) {
	if nonEmpty(accessToken) {
		state.set("access_token", accessToken)
	}
	setAgentKeyFromInvokeJWT(state)
	slog.Debug("Icebath Portal inference auth: using NAS invoke JWT")
	oauthTrace("portal_invoke_jwt_selected", sequenceID, map[string]any{"access_token_fp": tokenFingerprint(state.get("access_token"))})
}

func effectiveState(state *object) *object {
	return state.without(effectiveIgnoredKeys...)
}

func agentKeyUsable(agentKey, scope, agentKeyExpiresAt any, minTTL time.Duration) bool {
	return nonEmpty(agentKey) && invokeJWTUsable(agentKey, scope, agentKeyExpiresAt, max(0, minTTL))
}

func parseFloat(raw string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
