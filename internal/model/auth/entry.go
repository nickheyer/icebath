package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	StatusOK        = "ok"
	StatusExhausted = "exhausted"
	StatusDead      = "dead"

	SourceManual           = "manual"
	SourceManualDeviceCode = SourceManual + ":device_code"

	ReasonBilling           = "billing"
	ReasonBillingUnverified = "billing_unverified"
	ReasonUsageWindow       = "usage_window"
	ReasonModelEntitlement  = "model_entitlement"
	ReasonPersistFailed     = "credential_persist_failed"

	CustomPoolPrefix = "custom:"

	exhaustedTTL401     = 5 * 60
	exhaustedTTL429     = 60 * 60
	exhaustedTTLDefault = 60 * 60
	exhaustedTTLSole    = 60
	deadManualPruneTTL  = 24 * 60 * 60
	modelEntitlementTTL = 365 * 24 * 60 * 60
)

var entryFieldNames = []string{
	"id", "label", "auth_type", "priority", "source", "access_token", "refresh_token", "last_status",
	"last_status_at", "last_error_code", "last_error_reason", "last_error_message", "last_error_reset_at",
	"status_cleared_at", "base_url", "expires_at", "expires_at_ms", "last_refresh", "inference_base_url",
	"agent_key", "agent_key_expires_at", "request_count", "model_cooldowns",
}

type entryRuntime struct {
	key       func(e *Entry) (Secret, bool)
	codexBase func(ctx context.Context) string
}

type Entry struct {
	Provider          string
	ID                string
	Label             string
	AuthType          string
	Priority          int
	Source            string
	AccessToken       Secret
	RefreshToken      Secret
	LastStatus        string
	LastStatusAt      time.Time
	LastErrorCode     int
	LastErrorReason   string
	LastErrorMessage  string
	LastErrorResetAt  time.Time
	StatusClearedAt   time.Time
	BaseURL           string
	ExpiresAt         string
	ExpiresAtMS       int64
	LastRefresh       string
	InferenceBaseURL  string
	AgentKey          Secret
	AgentKeyExpiresAt string
	RequestCount      int

	expiresAtNum json.Number
	statusAt     float64
	resetAt      float64
	clearedAt    float64
	cooldowns    *doc
	extra        *doc
	stored       *doc
	rt           *entryRuntime
}

var clearStatusFields = []string{
	"last_status", "last_status_at", "last_error_code", "last_error_reason", "last_error_message", "last_error_reset_at",
}

func shortID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func normalizePoolAuthType(provider, token, authType string) string {
	if provider == ProviderAnthropic && strings.HasPrefix(token, "sk-ant-oat") {
		return AuthTypeOAuth
	}
	if authType == "" {
		return AuthTypeAPIKey
	}
	return authType
}

