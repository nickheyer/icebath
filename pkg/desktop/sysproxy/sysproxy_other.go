//go:build (!darwin && !windows && !(freebsd || linux || netbsd)) || ((freebsd || linux || netbsd) && (386 || arm))

package sysproxy

import (
	"net/url"

	"golang.org/x/net/http/httpproxy"
)

// Proxy the environment names for target, the configuration a browser reads where no system resolver exists
func resolve(target *url.URL) (string, error) {
	proxy, err := httpproxy.FromEnvironment().ProxyFunc()(target)
	if err != nil {
		return "", err
	}
	if proxy == nil {
		return "DIRECT", nil
	}
	return pacAnswer([]string{pacEntry(proxy.String())}), nil
}
