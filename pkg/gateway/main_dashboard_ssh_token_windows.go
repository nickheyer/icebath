//go:build windows

package gateway

// Reads and deletes a Desktop SSH token through the protected Windows runtime directory
func readSSHSessionTokenFile(path string) (string, error) { return readWindowsSSHToken(path) }
