//go:build !darwin && !windows

package pyurl

// No operating system proxy source outside macOS and Windows
func SystemProxies() map[string]string { return map[string]string{} }
