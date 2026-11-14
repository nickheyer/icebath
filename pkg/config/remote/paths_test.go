package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var knownFixtureVersions = []float64{1, 2}

func resolutionFixtures(t *testing.T) []string {
	files, _ := filepath.Glob(filepath.Join("testdata", "conformance", "config-resolution", "*.json"))
	slices.Sort(files)
	return files
}

func TestFixtureVersionIsOneThisRunnerReads(t *testing.T) {
	for _, f := range resolutionFixtures(t) {
		var doc map[string]any
		raw, _ := os.ReadFile(f)
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		v, _ := doc["fixtureVersion"].(float64)
		if !slices.Contains(knownFixtureVersions, v) {
			t.Fatalf("%s fixtureVersion %v", f, doc["fixtureVersion"])
		}
	}
}

func TestDottedKeysAndTheFirstCoveringLock(t *testing.T) {
	if !slices.Equal(FromDotted(`a\x.b`), []string{`a\x`, "b"}) || !slices.Equal(FromDotted(`a\.b.c`), []string{"a.b", "c"}) {
		t.Error("dotted keys split")
	}
	locks := []Lock{{Path{"model"}, "org"}, {Path{"model", "default"}, "profile"}}
	if got, ok := CoveringLock(locks, []string{"model", "default"}); !ok || got.Level != "org" {
		t.Errorf("covering lock %v %v", got, ok)
	}
	if _, ok := CoveringLock(locks, []string{"display"}); ok {
		t.Error("uncovered path matched a lock")
	}
}
