package sysproxy

import (
	"net/url"
	"strings"
)

// Proxy answer of the system configuration for rawURL in PAC form, PROXY host port entries or DIRECT
func Resolve(rawURL string) (string, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return resolve(target)
}

// PAC entry of one proxy URI the platform resolver answers, empty for a scheme PAC has no word for
func pacEntry(uri string) string {
	if uri == "direct://" {
		return "DIRECT"
	}
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Host == "" {
		return ""
	}
	kind := map[string]string{"http": "PROXY", "https": "HTTPS", "socks": "SOCKS", "socks4": "SOCKS4", "socks4a": "SOCKS4", "socks5": "SOCKS5", "socks5h": "SOCKS5"}[strings.ToLower(parsed.Scheme)]
	if kind == "" {
		return ""
	}
	return kind + " " + parsed.Host
}

// Joins PAC entries the way a PAC script answers, DIRECT when none is left
func pacAnswer(entries []string) string {
	var kept []string
	for _, entry := range entries {
		if entry != "" {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		return "DIRECT"
	}
	return strings.Join(kept, "; ")
}
