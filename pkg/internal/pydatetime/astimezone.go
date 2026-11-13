package pydatetime

import "time"

// Instant in the fixed offset zone a naive local datetime astimezone gives, named the way localtime names it
func Astimezone(t time.Time) time.Time {
	local := t.Local()
	_, offset := local.Zone()
	return t.In(time.FixedZone(localZoneName(local), offset))
}
