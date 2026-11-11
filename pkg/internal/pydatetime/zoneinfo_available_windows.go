package pydatetime

import _ "embed"

//go:embed tzdata_zones.txt
var tzdataZones string

// Zone list of the tzdata 2025.3 package Windows hermes installs
func tzdataPackageZones() string { return tzdataZones }
