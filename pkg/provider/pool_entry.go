package provider

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	StatusOK        = "ok"
	StatusExhausted = "exhausted"
	// Terminal failure that never recovers on its own, cleared only by a write side re-auth sync
	StatusDead = "dead"

	// Locally generated terminal reason for a rotated pair that never reached its store
	CredentialPersistFailedReason = "credential_persist_failed"
	// Quiet window after which a DEAD manual entry is pruned
	DeadManualPruneTTL = 24 * time.Hour

	AuthTypeOAuth = "oauth"

	SourceManual           = "manual"
	SourceManualDeviceCode = SourceManual + ":device_code"

	StrategyFillFirst  = "fill_first"
	StrategyRoundRobin = "round_robin"
	StrategyRandom     = "random"
	StrategyLeastUsed  = "least_used"

	ExhaustedTTL401            = 5 * time.Minute
	ExhaustedTTL429            = time.Hour
	ExhaustedTTLDefault        = time.Hour
	ExhaustedTTLSoleCredential = time.Minute

	FailureReasonBilling           = "billing"
	FailureReasonBillingUnverified = "billing_unverified"
	// Usage window plan limit that benches only the refused model
	FailureReasonUsageWindow = "usage_window"
	// Codex model entitlement bench that only an explicit reset clears
	ModelEntitlementBench = 365 * 24 * time.Hour

	NoAvailableEntriesLogThrottle     = 60 * time.Second
	CustomPoolPrefix                  = "custom:"
	DefaultMaxConcurrentPerCredential = 1
)

var SupportedPoolStrategies = map[string]bool{StrategyFillFirst: true, StrategyRoundRobin: true, StrategyRandom: true, StrategyLeastUsed: true}

// OAuth error reasons that mean the credential is permanently invalid server side
var terminalAuthReasons = map[string]bool{
	"token_invalidated": true, "token_revoked": true, "invalid_token": true,
	"invalid_grant": true, "unauthorized_client": true, "refresh_token_reused": true,
}

// Keys only round tripped through JSON and read through Extra rather than as fields
var poolExtraKeys = map[string]bool{
	"token_type": true, "scope": true, "client_id": true, "portal_base_url": true, "obtained_at": true,
	"expires_in": true, "agent_key_id": true, "agent_key_expires_in": true, "agent_key_reused": true,
	"agent_key_obtained_at": true, "tls": true, "secret_source": true, "secret_fingerprint": true,
	"auth_method": true, "account_tier": true, "anon_token": true, "user_id": true, "org_id": true,
	"failure_reason": true,
}

// Nous singleton metadata mirrored between auth.json state and the entry extras
var nousExtraStateKeys = []string{
	"obtained_at", "expires_in", "agent_key_id", "agent_key_expires_in", "agent_key_reused",
	"agent_key_obtained_at", "auth_method", "account_tier", "anon_token", "user_id", "org_id",
}

var poolFieldNames = map[string]bool{
	"id": true, "label": true, "auth_type": true, "priority": true, "source": true, "access_token": true,
	"refresh_token": true, "last_status": true, "last_status_at": true, "last_error_code": true,
	"last_error_reason": true, "last_error_message": true, "last_error_reset_at": true, "status_cleared_at": true,
	"base_url": true, "expires_at": true, "expires_at_ms": true, "last_refresh": true, "inference_base_url": true,
	"agent_key": true, "agent_key_expires_at": true, "request_count": true, "model_cooldowns": true,
}

// Fields always written on to_dict even when cleared
var poolClearStatusKeys = []string{"last_status", "last_status_at", "last_error_code", "last_error_reason", "last_error_message", "last_error_reset_at"}

// PooledCredential is one row of a provider credential pool
type PooledCredential struct {
	Provider          string
	ID                string
	Label             string
	AuthType          string
	Priority          int
	Source            string
	AccessToken       string
	RefreshToken      string
	LastStatus        string
	LastStatusAt      float64
	LastErrorCode     int
	LastErrorReason   string
	LastErrorMessage  string
	LastErrorResetAt  float64
	StatusClearedAt   float64
	BaseURL           string
	ExpiresAt         string
	ExpiresAtMS       int64
	LastRefresh       string
	InferenceBaseURL  string
	AgentKey          string
	AgentKeyExpiresAt string
	RequestCount      int
	ModelCooldowns    map[string]float64
	Extra             map[string]any
	// Profile scope the row reads ICEBATH_CODEX_BASE_URL through, nil reads the process environment
	scope *SecretScope
}

