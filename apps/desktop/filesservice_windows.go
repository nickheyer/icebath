//go:build windows

package main

import "golang.org/x/sys/windows/registry"

// ICEBATH_HOME of the user environment in HKCU Environment, expanded, empty when unset
func windowsUserHome() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, kind, err := k.GetStringValue("ICEBATH_HOME")
	if err != nil {
		return ""
	}
	if kind == registry.EXPAND_SZ {
		if expanded, err := registry.ExpandString(v); err == nil {
			return expanded
		}
	}
	return v
}

// WebView2 posts dropped files from the bridge itself, nothing native to wire
func nativeInstallFileDrop(any) {}

// IFileSaveDialog asks before replacing a file on its own
func nativeSaveNeedsOverwritePrompt() bool { return false }
