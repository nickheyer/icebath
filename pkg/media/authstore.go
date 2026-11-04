package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Default Nous Portal origin for OAuth account and token calls
const DefaultNousPortalURL = "https://portal.nousresearch.com"

// Default origin of the Codex backend the OAuth token belongs to
const CodexAuxBaseURL = "https://chatgpt.com/backend-api/codex"

// Default xAI OAuth inference origin
const DefaultXAIOAuthBaseURL = "https://api.x.ai/v1"

const nousAccessTokenRefreshSkew = 120 * time.Second

// Decoded auth.json of one home
type authStore struct {
	Providers      map[string]map[string]any
	CredentialPool map[string][]map[string]any
}

// Reads auth.json at path, returning an empty store when absent or unreadable
func loadAuthStore(path string) authStore {
	out := authStore{Providers: map[string]map[string]any{}, CredentialPool: map[string][]map[string]any{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	if providers, ok := doc["providers"].(map[string]any); ok {
		for k, v := range providers {
			if m, ok := v.(map[string]any); ok {
				out.Providers[k] = m
			}
		}
	}
	if pool, ok := doc["credential_pool"].(map[string]any); ok {
		for k, v := range pool {
			if list, ok := v.([]any); ok {
				for _, item := range list {
					if m, ok := item.(map[string]any); ok {
						out.CredentialPool[k] = append(out.CredentialPool[k], m)
					}
				}
			}
		}
	}
	return out
}

// Provider state from the profile auth.json, then the global root auth.json
func providerAuthState(env *tools.Env, provider string) (map[string]any, string) {
	h := homeOf(env)
	store := loadAuthStore(home.AuthPath(h))
	if st, ok := store.Providers[provider]; ok {
		return st, home.AuthPath(h)
	}
	if root := home.RootOf(h); root != "" && root != h {
		global := loadAuthStore(home.AuthPath(root))
		if st, ok := global.Providers[provider]; ok {
			return st, home.AuthPath(root)
		}
	}
	return nil, ""
}

// Credential pool rows for a provider from the profile store, then the global root
func credentialPool(env *tools.Env, provider string) []map[string]any {
	h := homeOf(env)
	if rows := loadAuthStore(home.AuthPath(h)).CredentialPool[provider]; len(rows) > 0 {
		return rows
	}
	if root := home.RootOf(h); root != "" && root != h {
		return loadAuthStore(home.AuthPath(root)).CredentialPool[provider]
	}
	return nil
}

// JWT payload claims or an empty map for malformed tokens
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return map[string]any{}
	}
	return claims
}

// Reports whether a JWT carries an exp claim in the past
func jwtExpired(token string) bool {
	exp, ok := numAt(jwtClaims(token), "exp")
	return ok && exp > 0 && float64(time.Now().Unix()) > exp
}

// Parses an ISO 8601 timestamp into epoch seconds
func parseISOTimestamp(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return 0, false
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.Unix()), true
		}
	}
	return 0, false
}

// Reports whether an expiry is unknown or within the skew of now
func isExpiring(expiresAt any, skew time.Duration) bool {
	epoch, ok := parseISOTimestamp(expiresAt)
	if !ok {
		return true
	}
	return epoch <= float64(time.Now().Add(skew).Unix())
}

// Reports whether a Nous state belongs to the anonymous guest flow
func isGuestState(state map[string]any) bool {
	return state != nil && strAt(state, "auth_method") == "anonymous"
}

// Reports whether nous.guest in config still admits the guest identity
func guestAllowed(env *tools.Env) bool {
	nous := section(cfgOf(env), "nous")
	if v, ok := nous["guest"]; ok && v != nil {
		return truthy(v, true)
	}
	return true
}

