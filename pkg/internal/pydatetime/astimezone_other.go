//go:build !windows

package pydatetime

import "time"

// Zone abbreviation localtime reports from the system zone database
func localZoneName(t time.Time) string {
	name, _ := t.Zone()
	return name
}
