//go:build windows

package config

// Windows has no process umask, so a managed home keeps the inherited ACLs
func setManagedUmask(bool) func() { return func() {} }