// Infers pool auth metadata for token formats with one unambiguous meaning
func normalizePoolAuthType(provider, token, authType string) string {
	if provider == "anthropic" && strings.HasPrefix(token, "sk-ant-oat") {
		return AuthTypeOAuth
	}
	if authType == "" {
		return AuthTypeAPIKey
	}
	return authType
}

func newPoolID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte{byte(time.Now().UnixNano()), byte(time.Now().UnixNano() >> 8), byte(time.Now().UnixNano() >> 16)})
	}
	return hex.EncodeToString(b[:])
}

func anyString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	}
	return stringifyAny(v)
}

func anyInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func anyFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func cooldownsFromAny(v any) map[string]float64 {
	m, ok := v.(map[string]any)
	if !ok {
		if typed, ok := v.(map[string]float64); ok {
			out := make(map[string]float64, len(typed))
			for k, val := range typed {
				out[k] = val
			}
			return out
		}
		return nil
	}
	out := map[string]float64{}
	for k, val := range m {
		if f, ok := anyFloat(val); ok {
			out[k] = f
		}
	}
	return out
}

// PooledCredentialFromMap rehydrates a stored row, defaulting id, label, auth type, priority and source
func PooledCredentialFromMap(provider string, payload map[string]any) PooledCredential {
	e := PooledCredential{Provider: provider, Extra: map[string]any{}}
	e.ID = anyString(payload["id"])
	if e.ID == "" {
		e.ID = newPoolID()
	}
	e.Source = anyString(payload["source"])
	if _, ok := payload["source"]; !ok {
		e.Source = SourceManual
	}
	if _, ok := payload["label"]; ok {
		e.Label = anyString(payload["label"])
	} else if src, ok := payload["source"]; ok {
		e.Label = anyString(src)
	} else {
		e.Label = provider
	}
	e.AuthType = anyString(payload["auth_type"])
	if _, ok := payload["auth_type"]; !ok {
		e.AuthType = AuthTypeAPIKey
	}
	e.Priority, _ = anyInt(payload["priority"])
	e.AccessToken = anyString(payload["access_token"])
	e.RefreshToken = anyString(payload["refresh_token"])
	e.LastStatus = anyString(payload["last_status"])
	if s, ok := payload["last_status_at"].(string); ok {
		e.LastStatusAt, _ = ParseAbsoluteTimestamp(s)
	} else {
		e.LastStatusAt, _ = anyFloat(payload["last_status_at"])
	}
	e.LastErrorCode, _ = anyInt(payload["last_error_code"])
	e.LastErrorReason = anyString(payload["last_error_reason"])
	e.LastErrorMessage = anyString(payload["last_error_message"])
	e.LastErrorResetAt, _ = anyFloat(payload["last_error_reset_at"])
	e.StatusClearedAt, _ = anyFloat(payload["status_cleared_at"])
	e.BaseURL = anyString(payload["base_url"])
	e.ExpiresAt = anyString(payload["expires_at"])
	if n, ok := anyFloat(payload["expires_at_ms"]); ok {
		e.ExpiresAtMS = int64(n)
	}
	e.LastRefresh = anyString(payload["last_refresh"])
	e.InferenceBaseURL = anyString(payload["inference_base_url"])
	e.AgentKey = anyString(payload["agent_key"])
	e.AgentKeyExpiresAt = anyString(payload["agent_key_expires_at"])
	e.RequestCount, _ = anyInt(payload["request_count"])
	e.ModelCooldowns = cooldownsFromAny(payload["model_cooldowns"])
	for k, v := range payload {
		if poolFieldNames[k] || k == "provider" || v == nil {
			continue
		}
		e.Extra[k] = v
	}
	e.AuthType = normalizePoolAuthType(provider, e.AccessToken, e.AuthType)
	return e
}

