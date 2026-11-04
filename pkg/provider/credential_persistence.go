package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

type providerSource struct{ provider, source string }

var persistableProviderSources = map[providerSource]bool{
	{"anthropic", "hermes_pkce"}:    true,
	{"minimax-oauth", "oauth"}:      true,
	{"nous", "device_code"}:         true,
	{"openai-codex", "device_code"}: true,
	{"xai-oauth", "device_code"}:    true,
}

var safeSecretishMetadataKeys = map[string]bool{
	"secret_fingerprint": true, "secret_source": true, "token_type": true, "scope": true, "client_id": true,
	"agent_key_id": true, "agent_key_expires_at": true, "agent_key_expires_in": true,
	"agent_key_reused": true, "agent_key_obtained_at": true, "expires_at": true, "expires_at_ms": true,
	"expires_in": true, "last_refresh": true, "last_status": true, "last_status_at": true,
	"last_error_code": true, "last_error_reason": true, "last_error_message": true,
	"last_error_reset_at": true,
}

var secretValueKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "agent_key": true, "api_key": true, "apikey": true,
	"api_token": true, "auth_token": true, "authorization": true, "bearer_token": true, "client_secret": true,
	"credential": true, "credentials": true, "id_token": true, "oauth_token": true, "private_key": true,
	"secret_key": true, "session_token": true, "password": true, "secret": true, "token": true, "tokens": true,
}

var secretValueSuffixes = []string{
	"_api_key", "_api_token", "_access_token", "_auth_token", "_refresh_token",
	"_bearer_token", "_client_secret", "_id_token", "_oauth_token",
	"_private_key", "_session_token", "_secret_key", "_password", "_secret",
	"_token", "_key",
}

// normalizeSecretKey lowercases a payload key after splitting camel case and unifying separators
func normalizeSecretKey(key string) string {
	key = strings.TrimSpace(key)
	var b strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	out := strings.ToLower(b.String())
	return strings.NewReplacer("-", "_", ".", "_").Replace(out)
}

// IsBorrowedCredentialSource reports whether a pool source is reference only and must never persist secrets
func IsBorrowedCredentialSource(source, providerID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(source))
	if normalized == "" || normalized == "manual" || strings.HasPrefix(normalized, "manual:") {
		return false
	}
	return !persistableProviderSources[providerSource{strings.ToLower(strings.TrimSpace(providerID)), normalized}]
}

func isSecretPayloadKey(key string) bool {
	normalized := normalizeSecretKey(key)
	if normalized == "" || safeSecretishMetadataKeys[normalized] {
		return false
	}
	if secretValueKeys[normalized] {
		return true
	}
	for _, suffix := range secretValueSuffixes {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// FingerprintSecretValue returns sha256 prefixed sixteen hex digest of a secret or empty for blank input
func FingerprintSecretValue(value any) string {
	text := stringifyAny(value)
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// stringifyAny renders a JSON scalar the way Python str does for fingerprinting
func stringifyAny(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func credentialSecretFingerprint(payload map[string]any) string {
	for _, k := range []string{"agent_key", "access_token", "refresh_token", "api_key", "token", "secret"} {
		if fp := FingerprintSecretValue(payload[k]); fp != "" {
			return fp
		}
	}
	for _, k := range sortedMapKeys(payload) {
		if isSecretPayloadKey(k) {
			if fp := FingerprintSecretValue(payload[k]); fp != "" {
				return fp
			}
		}
	}
	if existing, ok := payload["secret_fingerprint"].(string); ok && strings.HasPrefix(existing, "sha256:") {
		return existing
	}
	return ""
}

// SanitizeBorrowedCredentialPayload strips raw secret fields from borrowed pool rows and leaves owned rows unchanged
func SanitizeBorrowedCredentialPayload(payload map[string]any, providerID string) map[string]any {
	result := make(map[string]any, len(payload))
	for k, v := range payload {
		result[k] = v
	}
	source, _ := result["source"].(string)
	if !IsBorrowedCredentialSource(source, providerID) {
		return result
	}
	fingerprint := credentialSecretFingerprint(result)
	sanitized := make(map[string]any, len(result))
	for k, v := range result {
		if !isSecretPayloadKey(k) {
			sanitized[k] = v
		}
	}
	if fingerprint != "" {
		sanitized["secret_fingerprint"] = fingerprint
	}
	return sanitized
}
