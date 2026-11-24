//go:build windows

package mcp

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"golang.org/x/sys/windows/registry"
)

// proxyBypassSystem applies the Internet Settings ProxyOverride list when the proxy is enabled
func proxyBypassSystem(host string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
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
