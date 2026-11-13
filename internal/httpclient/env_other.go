//go:build !windows

package httpclient

// Variable name kept exact since names are case sensitive outside Windows
func envKey(name string) string { return name }
