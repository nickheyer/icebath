//go:build !windows

package config

import "syscall"

// Group shared umask for a managed home, restored by the returned func
func setManagedUmask(managed bool) func() {
	if !managed {
		return func() {}
	}
	old := syscall.Umask(0o007)
	return func() { syscall.Umask(old) }
}