// Bearer a managed tools call sends, the profile token else a guest connectors token
func heldConnectorsToken(state map[string]any, skew time.Duration, acceptExpiring bool) string {
	if state == nil {
		return ""
	}
	slots := []map[string]any{state}
	if isGuestState(state) {
		if ct, ok := state["connectors_token"].(map[string]any); ok {
			slots = append(slots, ct)
		}
	}
	type held struct {
		token string
		exp   any
	}
	var tokens []held
	for _, s := range slots {
		if t, ok := s["access_token"].(string); ok && t != "" {
			tokens = append(tokens, held{t, s["expires_at"]})
		}
	}
	for _, h := range tokens {
		if !isExpiring(h.exp, skew) {
			return h.token
		}
	}
	if len(tokens) > 0 && acceptExpiring {
		return tokens[0].token
	}
	return ""
}

// Nous provider state unless it is a guest identity that config no longer admits
func nousProviderState(env *tools.Env) (map[string]any, string) {
	state, path := providerAuthState(env, "nous")
	if state == nil {
		return nil, ""
	}
	if isGuestState(state) && !guestAllowed(env) {
		return nil, ""
	}
	return state, path
}

// Nous Portal origin, the env override then the persisted state value
func nousPortalBaseURL(env *tools.Env, state map[string]any) string {
	if v := firstSecret(env, "ICEBATH_PORTAL_BASE_URL", "NOUS_PORTAL_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := stripAt(state, "portal_base_url"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultNousPortalURL
}

var nousRefreshMu sync.Mutex

// Refreshes the Nous access token with the refresh token header and persists the rotated pair
func refreshNousToken(ctx context.Context, env *tools.Env, state map[string]any, path string) (string, error) {
	refresh := stripAt(state, "refresh_token")
	if refresh == "" {
		return "", errors.New("no Nous refresh token")
	}
	nousRefreshMu.Lock()
	defer nousRefreshMu.Unlock()
	form := url.Values{"grant_type": {"refresh_token"}}
	if cid := stripAt(state, "client_id"); cid != "" {
		form.Set("client_id", cid)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nousPortalBaseURL(env, state)+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-nous-refresh-token", refresh)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Nous Portal token refresh failed (HTTP %d)", resp.StatusCode)
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", errors.New("Nous Portal token refresh returned invalid JSON")
	}
	token := stripAt(payload, "access_token")
	if token == "" {
		return "", errors.New("Refresh response missing access_token")
	}
	persistNousRefresh(path, state, payload)
	return token, nil
}

