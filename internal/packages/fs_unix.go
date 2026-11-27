//go:build !windows

package packages

// POSIX has no junctions
func junction(string) bool { return false }