func entryFromDoc(provider string, payload *doc) *Entry {
	e := &Entry{Provider: provider, extra: newDoc(), stored: newDoc()}
	if payload == nil {
		payload = newDoc()
	}
	for _, k := range entryFieldNames {
		if payload.has(k) {
			e.stored.set(k, payload.value(k))
		}
	}
	e.ID = textOf(payload.value("id"))
	if !payload.has("id") {
		e.ID = shortID()
	}
	e.Source = textOf(payload.value("source"))
	switch {
	case payload.has("label"):
		e.Label = textOf(payload.value("label"))
	case payload.has("source"):
		e.Label = e.Source
	default:
		e.Label = provider
	}
	if !payload.has("source") {
		e.Source = SourceManual
	}
	if n, ok := intOf(payload.value("priority")); ok {
		e.Priority = int(n)
	}
	e.AccessToken = NewSecret(payload.str("access_token"))
	e.RefreshToken = NewSecret(payload.str("refresh_token"))
	e.AuthType = normalizePoolAuthType(provider, e.AccessToken.Reveal(), textOf(payload.value("auth_type")))
	e.LastStatus = payload.str("last_status")
	if at, ok := parseAbsolute(payload.value("last_status_at")); ok {
		e.statusAt, e.LastStatusAt = at, timeOf(at)
	}
	if n, ok := intOf(payload.value("last_error_code")); ok {
		e.LastErrorCode = int(n)
	}
	e.LastErrorReason = payload.str("last_error_reason")
	e.LastErrorMessage = payload.str("last_error_message")
	if at, ok := parseAbsolute(payload.value("last_error_reset_at")); ok {
		e.resetAt, e.LastErrorResetAt = at, timeOf(at)
	}
	if at, ok := parseAbsolute(payload.value("status_cleared_at")); ok {
		e.clearedAt, e.StatusClearedAt = at, timeOf(at)
	}
	e.BaseURL = payload.str("base_url")
	switch v := payload.value("expires_at").(type) {
	case string:
		e.ExpiresAt = v
	case json.Number:
		e.expiresAtNum = v
	}
	if n, ok := intOf(payload.value("expires_at_ms")); ok {
		e.ExpiresAtMS = n
	}
	e.LastRefresh = payload.str("last_refresh")
	e.InferenceBaseURL = payload.str("inference_base_url")
	e.AgentKey = NewSecret(payload.str("agent_key"))
	e.AgentKeyExpiresAt = payload.str("agent_key_expires_at")
	if n, ok := intOf(payload.value("request_count")); ok {
		e.RequestCount = int(n)
	}
	if c := payload.obj("model_cooldowns"); c != nil {
		e.cooldowns = c.clone()
	}
	for _, k := range payload.keys {
		if slices.Contains(entryFieldNames, k) || k == "provider" || payload.vals[k] == nil {
			continue
		}
		e.extra.set(k, deepCopy(payload.vals[k]))
	}
	return e
}

func epochOrZero(v any) float64 {
	if at, ok := parseAbsolute(v); ok {
		return at
	}
	return 0
}

func (e *Entry) storedMatches(key string, raw any) bool {
	n, _ := intOf(raw)
	s, _ := raw.(string)
	switch key {
	case "id":
		return textOf(raw) == e.ID
	case "label":
		return textOf(raw) == e.Label
	case "auth_type":
		return textOf(raw) == e.AuthType
	case "source":
		return textOf(raw) == e.Source
	case "priority":
		return int(n) == e.Priority
	case "access_token":
		return s == e.AccessToken.Reveal()
	case "refresh_token":
		return s == e.RefreshToken.Reveal()
	case "agent_key":
		return s == e.AgentKey.Reveal()
	case "last_status":
		return s == e.LastStatus
	case "last_error_reason":
		return s == e.LastErrorReason
	case "last_error_message":
		return s == e.LastErrorMessage
	case "base_url":
		return s == e.BaseURL
	case "last_refresh":
		return s == e.LastRefresh
	case "inference_base_url":
		return s == e.InferenceBaseURL
	case "agent_key_expires_at":
		return s == e.AgentKeyExpiresAt
	case "expires_at":
		if num, ok := raw.(json.Number); ok {
			return e.ExpiresAt == "" && num == e.expiresAtNum
		}
		return s == e.ExpiresAt && e.expiresAtNum == ""
	case "last_status_at":
		_, isNum := raw.(json.Number)
		return isNum && epochOrZero(raw) == e.statusEpoch()
	case "last_error_code":
		return int(n) == e.LastErrorCode
	case "last_error_reset_at":
		return epochOrZero(raw) == e.resetEpoch()
	case "status_cleared_at":
		return epochOrZero(raw) == e.clearedEpoch()
	case "expires_at_ms":
		return n == e.ExpiresAtMS
	case "request_count":
		return int(n) == e.RequestCount
	}
	return false
}

func (e *Entry) statusEpoch() float64 {
	if e.LastStatusAt.IsZero() {
		return 0
	}
	if e.statusAt != 0 && timeOf(e.statusAt).Equal(e.LastStatusAt) {
		return e.statusAt
	}
	return epochOf(e.LastStatusAt)
}

func (e *Entry) resetEpoch() float64 {
	if e.LastErrorResetAt.IsZero() {
		return 0
	}
	if e.resetAt != 0 && timeOf(e.resetAt).Equal(e.LastErrorResetAt) {
		return e.resetAt
	}
	return epochOf(e.LastErrorResetAt)
}