// ToMap serializes the row for auth.json with borrowed secrets stripped
func (e PooledCredential) ToMap() map[string]any {
	out := map[string]any{
		"id": e.ID, "label": e.Label, "auth_type": e.AuthType, "priority": e.Priority, "source": e.Source,
		"access_token": e.AccessToken, "request_count": e.RequestCount,
	}
	setStr := func(key, v string) {
		if v != "" {
			out[key] = v
		}
	}
	setStr("refresh_token", e.RefreshToken)
	setStr("base_url", e.BaseURL)
	setStr("expires_at", e.ExpiresAt)
	setStr("last_refresh", e.LastRefresh)
	setStr("inference_base_url", e.InferenceBaseURL)
	setStr("agent_key", e.AgentKey)
	setStr("agent_key_expires_at", e.AgentKeyExpiresAt)
	if e.ExpiresAtMS != 0 {
		out["expires_at_ms"] = e.ExpiresAtMS
	}
	if e.StatusClearedAt != 0 {
		out["status_cleared_at"] = e.StatusClearedAt
	}
	if len(e.ModelCooldowns) > 0 {
		out["model_cooldowns"] = cooldownsToAny(e.ModelCooldowns)
	}
	out["last_status"] = nilIfEmpty(e.LastStatus)
	out["last_status_at"] = nilIfZero(e.LastStatusAt)
	if e.LastErrorCode != 0 {
		out["last_error_code"] = e.LastErrorCode
	} else {
		out["last_error_code"] = nil
	}
	out["last_error_reason"] = nilIfEmpty(e.LastErrorReason)
	out["last_error_message"] = nilIfEmpty(e.LastErrorMessage)
	out["last_error_reset_at"] = nilIfZero(e.LastErrorResetAt)
	for k, v := range e.Extra {
		if v != nil {
			out[k] = v
		}
	}
	return SanitizeBorrowedCredentialPayload(out, e.Provider)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// Clone copies the row with independent extra and cooldown maps
func (e PooledCredential) Clone() PooledCredential {
	c := e
	c.Extra = make(map[string]any, len(e.Extra))
	for k, v := range e.Extra {
		c.Extra[k] = v
	}
	if e.ModelCooldowns != nil {
		c.ModelCooldowns = make(map[string]float64, len(e.ModelCooldowns))
		for k, v := range e.ModelCooldowns {
			c.ModelCooldowns[k] = v
		}
	}
	return c
}

// ExtraString reads a JSON only key such as scope or failure_reason
func (e PooledCredential) ExtraString(key string) string { return anyString(e.Extra[key]) }

// FailureReason is the classifier verdict persisted with the last exhaustion
func (e PooledCredential) FailureReason() string { return e.ExtraString("failure_reason") }

// ClearStatus blanks every error field
func (e PooledCredential) ClearStatus() PooledCredential {
	e.LastStatus, e.LastStatusAt, e.LastErrorCode = "", 0, 0
	e.LastErrorReason, e.LastErrorMessage, e.LastErrorResetAt = "", "", 0
	return e
}

// MarkOK clears every error field and stamps the ok status
func (e PooledCredential) MarkOK() PooledCredential {
	e = e.ClearStatus()
	e.LastStatus = StatusOK
	return e
}

// WithExtra returns a copy whose extras map has key set, or deleted when value is nil
func (e PooledCredential) WithExtra(key string, value any) PooledCredential {
	c := e.Clone()
	if value == nil {
		delete(c.Extra, key)
	} else {
		c.Extra[key] = value
	}
	return c
}

func (e PooledCredential) secretStr(name string) string {
	if e.scope != nil {
		v, _ := e.scope.GetSecret(name)
		return v
	}
	return processSecret(name)
}

// RuntimeAPIKey is the bearer a request sends, a usable Nous invoke JWT for nous
func (e PooledCredential) RuntimeAPIKey() string {
	if e.Provider == "nous" {
		for _, pair := range [][2]string{{e.AgentKey, e.AgentKeyExpiresAt}, {e.AccessToken, e.ExpiresAt}} {
			token := strings.TrimSpace(pair[0])
			if token != "" && NousInvokeJWTStatus(token, e.Extra["scope"], pair[1], NousInvokeJWTMinTTL) == "" {
				return token
			}
		}
		return ""
	}
	return e.AccessToken
}

// RuntimeBaseURL is the endpoint the row serves, the codex proxy override winning for every reader
func (e PooledCredential) RuntimeBaseURL() string {
	switch e.Provider {
	case "nous":
		if e.InferenceBaseURL != "" {
			return e.InferenceBaseURL
		}
		return e.BaseURL
	case "openai-codex":
		if override := strings.TrimRight(strings.TrimSpace(e.secretStr("ICEBATH_CODEX_BASE_URL")), "/"); override != "" {
			return override
		}
	}
	return e.BaseURL
}

func scopeValues(v any) map[string]bool {
	out := map[string]bool{}
	switch s := v.(type) {
	case string:
		for _, part := range strings.Fields(s) {
			out[part] = true
		}
	case []any:
		for _, item := range s {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				out[strings.TrimSpace(str)] = true
			}
		}
	case []string:
		for _, str := range s {
			if strings.TrimSpace(str) != "" {
				out[strings.TrimSpace(str)] = true
			}
		}
	}
	return out
}

