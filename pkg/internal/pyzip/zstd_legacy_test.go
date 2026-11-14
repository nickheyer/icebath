package pyzip

import (
	"path/filepath"
	"sort"
	"testing"
)

// Members holding real and damaged legacy Zstandard v0.5 to v0.7 frames read, test and extract as CPython 3.14.8 recorded
func TestZstdLegacyFrameMatchesCPython(t *testing.T) {
	expect := loadStreamExpect(t)
	var names []string
	for name := range expect {
		if isLegacyZstdCase(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) != 288 {
		t.Fatalf("%d legacy archives recorded", len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { checkStreamArchive(t, filepath.Join("testdata", "stream", name), expect[name]) })
	}
}