// Writes a refreshed Nous token pair back into the auth.json it was read from
func persistNousRefresh(path string, state, payload map[string]any) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc) != nil {
		return
	}
	providers, ok := doc["providers"].(map[string]any)
	if !ok {
		return
	}
	nous, ok := providers["nous"].(map[string]any)
	if !ok {
		return
	}
	nous["access_token"] = payload["access_token"]
	if rt := stripAt(payload, "refresh_token"); rt != "" {
		nous["refresh_token"] = rt
	}
	if exp, ok := numAt(payload, "expires_in"); ok && exp > 0 {
		nous["expires_at"] = time.Now().Add(time.Duration(exp) * time.Second).UTC().Format(time.RFC3339)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, out, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// Cheap Nous token probe from the env override or the cached store with no refresh
func peekNousAccessToken(env *tools.Env) string {
	if v := secret(env, "TOOL_GATEWAY_USER_TOKEN"); v != "" {
		return v
	}
	state, _ := nousProviderState(env)
	return heldConnectorsToken(state, 0, true)
}

// Refresh aware Nous access token, empty when no identity is held
func readNousAccessToken(ctx context.Context, env *tools.Env) string {
	if v := secret(env, "TOOL_GATEWAY_USER_TOKEN"); v != "" {
		return v
	}
	state, path := nousProviderState(env)
	if state == nil {
		return ""
	}
	if fresh := heldConnectorsToken(state, nousAccessTokenRefreshSkew, false); fresh != "" {
		return fresh
	}
	cached := heldConnectorsToken(state, 0, true)
	if refreshed, err := refreshNousToken(ctx, env, state, path); err == nil && refreshed != "" {
		return refreshed
	} else if err != nil {
		logOf(env).Debug("Nous access token refresh failed", "error", err)
	}
	return cached
}

// Snapshot of Nous Portal entitlement read from the held OAuth JWT
type nousAccountInfo struct {
	LoggedIn       bool
	PaidAccess     *bool
	ToolAccess     bool
	ToolCoverage   map[string]bool
	AccountTier    string
	PortalBaseURL  string
	InferenceBase  string
	ExpiresAt      time.Time
	Error          string
	ManagedTools   *bool
	OrgID          string
	SubscriptionID string
}

// Reports whether paid access or a live free tool pool entitles the managed gateway
func (n nousAccountInfo) toolGatewayEntitled() bool {
	return (n.PaidAccess != nil && *n.PaidAccess) || n.ToolAccess
}

// Reports whether a category is covered for pool users or anything for paid users
func (n nousAccountInfo) toolGatewayEntitledFor(category string) bool {
	if n.PaidAccess != nil && *n.PaidAccess {
		return true
	}
	return n.ToolAccess && n.ToolCoverage[category]
}

// Bool pointer from a JSON claim value
func claimBool(v any) *bool {
	switch t := v.(type) {
	case bool:
		return &t
	case string:
		n := strings.ToLower(strings.TrimSpace(t))
		if truthyStrings[n] {
			b := true
			return &b
		}
		if falsyStrings[n] {
			b := false
			return &b
		}
	}
	return nil
}

// Tool access block of a JWT or account payload
func toolAccessFrom(v any) (bool, map[string]bool) {
	coverage := map[string]bool{}
	switch t := v.(type) {
	case bool:
		return t, coverage
	case map[string]any:
		enabled := truthy(t["enabled"], false)
		if cov, ok := t["coverage"].(map[string]any); ok {
			for k, val := range cov {
				coverage[k] = truthy(val, false)
			}
		}
		return enabled, coverage
	}
	return false, coverage
}

var accountInfoCache struct {
	sync.Mutex
	key  string
	at   time.Time
	info nousAccountInfo
}

const accountInfoCacheTTL = 5 * time.Minute

// Account snapshot from JWT claims, refreshed against the portal when the token lacks claims or is forced
func nousPortalAccountInfo(ctx context.Context, env *tools.Env, forceFresh bool) nousAccountInfo {
	state, _ := nousProviderState(env)
	if state == nil {
		return nousAccountInfo{}
	}
	if isGuestState(state) && !truthy(state["free_inference"], false) && stripAt(state, "inference_base_url") == "" {
		return nousAccountInfo{}
	}
	token := heldConnectorsToken(state, 0, true)
	if token == "" {
		return nousAccountInfo{}
	}
	portal := nousPortalBaseURL(env, state)
	if !forceFresh {
		if info, ok := accountInfoFromJWT(token, state, portal, 60); ok {
			return info
		}
	}
	return freshAccountInfo(ctx, env, state, portal, forceFresh)
}

// Snapshot from an unexpired JWT with the entitlement claims
func accountInfoFromJWT(token string, state map[string]any, portal string, minTTL int) (nousAccountInfo, bool) {
	claims := jwtClaims(token)
	if len(claims) == 0 {
		return nousAccountInfo{}, false
	}
	exp, ok := numAt(claims, "exp")
	if !ok || exp <= float64(time.Now().Unix()+int64(minTTL)) {
		return nousAccountInfo{}, false
	}
	enabled, coverage := toolAccessFrom(claims["tool_access"])
	tier := stripAt(claims, "account_tier")
	if tier == "" {
		tier = stripAt(state, "account_tier")
	}
	return nousAccountInfo{
		LoggedIn: true, PaidAccess: claimBool(claims["paid_access"]), ToolAccess: enabled, ToolCoverage: coverage,
		AccountTier: tier, PortalBaseURL: portal, InferenceBase: stripAt(state, "inference_base_url"),
		ExpiresAt: time.Unix(int64(exp), 0), ManagedTools: claimBool(claims["managed_tools"]), OrgID: stripAt(claims, "org_id"),
	}, true
}

// Snapshot from the portal account endpoint, cached per token digest
func freshAccountInfo(ctx context.Context, env *tools.Env, state map[string]any, portal string, forceFresh bool) nousAccountInfo {
	token := readNousAccessToken(ctx, env)
	if token == "" {
		return nousAccountInfo{LoggedIn: false, Error: "no Nous access token"}
	}
	key := portal + ":" + token
	accountInfoCache.Lock()
	if !forceFresh && accountInfoCache.key == key && time.Since(accountInfoCache.at) < accountInfoCacheTTL {
		info := accountInfoCache.info
		accountInfoCache.Unlock()
		return info
	}
	accountInfoCache.Unlock()
	payload, status, err := getJSON(ctx, portal+"/api/oauth/account", map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}, 15*time.Second)
	if err != nil {
		return nousAccountInfo{LoggedIn: true, PortalBaseURL: portal, Error: fmt.Sprintf("account lookup failed (HTTP %d): %v", status, err)}
	}
	if errText := stripAt(payload, "error"); errText != "" {
		return nousAccountInfo{LoggedIn: true, PortalBaseURL: portal, Error: errText}
	}
	enabled, coverage := toolAccessFrom(payload["tool_access"])
	paid := claimBool(payload["paid_access"])
	if paid == nil {
		if psa, ok := payload["paid_service_access"].(map[string]any); ok {
			paid = claimBool(psa["allowed"])
		}
	}
	info := nousAccountInfo{
		LoggedIn: true, PaidAccess: paid, ToolAccess: enabled, ToolCoverage: coverage,
		AccountTier: stripAt(payload, "account_tier"), PortalBaseURL: portal,
		InferenceBase: stripAt(state, "inference_base_url"), ManagedTools: claimBool(payload["managed_tools"]),
		OrgID: stripAt(payload, "org_id"),
	}
	accountInfoCache.Lock()
	accountInfoCache.key, accountInfoCache.at, accountInfoCache.info = key, time.Now(), info
	accountInfoCache.Unlock()
	return info
}

