//go:build !windows

package pymimetypes

const hostWindows = false

// Hosts without a registry contribute no types
func readWindowsRegistry(*DB) {}
