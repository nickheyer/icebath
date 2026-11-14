package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes a valid image marker into dir and returns its path
func validImageMarker(t *testing.T, dir string) string {
	t.Helper()
	marker := filepath.Join(dir, "image-provenance.json")
	raw, _ := json.Marshal(map[string]any{"schema": 1, "deployment_kind": "image", "manager": "docker", "image": "nousresearch/icebath-agent",
		"version": "1.0.0", "revision": strings.Repeat("a", 40)})
	if err := os.WriteFile(marker, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestReaderAbsentMarkerMeansNone(t *testing.T) {
	if ReadImageProvenance(filepath.Join(t.TempDir(), "nope.json")) != nil {
		t.Fatal("absent marker read as provenance")
	}
}

func TestReaderValidMarker(t *testing.T) {
	provenance := ReadImageProvenance(validImageMarker(t, t.TempDir()))
	if provenance == nil || !provenance.Valid || provenance.Manager != "docker" || provenance.Version == nil || *provenance.Version != "1.0.0" {
		t.Fatalf("provenance %+v", provenance)
	}
	if text, _ := json.Marshal(provenance.ToDict().Vals); !strings.Contains(string(text), `"deployment_kind":"image"`) {
		t.Fatalf("dict %s", text)
	}
	if got := provenance.ToDict().Keys; strings.Join(got, ",") != "schema,deployment_kind,manager,image,version,revision,marker_path,valid,error" {
		t.Fatalf("field order %v", got)
	}
}

func TestReaderFailsClosedOnMalformed(t *testing.T) {
	object := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	for _, c := range []struct{ payload, reasonPrefix string }{
		{"not json {", "marker_unreadable"},
		{"[1, 2]", "marker_not_object"},
		{object(map[string]any{"schema": true, "deployment_kind": "image", "manager": "docker"}), "unsupported_marker_schema"},
		{object(map[string]any{"schema": 2, "deployment_kind": "image", "manager": "docker"}), "unsupported_marker_schema"},
		{object(map[string]any{"schema": 1, "deployment_kind": "source", "manager": "docker"}), "invalid_deployment_kind"},
		{object(map[string]any{"schema": 1, "deployment_kind": "image", "manager": "  "}), "missing_manager"},
		{object(map[string]any{"schema": 1, "deployment_kind": "image", "manager": "docker", "version": 3}), "invalid_version"},
		{"\xff\xfe", "marker_unreadable:UnicodeDecodeError"},
	} {
		marker := filepath.Join(t.TempDir(), "image-provenance.json")
		if err := os.WriteFile(marker, []byte(c.payload), 0o644); err != nil {
			t.Fatal(err)
		}
		provenance := ReadImageProvenance(marker)
		if provenance == nil || provenance.Valid || provenance.Error == nil || !strings.HasPrefix(*provenance.Error, c.reasonPrefix) {
			t.Errorf("payload %q gave %+v", c.payload, provenance)
		}
	}
}

func TestReaderRejectsSymlinkMarker(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(validImageMarker(t, dir), link); err != nil {
		t.Fatal(err)
	}
	provenance := ReadImageProvenance(link)
	if provenance == nil || provenance.Valid || provenance.Error == nil || *provenance.Error != "marker_not_regular_file" {
		t.Fatalf("provenance %+v", provenance)
	}
	if got := ReadImageProvenance(dir); got == nil || got.Valid || *got.Error != "marker_not_regular_file" {
		t.Fatalf("directory marker %+v", got)
	}
}