// Reports whether the account is entitled to the Nous Tool Gateway, failing closed on errors
func managedNousToolsEnabled(ctx context.Context, env *tools.Env, forceFresh bool) bool {
	info := nousPortalAccountInfo(ctx, env, forceFresh)
	return info.LoggedIn && info.toolGatewayEntitled()
}

// Account aware guidance for an unavailable Nous Tool Gateway path
func nousToolGatewayUnavailableMessage(ctx context.Context, env *tools.Env, capability string, forceFresh bool) string {
	info := nousPortalAccountInfo(ctx, env, forceFresh)
	switch {
	case !info.LoggedIn:
		return fmt.Sprintf("%s requires a Nous Portal login. Run `icebath model` and sign in to Nous Portal.", capability)
	case info.Error != "":
		return fmt.Sprintf("%s is unavailable: %s. Run `icebath model` to refresh your Nous Portal login and billing status.", capability, info.Error)
	case !info.toolGatewayEntitled():
		return fmt.Sprintf("%s requires an active Nous Subscription or tool pool on your Portal account. Visit %s/billing to enable it.", capability, info.PortalBaseURL)
	}
	return fmt.Sprintf("%s is unavailable. Run `icebath model` to refresh your Nous Portal login and billing status.", capability)
}

// Resolved managed tool gateway for one vendor
type managedGateway struct {
	Vendor        string
	GatewayOrigin string
	NousUserToken string
}

// Gateway URL scheme from the environment, http or https
func toolGatewayScheme(env *tools.Env) (string, error) {
	scheme := strings.ToLower(secret(env, "TOOL_GATEWAY_SCHEME"))
	if scheme == "" {
		return "https", nil
	}
	if scheme != "http" && scheme != "https" {
		return "", errors.New("TOOL_GATEWAY_SCHEME must be 'http' or 'https'")
	}
	return scheme, nil
}

