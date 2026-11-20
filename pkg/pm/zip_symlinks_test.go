package pm

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	zsIFLNK = 0o120000
	zsIFREG = 0o100000
)

// One zip entry with its raw unix mode, the high half of external_attr
type zsEntry struct {
	name string
	data string
	mode uint32
}

// Writes a stored zip of entries the way zipfile writestr encodes them
func zsZip(t *testing.T, path string, entries []zsEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store, ExternalAttrs: e.mode << 16, CreatorVersion: 3<<8 | 20}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buf.Bytes(), 0o666)
	return path
}

// Symlink entry whose content is the target path
func zsLink(member, target string) zsEntry { return zsEntry{member, target, zsIFLNK | 0o755} }

// Regular file entry with a permission mode
func zsFile(member, data string, mode uint32) zsEntry { return zsEntry{member, data, zsIFREG | mode} }

// Reports whether path is a symlink without following it
func zsIsSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// Reports whether anything, even a dangling link, sits at path
func zsLexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestFrameworkShapedLinksRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{
		zsFile("app/F.framework/Versions/A/F", "machO", 0o755),
		zsFile("app/F.framework/Versions/A/Resources/Info.plist", "<plist/>", 0o644),
		zsLink("app/F.framework/Versions/Current", "A"),
		zsLink("app/F.framework/Resources", "Versions/Current/Resources"),
		zsLink("app/F.framework/F", "Versions/Current/F"),
	})
	dest := filepath.Join(tmp, "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	framework := filepath.Join(dest, "app", "F.framework")
	current := filepath.Join(framework, "Versions", "Current")
	if target, err := os.Readlink(current); !zsIsSymlink(current) || err != nil || target != "A" {
		t.Fatalf("Versions/Current = %q %v", target, err)
	}
	resources := filepath.Join(framework, "Resources")
	if !zsIsSymlink(resources) {
		t.Fatal("Resources is not a symlink")
	}
	if got := string(readFile(t, filepath.Join(resources, "Info.plist"))); got != "<plist/>" {
		t.Fatalf("Info.plist through the link = %q", got)
	}
	if got := string(readFile(t, filepath.Join(framework, "F"))); got != "machO" {
		t.Fatalf("F through the link = %q", got)
	}
	if st, err := os.Stat(filepath.Join(framework, "Versions", "A", "F")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("binary lost its exec bit: %v", err)
	}
}

func TestIntreeDotdotTargetIsKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{
		zsFile("share/data.txt", "d", 0o644), zsLink("lib/data", "../share/data.txt"),
	})
	dest := filepath.Join(tmp, "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dest, "lib", "data")
	if !zsIsSymlink(link) || string(readFile(t, link)) != "d" {
		t.Fatal("in tree dot dot link was not kept")
	}
}

func TestHostileLinksAreSkippedNotWritten(t *testing.T) {
	for _, tc := range []struct{ member, target string }{
		{"lib/evil", "../../../victim"}, {"lib/evil", "/etc/passwd"}, {"../evil", "whatever"},
	} {
		t.Run(tc.member+" "+tc.target, func(t *testing.T) {
			tmp := t.TempDir()
			archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{zsFile("lib/ok.txt", "ok", 0o644), zsLink(tc.member, tc.target)})
			dest := filepath.Join(tmp, "deep", "out")
			if err := Extract(archive, dest); err != nil {
				t.Fatal(err)
			}
			if got := string(readFile(t, filepath.Join(dest, "lib", "ok.txt"))); got != "ok" {
				t.Fatalf("good entry = %q", got)
			}
			if zsLexists(filepath.Join(dest, "lib", "evil")) {
				t.Fatal("hostile link was written under dest")
			}
			for _, outside := range []string{"victim", "evil"} {
				if zsLexists(filepath.Join(tmp, outside)) {
					t.Fatalf("%s exists outside dest", outside)
				}
			}
		})
	}
}

func TestFileWrittenThroughEarlierLinkEntryCannotEscape(t *testing.T) {
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{zsLink("outdir", "../.."), zsFile("outdir/pwned.txt", "pwned", 0o644)})
	dest := filepath.Join(tmp, "deep", "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	if zsLexists(filepath.Join(tmp, "pwned.txt")) {
		t.Fatal("file escaped through the link entry")
	}
	written := filepath.Join(dest, "outdir", "pwned.txt")
	if st, err := os.Stat(written); err != nil || !st.Mode().IsRegular() || zsIsSymlink(filepath.Join(dest, "outdir")) {
		t.Fatalf("file did not land as a real path: %v", err)
	}
}

func TestWin32SafeLinkDegradesWithoutEscaping(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{
		zsFile("share/data.txt", "d", 0o644), zsLink("lib/data", "../share/data.txt"),
	})
	dest := filepath.Join(tmp, "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dest, "lib", "data")
	if zsIsSymlink(link) {
		if target, err := os.Readlink(link); err != nil || filepath.Clean(target) != filepath.Clean("../share/data.txt") {
			t.Fatalf("link target = %q %v", target, err)
		}
	} else if string(readFile(t, link)) != "../share/data.txt" {
		t.Fatal("placeholder does not hold the target path")
	}
	entries, _ := os.ReadDir(filepath.Join(dest, "lib"))
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("lib holds %v", entries)
	}
}

func TestPlainZipUnchanged(t *testing.T) {
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "a.zip"), []zsEntry{zsFile("bin/tool", "#!", 0o755), zsFile("README", "r", 0o644)})
	dest := filepath.Join(tmp, "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dest, "bin", "tool")); err != nil || st.Mode()&0o111 == 0 {
			t.Fatalf("tool lost its exec bit: %v", err)
		}
	}
	if string(readFile(t, filepath.Join(dest, "bin", "tool"))) != "#!" || string(readFile(t, filepath.Join(dest, "README"))) != "r" {
		t.Fatal("plain entries changed")
	}
}
