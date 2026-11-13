//go:build windows

package sysproxy

import (
	"errors"
	"net"
	"net/url"
	"path"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	winhttpAccessTypeNoProxy    = 1
	winhttpAccessTypeNamedProxy = 3
	winhttpAutoproxyAutoDetect  = 0x1
	winhttpAutoproxyConfigURL   = 0x2
	winhttpAutoDetectDHCP       = 0x1
	winhttpAutoDetectDNSA       = 0x2
)

var (
	winhttp            = windows.NewLazySystemDLL("winhttp.dll")
	procIEProxyConfig  = winhttp.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	procOpen           = winhttp.NewProc("WinHttpOpen")
	procGetProxyForURL = winhttp.NewProc("WinHttpGetProxyForUrl")
	procCloseHandle    = winhttp.NewProc("WinHttpCloseHandle")
	procGlobalFree     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

type ieProxyConfig struct {
	autoDetect    int32
	autoConfigURL *uint16
	proxy         *uint16
	proxyBypass   *uint16
}

type autoproxyOptions struct {
	flags                 uint32
	autoDetectFlags       uint32
	autoConfigURL         *uint16
	reserved              uintptr
	reservedFlags         uint32
	autoLogonIfChallenged int32
}

type proxyInfo struct {
	accessType  uint32
	proxy       *uint16
	proxyBypass *uint16
}

// Frees a string WinHTTP allocated with GlobalAlloc and answers its text
func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	text := windows.UTF16PtrToString(p)
	procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
	return text
}

// PAC entries of a WinHTTP proxy list for scheme, scheme scoped entries chosen as Chromium reads them
func manualEntries(list, scheme string) []string {
	var bare, scoped, socks []string
	for _, item := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' }) {
		name, value, scopedItem := strings.Cut(item, "=")
		if !scopedItem {
			bare = append(bare, "PROXY "+stripScheme(item))
			continue
		}
		switch strings.ToLower(name) {
		case scheme:
			scoped = append(scoped, "PROXY "+stripScheme(value))
		case "socks":
			socks = append(socks, "SOCKS "+stripScheme(value))
		}
	}
	switch {
	case len(scoped) > 0:
		return scoped
	case len(bare) > 0:
		return bare
	default:
		return socks
	}
}

func stripScheme(hostPort string) string {
	if _, rest, ok := strings.Cut(hostPort, "://"); ok {
		return strings.TrimSuffix(rest, "/")
	}
	return hostPort
}

// Whether the bypass list of the Internet settings exempts host, local meaning a name without dots
func bypassed(list string, target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	for _, rule := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' }) {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "<local>" {
			if !strings.Contains(host, ".") && net.ParseIP(host) == nil {
				return true
			}
			continue
		}
		rule = stripScheme(rule)
		if ruleHost, _, err := net.SplitHostPort(rule); err == nil {
			rule = ruleHost
		}
		if matched, err := path.Match(rule, host); err == nil && matched {
			return true
		}
	}
	return false
}

// Proxies WinHTTP answers for target under the current user's Internet settings, auto detect and PAC evaluated
func resolve(target *url.URL) (string, error) {
	if err := procIEProxyConfig.Find(); err != nil {
		return "", err
	}
	var config ieProxyConfig
	if ok, _, err := procIEProxyConfig.Call(uintptr(unsafe.Pointer(&config))); ok == 0 {
		return "", err
	}
	autoConfigURL := takeString(config.autoConfigURL)
	manual := takeString(config.proxy)
	bypass := takeString(config.proxyBypass)
	if config.autoDetect != 0 || autoConfigURL != "" {
		if answer, err := autoProxy(target, config.autoDetect != 0, autoConfigURL); err == nil {
			return answer, nil
		}
	}
	if manual == "" || bypassed(bypass, target) {
		return "DIRECT", nil
	}
	return pacAnswer(manualEntries(manual, strings.ToLower(target.Scheme))), nil
}

// Evaluates WPAD or the configured PAC file for target through WinHTTP
func autoProxy(target *url.URL, detect bool, configURL string) (string, error) {
	agent, _ := windows.UTF16PtrFromString("Icebath")
	session, _, err := procOpen.Call(uintptr(unsafe.Pointer(agent)), winhttpAccessTypeNoProxy, 0, 0, 0)
	if session == 0 {
		return "", err
	}
	defer procCloseHandle.Call(session)
	options := autoproxyOptions{autoLogonIfChallenged: 1}
	if detect {
		options.flags |= winhttpAutoproxyAutoDetect
		options.autoDetectFlags = winhttpAutoDetectDHCP | winhttpAutoDetectDNSA
	}
	if configURL != "" {
		options.flags |= winhttpAutoproxyConfigURL
		options.autoConfigURL, _ = windows.UTF16PtrFromString(configURL)
	}
	address, err := windows.UTF16PtrFromString(target.String())
	if err != nil {
		return "", err
	}
	var info proxyInfo
	if ok, _, err := procGetProxyForURL.Call(session, uintptr(unsafe.Pointer(address)), uintptr(unsafe.Pointer(&options)), uintptr(unsafe.Pointer(&info))); ok == 0 {
		return "", err
	}
	list := takeString(info.proxy)
	bypass := takeString(info.proxyBypass)
	if info.accessType != winhttpAccessTypeNamedProxy || list == "" || bypassed(bypass, target) {
		return "DIRECT", nil
	}
	entries := manualEntries(list, strings.ToLower(target.Scheme))
	if len(entries) == 0 {
		return "", errors.New("WinHTTP answered no usable proxy")
	}
	return pacAnswer(entries), nil
}
