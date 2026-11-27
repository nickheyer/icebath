//go:build !windows

package tools

// No Windows Shell to ask on this host
func shellProgID(string) string { return "" }

// No Windows registry on this host
func registryProgID() string { return "" }
