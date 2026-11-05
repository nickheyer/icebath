//go:build !windows

package tools

// Gateway Scheduled Tasks exist only on Windows, so no launcher is registered here
func registeredTaskLaunchers() []string {
	return nil
}
