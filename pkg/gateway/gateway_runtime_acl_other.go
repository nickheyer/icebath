//go:build !linux && !windows

package gateway

// Mode bits stay the policy where the platform offers no listxattr
func mayHaveAccessACL(path string) bool { return false }