func (e *Entry) clearedEpoch() float64 {
	if e.StatusClearedAt.IsZero() {
		return 0
	}
	if e.clearedAt != 0 && timeOf(e.clearedAt).Equal(e.StatusClearedAt) {
		return e.clearedAt
	}
	return epochOf(e.StatusClearedAt)
}

func epochOrNil(epoch float64) any {
	if epoch == 0 {
		return nil
	}
	return floatNum(epoch)
}

func (e *Entry) expiresAtValue() any {
	if e.ExpiresAt != "" {
		return e.ExpiresAt
	}
	if e.expiresAtNum != "" {
		return e.expiresAtNum
	}
	return nil
}

func (e *Entry) toDoc() *doc { return sanitizeBorrowedPayload(e.rawDoc(), e.Provider) }

func (e *Entry) rawDoc() *doc {
	d := newDoc()
	put := func(key string, v any, zero bool) {
		clearField := slices.Contains(clearStatusFields, key)
		if raw := e.stored.value(key); raw != nil && (!zero || !clearField) && e.storedMatches(key, raw) {
			d.set(key, raw)
			return
		}
		if !zero {
			d.set(key, v)
		} else if clearField {
			d.set(key, nil)
		}
	}
	put("id", e.ID, false)
	put("label", e.Label, false)
	put("auth_type", e.AuthType, false)
	put("priority", intNum(int64(e.Priority)), false)
	put("source", e.Source, false)
	put("access_token", e.AccessToken.Reveal(), false)
	put("refresh_token", e.RefreshToken.Reveal(), e.RefreshToken.Empty())
	put("last_status", e.LastStatus, e.LastStatus == "")
	put("last_status_at", epochOrNil(e.statusEpoch()), e.statusEpoch() == 0)
	put("last_error_code", intNum(int64(e.LastErrorCode)), e.LastErrorCode == 0)
	put("last_error_reason", e.LastErrorReason, e.LastErrorReason == "")
	put("last_error_message", e.LastErrorMessage, e.LastErrorMessage == "")
	put("last_error_reset_at", epochOrNil(e.resetEpoch()), e.resetEpoch() == 0)
	put("status_cleared_at", epochOrNil(e.clearedEpoch()), e.clearedEpoch() == 0)
	put("base_url", e.BaseURL, e.BaseURL == "")
	put("expires_at", e.expiresAtValue(), e.expiresAtValue() == nil)
	put("expires_at_ms", intNum(e.ExpiresAtMS), e.ExpiresAtMS == 0)
	put("last_refresh", e.LastRefresh, e.LastRefresh == "")
	put("inference_base_url", e.InferenceBaseURL, e.InferenceBaseURL == "")
	put("agent_key", e.AgentKey.Reveal(), e.AgentKey.Empty())
	put("agent_key_expires_at", e.AgentKeyExpiresAt, e.AgentKeyExpiresAt == "")
	put("request_count", intNum(int64(e.RequestCount)), false)
	if e.cooldowns != nil {
		d.set("model_cooldowns", e.cooldowns.clone())
	}
	if e.extra != nil {
		for _, k := range e.extra.keys {
			if v := e.extra.vals[k]; v != nil {
				d.set(k, deepCopy(v))
			}
		}
	}
	return d
}

func (e *Entry) clone() *Entry {
	c := *e
	c.cooldowns = e.cooldowns.clone()
	if e.extra != nil {
		c.extra = deepCopy(e.extra).(*doc)
	} else {
		c.extra = newDoc()
	}
	return &c
}

func (e *Entry) extraText(key string) string {
	if e.extra == nil {
		return ""
	}
	return e.extra.str(key)
}

func (e *Entry) setExtra(key string, v any) {
	if e.extra == nil {
		e.extra = newDoc()
	}
	if v == nil {
		e.extra.del(key)
		return
	}
	e.extra.set(key, v)
}

func (e *Entry) Extra(key string) (json.RawMessage, bool) {
	if e.extra == nil || !e.extra.has(key) {
		return nil, false
	}
	var b strings.Builder
	raw, err := json.Marshal(jsonView{e.extra.vals[key]})
	if err != nil {
		return nil, false
	}
	b.Write(raw)
	return json.RawMessage(b.String()), true
}