// NousInvokeJWTStatus is empty when the token can be used for inference, else the refusal reason
func NousInvokeJWTStatus(token string, scope any, expiresAt any, minTTL time.Duration) string {
	claims := DecodeJWTClaims(token)
	if len(claims) == 0 {
		return "access_token_not_jwt"
	}
	scopes := scopeValues(scope)
	for k := range scopeValues(claims["scope"]) {
		scopes[k] = true
	}
	for k := range scopeValues(claims["scp"]) {
		scopes[k] = true
	}
	if !scopes[NousInferenceInvokeScope] {
		return "missing_inference_invoke_scope"
	}
	if minTTL < 0 {
		minTTL = 0
	}
	if exp, ok := anyFloat(claims["exp"]); ok {
		if exp <= nowEpoch()+minTTL.Seconds() {
			return "invoke_jwt_expiring"
		}
		return ""
	}
	if IsExpiring(expiresAt, minTTL) {
		return "invoke_jwt_expiry_unknown_or_expiring"
	}
	return ""
}

// NousInvokeJWTUsable reports whether the token carries a live inference invoke scope
func NousInvokeJWTUsable(token string, scope any, expiresAt any) bool {
	return NousInvokeJWTStatus(token, scope, expiresAt, NousInvokeJWTMinTTL) == ""
}

