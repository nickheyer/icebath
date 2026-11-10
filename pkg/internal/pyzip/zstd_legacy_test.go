package pyzip

import "testing"

// A legacy Zstandard v0.5 frame decodes through the libzstd legacy stream decoder as CPython 3.14.8 recorded
func TestZstdLegacyFrameMatchesCPython(t *testing.T) {
	expect := loadStreamExpect(t)
	checkStreamArchive(t, legacyZstdCase, expect[legacyZstdCase])
}
