//go:build windows

package provider

import "time"

// Resolves no value on Windows because the helper contract is a POSIX sh -c command line
func runSecretHelper(command, secretKey string, timeout time.Duration, maxOutput int, environ *SourceEnvironment) (string, bool) {
	commandSecretLog("the 'command' provider is POSIX-only (needs /bin/sh); resolving no value on Windows")
	return "", false
}
