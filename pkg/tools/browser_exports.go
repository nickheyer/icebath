package tools

import "net/netip"

// Url the browser and media tools refuse whatever the private url opt in says, metadata and fake ip floors included
func IsAlwaysBlockedURL(env *Env, raw string) bool { return isAlwaysBlockedURL(env, raw) }

// Url re quoted the way a request client sends it, path query and fragment percent encoded
func NormalizeURLForRequest(raw string) string { return normalizeURLForRequest(raw) }

// Address in the always blocked ranges such as cloud metadata and link local, independent of opt ins
func IsAlwaysBlockedIP(ip netip.Addr) bool { return isAlwaysBlockedIP(ip) }

// Address inside a fake ip range the proxy configuration declares for the env
func IsDeclaredFakeIP(env *Env, ip netip.Addr) bool { return isDeclaredFakeIP(env, ip) }

// Hostname on the cloud metadata blocklist the browser floor refuses before any resolution
func IsBlockedHostname(host string) bool { return blockedHostnames[host] }

// Text carrying a known secret prefix the redaction patterns recognise, the browser url guard input
func RedactPrefixSearch(text string) bool { return prefixSearch(text) }

// Ambiguous authority reason of an http url, the floor form keeping only host moving checks, with the urlsplit ValueError
func AuthorityAmbiguity(raw string, floor bool) (string, error) {
	return authorityAmbiguity(raw, floor)
}

// Host a WHATWG client dials for the url, optionally percent decoded first, with the urlsplit ValueError
func URLHostname(raw string, percentDecode bool) (string, error) {
	return urlHostname(raw, percentDecode)
}

// Cdp endpoint url safe to log, query credentials and userinfo passwords masked
func RedactCDPURL(value string) string { return redactCDPURL(value) }
