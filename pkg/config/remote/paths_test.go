package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const approvedFixtureDigest = "3fdf70682367f81a455daa8e3894efb5a979064087e6394174206373053fafc4"

var knownFixtureVersions = []float64{1, 2}

func loadVector(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "conformance", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func resolutionFixtures(t *testing.T) []string {
	files, _ := filepath.Glob(filepath.Join("testdata", "conformance", "config-resolution", "*.json"))
	slices.Sort(files)
	return files
}

func TestVendoredFixturesMatchManifestAndSource(t *testing.T) {
	var source map[string]any
	raw, _ := os.ReadFile(filepath.Join("testdata", "SOURCE.json"))
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join("testdata", "conformance", "config-fixtures.sha256")
	if sha256File(t, manifest) != approvedFixtureDigest {
		t.Fatal("vendored manifest is not the approved fixture set")
	}
	if source["fixtureDigest"] != approvedFixtureDigest {
		t.Fatal("SOURCE.json names a different fixture set")
	}
	text, _ := os.ReadFile(manifest)
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(text), "\n"), "\n") {
		fields := strings.Fields(line)
		listed[strings.TrimSpace(strings.TrimPrefix(line, fields[0]))] = fields[0]
	}
	for rel, digest := range listed {
		if sha256File(t, filepath.Join("testdata", rel)) != digest {
			t.Fatal(rel)
		}
	}
	onDisk := map[string]string{}
	filepath.WalkDir(filepath.Join("testdata", "conformance"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			rel, _ := filepath.Rel("testdata", p)
			onDisk[filepath.ToSlash(rel)] = listed[filepath.ToSlash(rel)]
		}
		return nil
	})
	if !reflect.DeepEqual(onDisk, listed) {
		t.Fatal("every vendored fixture is in the manifest and vice versa")
	}
	if len(resolutionFixtures(t)) == 0 {
		t.Fatal("no resolution fixtures")
	}
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

type pathVectors struct {
	Valid []struct {
		Encoded  string   `json:"encoded"`
		Segments []string `json:"segments"`
	} `json:"valid"`
	Invalid []string `json:"invalid"`
	Covers  []struct {
		Lock   string `json:"lock"`
		Path   string `json:"path"`
		Covers bool   `json:"covers"`
	} `json:"covers"`
}

func loadPathVectors(t *testing.T) pathVectors {
	t.Helper()
	var vectors pathVectors
	loadVector(t, "config-paths.json", &vectors)
	if len(vectors.Valid) == 0 || len(vectors.Invalid) == 0 || len(vectors.Covers) == 0 {
		t.Fatalf("config-paths.json holds %d valid, %d invalid and %d coverage vectors", len(vectors.Valid), len(vectors.Invalid), len(vectors.Covers))
	}
	return vectors
}

func TestValidPathsRoundTrip(t *testing.T) {
	for _, v := range loadPathVectors(t).Valid {
		if got := Encode(v.Segments); got != v.Encoded {
			t.Errorf("encode %v = %q, want %q", v.Segments, got, v.Encoded)
		}
		if got, err := Decode(v.Encoded); err != nil || !slices.Equal(got, v.Segments) {
			t.Errorf("decode %q = %v %v, want %v", v.Encoded, got, err, v.Segments)
		}
	}
}

func TestInvalidPathsRejected(t *testing.T) {
	for _, encoded := range loadPathVectors(t).Invalid {
		var perr *PathError
		if _, err := Decode(encoded); !errors.As(err, &perr) {
			t.Errorf("decode %q gave %v, want a PathError", encoded, err)
		}
	}
}

func TestLockCoverage(t *testing.T) {
	for _, v := range loadPathVectors(t).Covers {
		lock, lerr := Decode(v.Lock)
		path, perr := Decode(v.Path)
		if lerr != nil || perr != nil {
			t.Errorf("decode %q %v, %q %v", v.Lock, lerr, v.Path, perr)
			continue
		}
		if got := Covers(lock, path); got != v.Covers {
			t.Errorf("covers(%q, %q) = %v, want %v", v.Lock, v.Path, got, v.Covers)
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
