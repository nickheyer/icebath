package browser

import (
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Credential bearing query parameter names masked in CDP endpoints
var sensitiveQueryParams = map[string]bool{
	"access_token": true, "api_key": true, "apikey": true, "auth_token": true, "authorization": true,
	"awsaccesskeyid": true, "client_secret": true, "credential": true, "credentials": true, "jwt": true,
	"password": true, "passwd": true, "secret": true, "session_id": true, "signature": true, "token": true,
	"x_amz_security_token": true, "x_amz_signature": true, "x-amz-security-token": true, "x-amz-signature": true,
	"key": true, "sig": true, "auth": true, "session": true, "code": true,
}

var (
	urlWithQueryRE = regexp.MustCompile(`(?i)\b(https?|wss?)://([^\s/?#]+)([^\s?#]*)\?([^\s#]+)(#\S*)?`)
	urlUserinfoRE  = regexp.MustCompile(`(?i)\b(https?|wss?)://([^\s:@/]+):([^\s@/]+)@`)
	// Vendor key prefixes that mark a credential wherever they appear
	credentialPrefixRE = regexp.MustCompile(`(?:sk-(?:proj-|ant-|or-v1-)?[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{15,}|hf_[A-Za-z0-9]{20,}|pypi-[A-Za-z0-9_-]{20,}|npm_[A-Za-z0-9]{20,}|r8_[A-Za-z0-9]{20,}|fc-[A-Za-z0-9]{20,}|bb_live_[A-Za-z0-9_-]{10,}|bu_[A-Za-z0-9_-]{20,}|tvly-[A-Za-z0-9_-]{15,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)
	envAssignRE        = regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL)S?)\s*[=:]\s*["']?([^\s"',;]{6,})`)
)

// Masks the sensitive parameters of a query string
func redactQuery(query string) string {
	parts := strings.Split(query, "&")
	for i, part := range parts {
		name, _, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if sensitiveQueryParams[strings.ToLower(strings.TrimSpace(name))] {
			parts[i] = name + "=***"
		}
	}
	return strings.Join(parts, "&")
}

// RedactCDPURL masks tokens, credential query parameters, and userinfo in a CDP endpoint or error text
func RedactCDPURL(text string) string {
	if text == "" {
		return text
	}
	out := RedactSensitiveText(text)
	out = urlWithQueryRE.ReplaceAllStringFunc(out, func(m string) string {
		sub := urlWithQueryRE.FindStringSubmatch(m)
		return sub[1] + "://" + sub[2] + sub[3] + "?" + redactQuery(sub[4]) + sub[5]
	})
	return urlUserinfoRE.ReplaceAllString(out, "$1://$2:***@")
}

// Masks a vendor prefixed credential to its prefix label only
func maskCredential(token string) string {
	cut := 4
	if idx := strings.IndexAny(token, "-_"); idx > 0 && idx < 12 {
		cut = idx + 1
	}
	if cut > len(token) {
		cut = len(token)
	}
	return "«redacted:" + token[:cut] + "…»"
}

// RedactSensitiveText masks vendor credentials, secret assignments, and every registered vault value
func RedactSensitiveText(text string) string {
	if text == "" {
		return text
	}
	out := credentialPrefixRE.ReplaceAllStringFunc(text, maskCredential)
	out = envAssignRE.ReplaceAllString(out, "$1=***")
	return redactVaultValues(out)
}

// Vault values injected into pages, scrubbed from every later browser result
var (
	vaultValuesMu sync.RWMutex
	vaultValues   = map[string]struct{}{}
)

// RegisterVaultRedactionValue adds a secret the browser results must never echo
func RegisterVaultRedactionValue(value string) {
	if len(strings.TrimSpace(value)) < 4 {
		return
	}
	vaultValuesMu.Lock()
	vaultValues[value] = struct{}{}
	vaultValuesMu.Unlock()
}

// Replaces every registered vault value, longest first, and its URL encoded form
func redactVaultValues(text string) string {
	vaultValuesMu.RLock()
	values := make([]string, 0, len(vaultValues))
	for v := range vaultValues {
		values = append(values, v)
	}
	vaultValuesMu.RUnlock()
	if len(values) == 0 {
		return text
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
		if enc := url.QueryEscape(v); enc != v {
			text = strings.ReplaceAll(text, enc, "[REDACTED]")
		}
	}
	return text
}

// ScrubSecretValues removes every value of a secret payload from text
func ScrubSecretValues(text string, secret map[string]string) string {
	values := make([]string, 0, len(secret))
	for _, v := range secret {
		if len(v) >= 4 {
			values = append(values, v)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
	}
	return text
}

func slogDefault() *slog.Logger { return slog.Default() }
