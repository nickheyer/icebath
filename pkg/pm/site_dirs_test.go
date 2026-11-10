package pm

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// Relative forms of import path entries under base
func sdRelative(base string, entries []string) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = strings.TrimPrefix(entry, base)
	}
	return out
}

func sdMkdirs(t *testing.T, base string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o777); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSiteDirAddsItsPthDirectoriesLikeAddsitedir(t *testing.T) {
	base := resolvePath(t.TempDir())
	sdMkdirs(t, base, "site/rel", "ed1", "ed2", "ed3", "other")
	ed := func(name string) string { return filepath.Join(base, name) }
	writeFile(t, filepath.Join(base, "site", "a.pth"), []byte("\xef\xbb\xbf# comment\n\nimport os\n"+ed("ed1")+"  \nrel\nmissing\n"+ed("ed1")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "site", "b.pth"), []byte(ed("ed2")+"\r\nimport\tsys\n"), 0o666)
	writeFile(t, filepath.Join(base, "site", ".hidden.pth"), []byte(ed("ed3")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "site", "c.txt"), []byte(ed("ed3")+"\n"), 0o666)
	got, err := addSiteDir([]string{ed("other"), ed("ed2")}, filepath.Join(base, "site"), nil)
	sep := string(filepath.Separator)
	want := []string{sep + "other", sep + "ed2", sep + "site", sep + "ed1", sep + filepath.Join("site", "rel")}
	if err != nil || !reflect.DeepEqual(sdRelative(base, got), want) {
		t.Fatalf("import path %v err %v, want %v", sdRelative(base, got), err, want)
	}
	if again, err := addSiteDir(got, filepath.Join(base, "site"), nil); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("a second pass changed the import path to %v err %v", again, err)
	}
	if got, err := addSiteDir(nil, filepath.Join(base, "absent"), nil); err != nil || !reflect.DeepEqual(got, []string{filepath.Join(base, "absent")}) {
		t.Fatalf("missing site dir %v err %v", got, err)
	}
}

func TestSiteDirDecodesNonUTF8LinesThroughTheLocale(t *testing.T) {
	base := resolvePath(t.TempDir())
	sdMkdirs(t, base, "site", "café")
	writeFile(t, filepath.Join(base, "site", "u.pth"), []byte(filepath.Join(base, "café")+"\n"), 0o666)
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "C.UTF-8")
	sep := string(filepath.Separator)
	got, err := addSiteDir(nil, filepath.Join(base, "site"), nil)
	if err != nil || !reflect.DeepEqual(sdRelative(base, got), []string{sep + "site", sep + "café"}) {
		t.Fatalf("utf-8 line %v err %v", sdRelative(base, got), err)
	}
	writeFile(t, filepath.Join(base, "site", "bad.pth"), []byte("\xff\n"), 0o666)
	t.Setenv("LC_ALL", "C")
	got, err = addSiteDir(nil, filepath.Join(base, "site"), nil)
	want := "'ascii' codec can't decode byte 0xff in position 0: ordinal not in range(128)"
	if runtime.GOOS == "windows" {
		_, decodeErr := pycodecs.Decode([]byte("\xff\n"), pycodecs.GetEncoding())
		if decodeErr == nil {
			if err != nil || !reflect.DeepEqual(sdRelative(base, got), []string{sep + "site", sep + "café"}) {
				t.Fatalf("code page line %v err %v", sdRelative(base, got), err)
			}
			return
		}
		want = decodeErr.Error()
	}
	var py *PyError
	if !errors.As(err, &py) || py.Class != "UnicodeDecodeError" || py.Message != want {
		t.Fatalf("undecodable pth error %#v, want %q", err, want)
	}
	if !reflect.DeepEqual(sdRelative(base, got), []string{sep + "site"}) {
		t.Fatalf("import path before the failure %v", sdRelative(base, got))
	}
}

func TestAdoptedSitePackagesSkipTheNamesThePreviousGenerationHad(t *testing.T) {
	base := resolvePath(t.TempDir())
	sdMkdirs(t, base, "old", "new/x.pth", "e1", "e2", "e3")
	ed := func(name string) string { return filepath.Join(base, name) }
	writeFile(t, filepath.Join(base, "old", "a.pth"), []byte(ed("e1")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "new", "a.pth"), []byte(ed("e1")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "new", "B.pth"), []byte(ed("e2")+"\n"+ed("e3")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "new", ".c.pth"), []byte(ed("e2")+"\n"), 0o666)
	got, err := addAdoptedSitePackages([]string{ed("new")}, ed("old"), ed("new"))
	sep := string(filepath.Separator)
	want := []string{sep + "new", sep + "e2", sep + "e3"}
	if err != nil || !reflect.DeepEqual(sdRelative(base, got), want) {
		t.Fatalf("adopted import path %v err %v, want %v", sdRelative(base, got), err, want)
	}
}
