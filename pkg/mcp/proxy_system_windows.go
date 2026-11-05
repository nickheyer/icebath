//go:build windows

package mcp

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// registryValue reads a value with Python winreg truthiness and str conversion
func registryValue(k registry.Key, name string) (text string, truthy bool, err error) {
	if n, _, ierr := k.GetIntegerValue(name); ierr == nil {
		return strconv.FormatUint(n, 10), n != 0, nil
	} else if !errors.Is(ierr, registry.ErrUnexpectedType) {
		return "", false, ierr
	}
	if s, _, serr := k.GetStringValue(name); serr == nil {
		return s, s != "", nil
	}
	if ss, _, merr := k.GetStringsValue(name); merr == nil {
		return "['" + strings.Join(ss, "', '") + "']", len(ss) > 0, nil
	}
	b, _, berr := k.GetBinaryValue(name)
	if berr != nil {
		return "", false, berr
	}
	return pyBytesRepr(b), len(b) > 0, nil
}

// pyBytesRepr renders bytes the way Python str of a bytes object does
func pyBytesRepr(b []byte) string {
	var sb strings.Builder
	sb.WriteString("b'")
	for _, c := range b {
		switch {
		case c == '\\' || c == '\'':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c == '\t':
			sb.WriteString(`\t`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c < 0x20 || c >= 0x7f:
			sb.WriteString(`\x` + strconv.FormatUint(uint64(c)|0x100, 16)[1:])
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('\'')
	return sb.String()
}

var schemePrefixRE = regexp.MustCompile(`^(?:[^/:]+)://`)

// getproxiesSystem reads the Internet Settings ProxyServer when ProxyEnable is set, like getproxies_registry
func getproxiesSystem() map[string]string {
	proxies := map[string]string{}
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return proxies
	}
	defer k.Close()
	_, enabled, err := registryValue(k, "ProxyEnable")
	if err != nil || !enabled {
		return proxies
	}
	proxyServer, _, err := registryValue(k, "ProxyServer")
	if err != nil {
		return proxies
	}
	if !strings.Contains(proxyServer, "=") && !strings.Contains(proxyServer, ";") {
		proxyServer = "http=" + proxyServer + ";https=" + proxyServer + ";ftp=" + proxyServer
	}
	for _, p := range strings.Split(proxyServer, ";") {
		protocol, address, ok := strings.Cut(p, "=")
		if !ok {
			return proxies
		}
		if !schemePrefixRE.MatchString(address) {
			switch protocol {
			case "http", "https", "ftp":
				address = "http://" + address
			case "socks":
				address = "socks://" + address
			}
		}
		proxies[protocol] = address
	}
	if socks := proxies["socks"]; socks != "" {
		address := socks
		if strings.HasPrefix(address, "socks://") {
			address = "socks4://" + address[len("socks://"):]
		}
		if proxies["http"] == "" {
			proxies["http"] = address
		}
		if proxies["https"] == "" {
			proxies["https"] = address
		}
	}
	return proxies
}

// proxyBypassSystem applies the Internet Settings ProxyOverride list when the proxy is enabled
func proxyBypassSystem(host string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, enabled, err := registryValue(k, "ProxyEnable")
	if err != nil {
		return false
	}
	override, _, err := registryValue(k, "ProxyOverride")
	if err != nil || !enabled || override == "" {
		return false
	}
	return winregOverrideBypass(host, override)
}

// winregOverrideBypass matches host against the semicolon list, local covering dotless names
func winregOverrideBypass(host, override string) bool {
	host, _ = pySplitPort(host)
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "/", `\`) }
	for _, test := range strings.Split(override, ";") {
		test = strings.TrimSpace(test)
		if test == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
		} else if fnmatch.FnmatchCase(norm(host), norm(test)) {
			return true
		}
	}
	return false
}
