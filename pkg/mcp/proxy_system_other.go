//go:build !darwin && !windows

package mcp

// getproxiesSystem has no operating system proxy source outside macOS and Windows
func getproxiesSystem() map[string]string { return map[string]string{} }

// proxyBypassSystem has no operating system bypass list outside macOS and Windows
func proxyBypassSystem(string) bool { return false }
