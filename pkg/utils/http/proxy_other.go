//go:build !darwin && !windows

package httputil

// No operating system proxy settings outside macOS and Windows, where the environment is the only source
func loadSystemSettings() *systemSettings { return &systemSettings{proxies: map[string]string{}} }