// LabelFromToken picks the email like claim of a JWT as the display label
func LabelFromToken(token, fallback string) string {
	claims := DecodeJWTClaims(token)
	for _, key := range []string{"email", "preferred_username", "upn"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return fallback
}

// Account id and subject of a Codex access token, both required to name one principal
func codexPrincipalIdentity(accessToken string) (string, string, bool) {
	claims := DecodeJWTClaims(accessToken)
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	account, _ := authClaims["chatgpt_account_id"].(string)
	subject, _ := claims["sub"].(string)
	if strings.TrimSpace(account) == "" || strings.TrimSpace(subject) == "" {
		return "", "", false
	}
	return strings.TrimSpace(account), strings.TrimSpace(subject), true
}

// Whether a Codex pool row may adopt the auth.json singleton token pair
func codexEntryTracksSingleton(entry PooledCredential, singletonTokens map[string]any) bool {
	if entry.Source == "device_code" {
		return true
	}
	if entry.Source != SourceManualDeviceCode {
		return false
	}
	acc, sub, ok := codexPrincipalIdentity(entry.AccessToken)
	if !ok {
		return false
	}
	sAcc, sSub, sok := codexPrincipalIdentity(anyString(singletonTokens["access_token"]))
	return sok && acc == sAcc && sub == sSub
}

func nextPriority(entries []PooledCredential) int {
	max := -1
	for _, e := range entries {
		if e.Priority > max {
			max = e.Priority
		}
	}
	return max + 1
}

func isManualSource(source string) bool {
	n := strings.ToLower(strings.TrimSpace(source))
	return n == SourceManual || strings.HasPrefix(n, SourceManual+":")
}

// Cooldown for an exhaustion sized by the status, the classifier verdict and whether anything else can serve
func exhaustedTTL(errorCode int, soleCredential bool, failureReason string) time.Duration {
	if errorCode == 401 {
		return ExhaustedTTL401
	}
	base := ExhaustedTTLDefault
	if errorCode == 429 {
		base = ExhaustedTTL429
	}
	if failureReason == FailureReasonBillingUnverified && errorCode != 402 {
		return minDuration(base, ExhaustedTTLSoleCredential)
	}
	isBilling := errorCode == 402 || failureReason == FailureReasonBilling
	if soleCredential && !isBilling {
		return minDuration(base, ExhaustedTTLSoleCredential)
	}
	return base
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// True only when the auth.json singleton is provably older than the entry
func singletonPredatesEntry(state map[string]any, entry PooledCredential) bool {
	entryTS, ok := ParseAbsoluteTimestamp(entry.LastRefresh)
	if !ok {
		return false
	}
	stateTS, ok := ParseAbsoluteTimestamp(state["last_refresh"])
	if !ok {
		return false
	}
	return stateTS < entryTS
}

// Trims reason and message and resolves the reset epoch from timestamps or the message text
func normalizeErrorContext(errorContext map[string]any) map[string]any {
	out := map[string]any{}
	if errorContext == nil {
		return out
	}
	for _, key := range []string{"reason", "message"} {
		if v, ok := errorContext[key].(string); ok && strings.TrimSpace(v) != "" {
			out[key] = strings.TrimSpace(v)
		}
	}
	var resetAt any
	for _, key := range []string{"reset_at", "resets_at", "retry_until"} {
		if v := errorContext[key]; truthyAny(v) {
			resetAt = v
			break
		}
	}
	parsed, ok := ParseAbsoluteTimestamp(resetAt)
	if !ok {
		if msg, isStr := errorContext["message"].(string); isStr {
			if delay, found := ResetDelayFromMessage(msg); found {
				parsed, ok = nowEpoch()+delay.Seconds(), true
			}
		}
	}
	if ok {
		out["reset_at"] = parsed
	}
	return out
}

// exhaustedUntil is the epoch an exhausted entry re-enters rotation, false when none is known
func exhaustedUntil(entry PooledCredential, soleCredential bool) (float64, bool) {
	if entry.LastStatus != StatusExhausted {
		return 0, false
	}
	if resetAt, ok := ParseAbsoluteTimestamp(entry.LastErrorResetAt); ok {
		return resetAt, true
	}
	if entry.LastStatusAt != 0 {
		return entry.LastStatusAt + exhaustedTTL(entry.LastErrorCode, soleCredential, entry.FailureReason()).Seconds(), true
	}
	return 0, false
}

// PoolFailureReason is the reason persisted for a rotation, usage window and unverified billing sized apart
func PoolFailureReason(reason FailoverReason, errorContext map[string]any, billingUnverified bool) string {
	if reason == ReasonBilling && truthyAny(errorContext["usage_window"]) {
		return FailureReasonUsageWindow
	}
	if reason == ReasonBilling && billingUnverified {
		return FailureReasonBillingUnverified
	}
	return string(reason)
}

// ModelCooldownUntil is the active cooldown blocking the entry for model, any model when unscoped
func ModelCooldownUntil(entry PooledCredential, model string) (float64, bool) {
	now := nowEpoch()
	best, found := 0.0, false
	consider := func(until float64) {
		if until > now && (!found || until > best) {
			best, found = until, true
		}
	}
	if model == "" {
		for _, until := range entry.ModelCooldowns {
			consider(until)
		}
	} else if until, ok := entry.ModelCooldowns[model]; ok {
		consider(until)
	}
	return best, found
}

// MergeModelCooldowns keeps the latest reset per model across snapshots
func MergeModelCooldowns(maps ...any) map[string]float64 {
	merged := map[string]float64{}
	for _, raw := range maps {
		for model, until := range cooldownsFromAny(raw) {
			if until > merged[model] {
				merged[model] = until
			}
		}
	}
	return merged
}

func sortedCooldownModels(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
