package config

import (
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Proxy URL variables rewritten to canonical forms, both casings
var proxyEnvKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

// Bypass list variables sanitized for IPv6 entries, both casings
var noProxyEnvKeys = []string{"NO_PROXY", "no_proxy"}

var (
	noProxyBracketedIPv6Re = regexp.MustCompile(`^\[([0-9A-Fa-f:.]+)\](?:[/:].*)?$`)
	noProxyCIDRIPv6Re      = regexp.MustCompile(`^([0-9A-Fa-f:.]+)/\d+$`)
)

// The socks5 form of a socks proxy URL, other values stripped, empty for a blank value
func NormalizeProxyURL(proxyURL string) string {
	candidate := strings.TrimSpace(proxyURL)
	if strings.HasPrefix(strings.ToLower(candidate), "socks://") {
		return "socks5://" + string([]rune(candidate)[len("socks://"):])
	}
	return candidate
}

// Bare literal a bracketed or CIDR IPv6 bypass entry becomes, empty for every other entry
func bareIPv6Literal(entry string) string {
	candidate := strings.TrimSpace(entry)
	m := noProxyBracketedIPv6Re.FindStringSubmatch(candidate)
	if m == nil {
		m = noProxyCIDRIPv6Re.FindStringSubmatch(candidate)
	}
	if m == nil {
		return ""
	}
	ip, err := netip.ParseAddr(m[1])
	if err != nil || ip.Is4() {
		return ""
	}
	return m[1]
}

// Bypass list with bracketed and CIDR IPv6 entries rewritten to bare literals, the input itself when nothing changed
func SanitizeNoProxyEntries(noProxyValue string) string {
	entries := strings.FieldsFunc(strings.TrimSpace(noProxyValue), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(entries) == 0 || slices.Contains(entries, "*") {
		return noProxyValue
	}
	rewritten := make([]string, 0, len(entries))
	for _, entry := range entries {
		bare := bareIPv6Literal(entry)
		if bare == "" {
			rewritten = append(rewritten, entry)
		} else if !slices.Contains(rewritten, bare) {
			rewritten = append(rewritten, bare)
		}
	}
	if slices.Equal(rewritten, entries) {
		return noProxyValue
	}
	return strings.Join(rewritten, ",")
}

// Rewrites the proxy and bypass variables of the process to their canonical forms in place
func NormalizeProxyEnvVars() {
	for _, key := range proxyEnvKeys {
		value := os.Getenv(key)
		if normalized := NormalizeProxyURL(value); normalized != "" && normalized != value {
			os.Setenv(key, normalized)
		}
	}
	for _, key := range noProxyEnvKeys {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if sanitized := SanitizeNoProxyEntries(value); sanitized != value {
			os.Setenv(key, sanitized)
		}
	}
}

// Whether an OpenAI model family rejects max_tokens and needs max_completion_tokens
func ModelForcesMaxCompletionTokens(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	for _, prefix := range []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
