//go:build !windows

package pydatetime

import "time"

// Configure default TZPATH of a POSIX CPython build
func platformTZPath() []string { return defaultTZPath }

// POSIX installs carry no tzdata package, so a key missing from TZPATH is not found
func tzdataPackageZone(key string) (*time.Location, bool) { return nil, false }
