//go:build windows

package pydatetime

import (
	"time"

	"golang.org/x/sys/windows"
)

// Standard or daylight name of the system time zone the CRT reports, its PST and PDT defaults when unreadable
func localZoneName(t time.Time) string {
	var tzi windows.Timezoneinformation
	if _, err := windows.GetTimeZoneInformation(&tzi); err != nil {
		if t.IsDST() {
			return "PDT"
		}
		return "PST"
	}
	if t.IsDST() {
		return windows.UTF16ToString(tzi.DaylightName[:])
	}
	return windows.UTF16ToString(tzi.StandardName[:])
}
