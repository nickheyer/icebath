//go:build windows

package pyhttpx

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"golang.org/x/sys/windows/registry"
)

// Internet Settings ProxyOverride list when the proxy is enabled, local covering dotless names, as proxy_bypass_registry applies it
func systemProxyBypass(host string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, pyurl.InternetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, enabled, err := pyurl.RegistryValue(k, "ProxyEnable")
	if err != nil {
		return false
	}
	override, _, err := pyurl.RegistryValue(k, "ProxyOverride")
	if err != nil || !enabled || override == "" {
		return false
	}
	host, _ = splitPort(host)
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
