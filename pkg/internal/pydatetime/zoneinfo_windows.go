package pydatetime

import (
	"strings"
	"time"
	_ "time/tzdata"
)

// Windows CPython is built without a TZPATH
func platformTZPath() []string { return nil }

// Every IANA key resolves from the embedded tzdata database on Windows
func tzdataPackageZone(key string) (*time.Location, bool) {
	if key == "Local" {
		return nil, false
	}
	loc, err := time.LoadLocation(strings.ReplaceAll(key, "\\", "/"))
	return loc, err == nil
}
