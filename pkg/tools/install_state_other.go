//go:build !windows

package tools

// The registry home exists only on Windows, so no root is named here
func installRegistryHome(func(string) bool) {}
