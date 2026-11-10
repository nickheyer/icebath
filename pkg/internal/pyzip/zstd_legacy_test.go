package pyzip

import "testing"

// Members with a legacy Zstandard v0.5 magic go through the libzstd legacy stream decoder as CPython 3.14.8 recorded
func TestZstdLegacyFrameMatchesCPython(t *testing.T) {
	expect := loadStreamExpect(t)
	for name := range legacyZstdCases {
		want, ok := expect[name]
		if !ok {
			t.Fatalf("%s has no recorded result", name)
		}
		t.Run(name, func(t *testing.T) { checkStreamArchive(t, name, want) })
	}
}