type jsonView struct{ v any }

func (j jsonView) MarshalJSON() ([]byte, error) {
	holder := newDoc()
	holder.set("v", j.v)
	raw, err := holder.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return raw[len(`{"v":`) : len(raw)-1], nil
}

func (e *Entry) ExtraString(key string) string { return e.extraText(key) }

func (e *Entry) ExtraKeys() []string {
	if e.extra == nil {
		return nil
	}
	return slices.Clone(e.extra.keys)
}

func (e *Entry) SetExtra(key string, raw json.RawMessage) error {
	if len(raw) == 0 {
		e.setExtra(key, nil)
		return nil
	}
	v, err := parseJSON(raw)
	if err != nil {
		return err
	}
	e.setExtra(key, v)
	return nil
}

func (e *Entry) FailureReason() string { return e.extraText("failure_reason") }

func (e *Entry) ModelCooldowns() map[string]time.Time {
	out := map[string]time.Time{}
	if e.cooldowns == nil {
		return out
	}
	for _, model := range e.cooldowns.keys {
		if until, ok := numberOf(e.cooldowns.vals[model]); ok {
			out[model] = timeOf(until)
		}
	}
	return out
}

func (e *Entry) RuntimeAPIKey() Secret {
	if e.rt != nil && e.rt.key != nil {
		if key, ok := e.rt.key(e); ok {
			return key
		}
	}
	if e.Provider == ProviderPortal {
		return Secret{}
	}
	return e.AccessToken
}

func (e *Entry) RuntimeBaseURL(ctx context.Context) string {
	switch e.Provider {
	case ProviderPortal:
		if e.InferenceBaseURL != "" {
			return e.InferenceBaseURL
		}
	case ProviderCodex:
		if e.rt != nil && e.rt.codexBase != nil {
			if override := e.rt.codexBase(ctx); override != "" {
				return override
			}
		}
	}
	return e.BaseURL
}

func isManualSource(source string) bool {
	normalized := strings.ToLower(strings.TrimSpace(source))
	return normalized == SourceManual || strings.HasPrefix(normalized, SourceManual+":")
}

func exhaustedTTL(code int, sole bool, reason string) float64 {
	if code == 401 {
		return exhaustedTTL401
	}
	base := float64(exhaustedTTLDefault)
	if code == 429 {
		base = exhaustedTTL429
	}
	if reason == ReasonBillingUnverified && code != 402 {
		return min(base, exhaustedTTLSole)
	}
	billing := code == 402 || reason == ReasonBilling
	if sole && !billing {
		return min(base, exhaustedTTLSole)
	}
	return base
}

func exhaustedUntil(e *Entry, sole bool) (float64, bool) {
	if e.LastStatus != StatusExhausted {
		return 0, false
	}
	if reset := e.resetEpoch(); reset != 0 {
		return reset, true
	}
	if at := e.statusEpoch(); at != 0 {
		return at + exhaustedTTL(e.LastErrorCode, sole, e.FailureReason()), true
	}
	return 0, false
}

func modelCooldownUntil(e *Entry, model string) (float64, bool) {
	if e.cooldowns == nil {
		return 0, false
	}
	now := nowEpoch()
	best, found := 0.0, false
	consider := func(v any) {
		if until, ok := numberOf(v); ok && until > now && (!found || until > best) {
			best, found = until, true
		}
	}
	if model == "" {
		for _, k := range e.cooldowns.keys {
			consider(e.cooldowns.vals[k])
		}
	} else {
		consider(e.cooldowns.value(model))
	}
	return best, found
}

func mergeModelCooldowns(maps ...*doc) *doc {
	merged := newDoc()
	for _, cooldowns := range maps {
		if cooldowns == nil {
			continue
		}
		for _, model := range cooldowns.keys {
			until, ok := numberOf(cooldowns.vals[model])
			if !ok {
				continue
			}
			if prior, ok := numberOf(merged.value(model)); ok && prior > until {
				until = prior
			}
			merged.set(model, floatNum(max(until, 0)))
		}
	}
	return merged
}