// Gateway origin for a vendor, an explicit override else the shared gateway domain
func buildVendorGatewayURL(env *tools.Env, vendor string) (string, error) {
	envName := strings.ToUpper(strings.ReplaceAll(vendor, "-", "_")) + "_GATEWAY_URL"
	if explicit := strings.TrimRight(secret(env, envName), "/"); explicit != "" {
		return explicit, nil
	}
	domain := strings.Trim(secret(env, "TOOL_GATEWAY_DOMAIN"), "/")
	if domain == "" {
		domain = "nousresearch.com"
	}
	scheme, err := toolGatewayScheme(env)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s-gateway.%s", scheme, vendor, domain), nil
}

// Managed gateway config for an entitled account, nil otherwise
func resolveManagedToolGateway(ctx context.Context, env *tools.Env, vendor string) *managedGateway {
	if !managedNousToolsEnabled(ctx, env, false) {
		return nil
	}
	origin, err := buildVendorGatewayURL(env, vendor)
	if err != nil || origin == "" {
		return nil
	}
	token := readNousAccessToken(ctx, env)
	if token == "" {
		return nil
	}
	return &managedGateway{Vendor: vendor, GatewayOrigin: origin, NousUserToken: token}
}

// Reports whether a gateway URL and a cached token are present without any refresh
func isManagedToolGatewayReady(ctx context.Context, env *tools.Env, vendor string) bool {
	if !managedNousToolsEnabled(ctx, env, false) {
		return false
	}
	origin, err := buildVendorGatewayURL(env, vendor)
	return err == nil && origin != "" && peekNousAccessToken(env) != ""
}

// Codex OAuth token and the base URL it routes to, empty token when none is usable
func codexCredential(env *tools.Env) (string, string) {
	override := strings.TrimRight(secret(env, "ICEBATH_CODEX_BASE_URL"), "/")
	for _, row := range credentialPool(env, "openai-codex") {
		token := stripAt(row, "runtime_api_key")
		if token == "" {
			token = stripAt(row, "access_token")
		}
		if token == "" || jwtExpired(token) {
			continue
		}
		if override != "" {
			return token, override
		}
		if base := strings.TrimRight(stripAt(row, "base_url"), "/"); base != "" {
			return token, base
		}
		return token, CodexAuxBaseURL
	}
	state, _ := providerAuthState(env, "openai-codex")
	tokens, _ := state["tokens"].(map[string]any)
	token := stripAt(tokens, "access_token")
	if token == "" || jwtExpired(token) {
		if override != "" {
			return "", override
		}
		return "", CodexAuxBaseURL
	}
	if override != "" {
		return token, override
	}
	return token, CodexAuxBaseURL
}

// Reports whether a base URL is the official OpenAI Codex endpoint
func isOfficialCodexBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme == "https" && u.Hostname() == "chatgpt.com" && (u.Port() == "" || u.Port() == "443") &&
		(path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"))
}

// Identity and account headers for the Codex backend derived from the OAuth JWT
func codexHeaders(token, baseURL string) map[string]string {
	headers := map[string]string{"User-Agent": "codex_cli_rs/0.0.0 (Icebath Agent)", "originator": "codex_cli_rs"}
	if isOfficialCodexBaseURL(baseURL) {
		headers = map[string]string{"User-Agent": UserAgent, "originator": "icebath-agent"}
	}
	auth := dictAt(jwtClaims(token), "https://api.openai.com/auth")
	if acct := stripAt(auth, "chatgpt_account_id"); acct != "" {
		headers["ChatGPT-Account-ID"] = acct
	}
	residency := stripAt(auth, "chatgpt_data_residency")
	if residency == "" {
		residency = stripAt(auth, "chatgpt_compute_residency")
	}
	if residency != "" {
		headers["x-openai-internal-codex-residency"] = residency
	}
	return headers
}
