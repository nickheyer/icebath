//go:build windows

package pyurl

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Registry key holding the Internet Settings proxy values of the current user
const InternetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// Registry value with Python winreg truthiness and str conversion
func RegistryValue(k registry.Key, name string) (text string, truthy bool, err error) {
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

// Internet Settings ProxyServer entries when ProxyEnable is set, the way getproxies_registry reads them
func SystemProxies() map[string]string {
	proxies := map[string]string{}
	k, err := registry.OpenKey(registry.CURRENT_USER, InternetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return proxies
	}
	defer k.Close()
	_, enabled, err := RegistryValue(k, "ProxyEnable")
	if err != nil || !enabled {
		return proxies
	}
	proxyServer, _, err := RegistryValue(k, "ProxyServer")
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