var persistableProviderSources = map[[2]string]bool{
	{ProviderAnthropic, "icebath_pkce"}: true,
	{ProviderMiniMax, "oauth"}:          true,
	{ProviderPortal, "device_code"}:     true,
	{ProviderCodex, "device_code"}:      true,
	{ProviderXAIOAuth, "device_code"}:   true,
}

var safeSecretishMetadataKeys = []string{
	"secret_fingerprint", "secret_source", "token_type", "scope", "client_id",
	"agent_key_id", "agent_key_expires_at", "agent_key_expires_in",
	"agent_key_reused", "agent_key_obtained_at", "expires_at", "expires_at_ms",
	"expires_in", "last_refresh", "last_status", "last_status_at",
	"last_error_code", "last_error_reason", "last_error_message",
	"last_error_reset_at",
}

var secretValueKeys = []string{
	"access_token", "refresh_token", "agent_key", "api_key", "apikey",
	"api_token", "auth_token", "authorization", "bearer_token", "client_secret",
	"credential", "credentials", "id_token", "oauth_token", "private_key",
	"secret_key", "session_token", "password", "secret", "token", "tokens",
}

var secretValueSuffixes = []string{
	"_api_key", "_api_token", "_access_token", "_auth_token", "_refresh_token",
	"_bearer_token", "_client_secret", "_id_token", "_oauth_token",
	"_private_key", "_session_token", "_secret_key", "_password", "_secret",
	"_token", "_key",
}

func normalizePayloadKey(key string) string {
	runes := []rune(strings.TrimSpace(key))
	var b strings.Builder
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.NewReplacer("-", "_", ".", "_").Replace(b.String())
}

func IsBorrowedSource(source, provider string) bool {
	normalized := strings.ToLower(strings.TrimSpace(source))
	if normalized == "" || normalized == SourceManual || strings.HasPrefix(normalized, SourceManual+":") {
		return false
	}
	return !persistableProviderSources[[2]string{strings.ToLower(strings.TrimSpace(provider)), normalized}]
}

func isSecretPayloadKey(key string) bool {
	normalized := normalizePayloadKey(key)
	if normalized == "" || slices.Contains(safeSecretishMetadataKeys, normalized) {
		return false
	}
	if slices.Contains(secretValueKeys, normalized) {
		return true
	}
	return slices.ContainsFunc(secretValueSuffixes, func(suffix string) bool { return strings.HasSuffix(normalized, suffix) })
}

func FingerprintSecret(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func fingerprintValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return FingerprintSecret(t)
	case json.Number:
		return FingerprintSecret(t.String())
	case bool:
		if t {
			return FingerprintSecret("True")
		}
		return FingerprintSecret("False")
	}
	raw, err := json.Marshal(jsonView{v})
	if err != nil {
		return ""
	}
	return FingerprintSecret(string(raw))
}

func credentialSecretFingerprint(payload *doc) string {
	candidates := []any{}
	for _, k := range []string{"agent_key", "access_token", "refresh_token", "api_key", "token", "secret"} {
		candidates = append(candidates, payload.value(k))
	}
	for _, k := range payload.keys {
		if isSecretPayloadKey(k) {
			candidates = append(candidates, payload.vals[k])
		}
	}
	for _, v := range candidates {
		if fp := fingerprintValue(v); fp != "" {
			return fp
		}
	}
	if existing := payload.str("secret_fingerprint"); strings.HasPrefix(existing, "sha256:") {
		return existing
	}
	return ""
}

func sanitizeBorrowedPayload(payload *doc, provider string) *doc {
	result := payload.clone()
	if !IsBorrowedSource(textOf(result.value("source")), provider) {
		return result
	}
	fingerprint := credentialSecretFingerprint(result)
	sanitized := newDoc()
	for _, k := range result.keys {
		if !isSecretPayloadKey(k) {
			sanitized.set(k, result.vals[k])
		}
	}
	if fingerprint != "" {
		sanitized.set("secret_fingerprint", fingerprint)
	}
	return sanitized
}
