//go:build !windows

package browser

// No Windows Shell association query exists on this host
func windowsShellProgID(string) string { return "" }

// No Windows registry exists on this host
func windowsRegistryProgID() string { return "" }
