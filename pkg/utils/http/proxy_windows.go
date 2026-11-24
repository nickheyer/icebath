//go:build windows

package httputil

import (
	"errors"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Registry key holding the Internet Settings of the current user
const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// Internet Settings ProxyServer and ProxyOverride of the current user, both ignored unless ProxyEnable is set
func loadSystemSettings() *systemSettings {
	s := &systemSettings{proxies: map[string]string{}}
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return s
	}
	defer k.Close()
	if !registryFlag(k, "ProxyEnable") {
		return s
	}
	if server, _, err := k.GetStringValue("ProxyServer"); err == nil {
		parseProxyServer(server, s.proxies)
	}
	if override, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		for entry := range strings.SplitSeq(override, ";") {
			switch entry = strings.TrimSpace(entry); {
			case entry == "":
			case strings.EqualFold(entry, "<local>"):
				s.bypassSimple = true
			case strings.HasPrefix(entry, "<"):
			default:
				s.exceptions = append(s.exceptions, entry)
			}
		}
	}
	return s
}

// Whether a registry value holds a non zero number, or text other than empty and 0
func registryFlag(k registry.Key, name string) bool {
	n, _, err := k.GetIntegerValue(name)
	if err == nil {
		return n != 0
	}
	if !errors.Is(err, registry.ErrUnexpectedType) {
		return false
	}
	text, _, err := k.GetStringValue(name)
	if err != nil {
		return false
	}
	text = strings.TrimSpace(text)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n != 0
	}
	return text != ""
}

// Proxies of a ProxyServer value: one address for http and https, or protocol=address entries split by semicolons where a bare address serves the protocols left unset and a socks entry the ones still unset after it
func parseProxyServer(server string, out map[string]string) {
	server = strings.TrimSpace(server)
	if server == "" {
		return
	}
	if !strings.ContainsAny(server, "=;") {
		proxy := withProxyScheme(server, "http")
		out["http"], out["https"] = proxy, proxy
		return
	}
	bare, socks := "", ""
	for entry := range strings.SplitSeq(server, ";") {
		protocol, address, ok := strings.Cut(entry, "=")
		if !ok {
			if address = strings.TrimSpace(protocol); address != "" && bare == "" {
				bare = withProxyScheme(address, "http")
			}
			continue
		}
		if address = strings.TrimSpace(address); address == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(protocol)) {
		case "http":
			out["http"] = withProxyScheme(address, "http")
		case "https":
			out["https"] = withProxyScheme(address, "http")
		case "socks":
			// Windows names its SOCKS proxy without a version; Go dials SOCKS5 only, so a bare socks:// scheme is read as that
			if rest, ok := cutPrefixFold(address, "socks://"); ok {
				address = rest
			}
			socks = withProxyScheme(address, "socks5")
		}
	}
	for _, fallback := range [...]string{bare, socks} {
		if fallback == "" {
			continue
		}
		for _, scheme := range [...]string{"http", "https"} {
			if out[scheme] == "" {
				out[scheme] = fallback
			}
		}
	}
}

// s without a leading prefix matched regardless of case, true when it had one
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// Address with the scheme put in front unless it carries one
func withProxyScheme(address, scheme string) string {
	if strings.Contains(address, "://") {
		return address
	}
	return scheme + "://" + address
}
