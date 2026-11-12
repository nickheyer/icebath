package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestComposerImageTimestampRendersLocalWallClockComponentsNotUtc(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	moment := time.Date(2026, 8, 18, 12, 52, 56, 282e6, time.UTC).In(zone)
	if got := ComposerImageTimestamp(moment); got != "2026-08-18_20-52-56-282" {
		t.Fatalf("got %s", got)
	}
}

func TestComposerImageTimestampZeroPadsEveryComponent(t *testing.T) {
	if got := ComposerImageTimestamp(time.Date(2026, 1, 3, 4, 5, 6, 7e6, time.Local)); got != "2026-01-03_04-05-06-007" {
		t.Fatalf("got %s", got)
	}
}

func TestComposerImageTimestampKeepsTheLegacyFilenameShapeForRealDates(t *testing.T) {
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}-\d{3}$`).MatchString(ComposerImageTimestamp(time.Now())) {
		t.Fatal("shape")
	}
}

func TestLandsThePasteDirectlyUnderIcebathHomeComposerPastesSoTheBackendAdmitsIt(t *testing.T) {
	home := t.TempDir()
	file, err := WriteComposerPaste(home, "pasted body", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(file) != filepath.Join(home, ComposerPastesDirname) {
		t.Fatalf("dir %s", file)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "pasted body" {
		t.Fatalf("body %q", raw)
	}
}

func TestSavedComposerImagesKeepASanitizedNameAndASafeExtension(t *testing.T) {
	s, _ := serviceForTest(t)
	file, err := s.SaveImageBuffer([]byte{1, 2}, json.RawMessage(`"PNG "`), json.RawMessage(`"C:\\shots\\my shot!.final.png"`))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^my_shot_\.final_[0-9a-f]{6}\.png$`).MatchString(filepath.Base(file)) || filepath.Base(filepath.Dir(file)) != "composer-images" {
		t.Fatalf("file %s", file)
	}
	file, _ = s.SaveImageBuffer([]byte{}, json.RawMessage(`"..exe-bad"`), json.RawMessage(`null`))
	if !strings.HasPrefix(filepath.Base(file), "composer_") || !strings.HasSuffix(file, ".png") {
		t.Fatalf("file %s", file)
	}
	if _, err := s.SaveImageBuffer(nil, json.RawMessage(`".png"`), json.RawMessage(`null`)); err == nil || err.Error() != "saveImageBuffer: missing data" {
		t.Fatalf("missing %v", err)
	}
	if _, err := s.SavePastedText(json.RawMessage(`""`)); err == nil || err.Error() != "savePastedText: missing text" {
		t.Fatalf("missing text %v", err)
	}
}

func TestPluginSourceReadsAreCheckedAndAnswerTextSizeAndPath(t *testing.T) {
	s, _ := serviceForTest(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(dir, "index.js")
	os.WriteFile(file, []byte("export default 1\n"), 0o644)
	raw, _ := json.Marshal(file)
	got, err := s.ReadPluginSource(raw)
	if err != nil || jsStringify(got, "") != `{"byteSize":17,"path":`+string(raw)+`,"text":"export default 1\n","truncated":false}` {
		t.Fatalf("got %s %v", jsStringify(got, ""), err)
	}
	if _, err := s.ReadPluginSource(json.RawMessage(`null`)); err == nil || err.Error() != "Plugin source failed: file path is required." {
		t.Fatalf("null %v", err)
	}
}
