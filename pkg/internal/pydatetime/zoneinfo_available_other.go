//go:build !windows

package pydatetime

// POSIX installs carry no tzdata package, so its zone list is empty
func tzdataPackageZones() string { return "" }
