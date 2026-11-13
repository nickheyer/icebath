//go:build unix

package home

// Fixed temp directories a POSIX host falls back to after the temp variables
func platformTempDirs() []string { return []string{"/tmp", "/var/tmp", "/usr/tmp"} }
