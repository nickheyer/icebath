//go:build !darwin && !windows

package mcp

// proxyBypassSystem has no operating system bypass list outside macOS and Windows
func proxyBypassSystem(string) bool { return false }
