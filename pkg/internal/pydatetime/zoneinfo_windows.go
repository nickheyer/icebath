package pydatetime

import (
	"strings"
	"time"
	_ "time/tzdata"
)

// Windows CPython is built without a TZPATH
func platformTZPath() []string { return nil }

// Windows ships the tzdata package with hermes, every IANA key resolves from the embedded database
func tzdataPackageZone(key string) (*time.Location, bool) {
	if key == "Local" {
		return nil, false
	}
	loc, err := time.LoadLocation(strings.ReplaceAll(key, "\\", "/"))
	return loc, err == nil
}
