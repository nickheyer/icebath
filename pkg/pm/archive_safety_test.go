package pm

import (
	"archive/tar"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Writes a gzipped tar of arbitrary headers with their payloads
func asTar(t *testing.T, path string, headers []*tar.Header, data map[string][]byte) string {
	t.Helper()
	writeFile(t, path, makeTarHeaders(t, headers, data, true), 0o666)
	return path
}

// Zeroes the held retry backoff for the test the way the Python fixture does
func asInstantBackoff(t *testing.T) {
	saved := heldRetryDelays
	heldRetryDelays = make([]time.Duration, len(saved))
	t.Cleanup(func() { heldRetryDelays = saved })
}

// Replaces the flatten rename so moving a bin entry fails failures times with err, counting each bin rename
func asHeldRename(t *testing.T, err error, failures int) *int {
	calls := new(int)
	saved := renameFile
	renameFile = func(source, target string) error {
		if filepath.Base(source) == "bin" {
			*calls++
			if failures < 0 || *calls <= failures {
				return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
			}
		}
		return saved(source, target)
	}
	t.Cleanup(func() { renameFile = saved })
	return calls
}

// Extracts an archive into dest failing the test on error
func asExtract(t *testing.T, archive, dest string) {
	t.Helper()
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
}

// Reports whether path holds a regular file
func asIsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func TestZipEntriesCannotEscapeTheDestination(t *testing.T) {
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "slip.zip"), []zsEntry{{"../../ESCAPED.txt", "pwned", 0o600}, {"bin/tool", "fine", 0o600}})
	dest := filepath.Join(tmp, "sub", "dest")
	asExtract(t, archive, dest)
	if zsLexists(filepath.Join(tmp, "ESCAPED.txt")) || zsLexists(filepath.Join(filepath.Dir(tmp), "ESCAPED.txt")) {
		t.Fatal("zip entry escaped the destination")
	}
	if !asIsFile(filepath.Join(dest, "bin", "tool")) {
		t.Fatal("bin/tool missing")
	}
}

func TestAbsoluteZipPathsStayInsideTheDestination(t *testing.T) {
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "abs.zip"), []zsEntry{{"/etc/PWNED.txt", "pwned", 0o600}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if zsLexists("/etc/PWNED.txt") {
		t.Fatal("absolute zip path was written")
	}
	if !asIsFile(filepath.Join(dest, "etc", "PWNED.txt")) {
		t.Fatal("absolute zip path did not land inside dest")
	}
}

func TestTarEntriesCannotEscapeTheDestination(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "slip.tar.gz"), []member{{"../../ESCAPED.txt", []byte("pwned")}})
	if err := Extract(archive, filepath.Join(tmp, "sub", "dest")); err == nil {
		t.Fatal("escaping tar entry was accepted")
	}
	if zsLexists(filepath.Join(filepath.Dir(tmp), "ESCAPED.txt")) {
		t.Fatal("tar entry escaped the destination")
	}
}

func TestAbsoluteTarPathsCannotEscape(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "abs.tar.gz"), []member{{"/tmp/PWNED-pm-store.txt", []byte("pwned")}})
	dest := filepath.Join(tmp, "dest")
	_ = Extract(archive, dest)
	if zsLexists("/tmp/PWNED-pm-store.txt") {
		t.Fatal("absolute tar path was written")
	}
	var outside []string
	filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "PWNED-pm-store.txt" && !strings.HasPrefix(path, dest+string(filepath.Separator)) {
			outside = append(outside, path)
		}
		return nil
	})
	if len(outside) != 0 {
		t.Fatalf("absolute tar path landed outside dest at %v", outside)
	}
}

func TestATarSymlinkCannotBeUsedToWriteOutside(t *testing.T) {
	tmp := t.TempDir()
	victim := filepath.Join(tmp, "outside.txt")
	writeFile(t, victim, []byte("ORIGINAL"), 0o666)
	archive := asTar(t, filepath.Join(tmp, "symlink.tar.gz"), []*tar.Header{
		{Name: "escape", Mode: 0o644, Typeflag: tar.TypeSymlink, Linkname: "../../outside.txt"},
		{Name: "escape", Mode: 0o644, Typeflag: tar.TypeReg},
	}, map[string][]byte{"escape": []byte("OVERWRITTEN")})
	if err := Extract(archive, filepath.Join(tmp, "sub", "dest")); err == nil {
		t.Fatal("escaping symlink was accepted")
	}
	if string(readFile(t, victim)) != "ORIGINAL" {
		t.Fatal("victim was overwritten")
	}
}

func TestATraversingEntryCannotTouchAFileOutside(t *testing.T) {
	tmp := t.TempDir()
	victim := filepath.Join(tmp, "victim.txt")
	writeFile(t, victim, []byte("untouched"), 0o666)
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	archive := zsZip(t, filepath.Join(tmp, "chmod.zip"), []zsEntry{{"../../victim.txt", "x", 0o777}})
	asExtract(t, archive, filepath.Join(tmp, "sub", "dest"))
	if string(readFile(t, victim)) != "untouched" {
		t.Fatal("victim content changed")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(victim); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("victim mode changed: %v", err)
		}
	}
}

func TestTheExecutableBitIsStillRestoredForRealEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "exec.zip"), []zsEntry{{"uv", "#!/bin/sh\n", 0o755}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if st, err := os.Stat(filepath.Join(dest, "uv")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("exec bit was not restored: %v", err)
	}
}

func TestDuplicateEntryNamesStayInsideDest(t *testing.T) {
	tmp := t.TempDir()
	archive := zsZip(t, filepath.Join(tmp, "dupe.zip"), []zsEntry{{"bin/tool", "first", 0o600}, {"bin/tool", "second", 0o600}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if got := string(readFile(t, filepath.Join(dest, "bin", "tool"))); got != "second" {
		t.Fatalf("bin/tool = %q", got)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 || entries[0].Name() != "bin" {
		t.Fatalf("dest holds %v", entries)
	}
}

func TestAHiddenTopLevelFileIsNotReplacedByFlatten(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "hidden.tar.gz"), []member{
		{".config", []byte("TOP LEVEL ORIGINAL")}, {"wrapper/.config", []byte("FROM WRAPPER")}, {"wrapper/bin/tool", []byte("x")},
	})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(dest, ".config"))) != "TOP LEVEL ORIGINAL" {
		t.Fatal("outer dotfile was replaced")
	}
}

func TestAChildNamedLikeItsWrapperRefusesRatherThanClobbers(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "same.tar.gz"), []member{{"gh/gh", []byte("inner")}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(dest, "gh", "gh"))) != "inner" {
		t.Fatal("wrapper child was clobbered")
	}
}

func TestARealWrapperStillUnwraps(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "wrapped.tar.gz"), []member{{"gh_2.97.0_linux_amd64/bin/gh", []byte("x")}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if !asIsFile(filepath.Join(dest, "bin", "gh")) {
		t.Fatal("wrapper was not hoisted")
	}
}

func TestALoneLayoutDirIsLeftAlone(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "flat.tar.gz"), []member{{"bin/gh", []byte("x")}})
	dest := filepath.Join(tmp, "dest")
	asExtract(t, archive, dest)
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if !asIsFile(filepath.Join(dest, "bin", "gh")) {
		t.Fatal("layout dir was hoisted")
	}
}

func TestTheHoistWaitsOutAScannerHold(t *testing.T) {
	asInstantBackoff(t)
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "dest")
	asExtract(t, stTarGz(t, filepath.Join(tmp, "wrapped.tar.gz"), []member{{"pkg/bin/tool", []byte("x")}, {"pkg/README.md", []byte("docs")}}), dest)
	calls := asHeldRename(t, syscall.EACCES, 2)
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatalf("bin rename attempted %d times", *calls)
	}
	if !asIsFile(filepath.Join(dest, "bin", "tool")) || !asIsFile(filepath.Join(dest, "README.md")) {
		t.Fatal("hoist did not complete")
	}
}

func TestARefusalWaitingCannotFixStillRaises(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  syscall.Errno
	}{{"permission", syscall.EACCES}, {"cross-device", syscall.EXDEV}} {
		t.Run(tc.name, func(t *testing.T) {
			asInstantBackoff(t)
			tmp := t.TempDir()
			dest := filepath.Join(tmp, "dest")
			asExtract(t, stTarGz(t, filepath.Join(tmp, "wrapped.tar.gz"), []member{{"pkg/bin/tool", []byte("x")}}), dest)
			calls := asHeldRename(t, tc.err, -1)
			if err := FlattenSingleDir(dest); !errors.Is(err, tc.err) {
				t.Fatalf("expected %v, got %v", tc.err, err)
			}
			want := len(heldRetryDelays) + 1
			if tc.err == syscall.EXDEV {
				want = 1
			}
			if *calls != want {
				t.Fatalf("bin rename attempted %d times, want %d", *calls, want)
			}
		})
	}
}

func TestAHeldDuplicateMetadataUnlinkIsRetried(t *testing.T) {
	asInstantBackoff(t)
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "dest")
	asExtract(t, stTarGz(t, filepath.Join(tmp, "meta.tar.gz"), []member{
		{".DS_Store", []byte("outer")}, {"pkg/.DS_Store", []byte("inner")}, {"pkg/bin/tool", []byte("x")},
	}), dest)
	var calls []string
	saved := unlinkFile
	unlinkFile = func(path string) error {
		if filepath.Base(path) == ".DS_Store" {
			calls = append(calls, path)
			if len(calls) == 1 {
				return &fs.PathError{Op: "unlink", Path: path, Err: syscall.EACCES}
			}
		}
		return saved(path)
	}
	t.Cleanup(func() { unlinkFile = saved })
	if err := FlattenSingleDir(dest); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("metadata unlink attempted %d times", len(calls))
	}
	if string(readFile(t, filepath.Join(dest, ".DS_Store"))) != "inner" || !asIsFile(filepath.Join(dest, "bin", "tool")) {
		t.Fatal("hoist did not replace the duplicate metadata")
	}
}

func TestExtractReplacesOnlyItsOwnEntryDirectory(t *testing.T) {
	tmp := t.TempDir()
	store := filepath.Join(tmp, "store")
	writeFile(t, filepath.Join(store, "node-abc", "bin", "node"), []byte("other entry"), 0o666)
	keep := filepath.Join(store, "downloads", "important.bin")
	writeFile(t, keep, []byte("KEEP"), 0o666)
	archive := stTarGz(t, filepath.Join(tmp, "gh.tar.gz"), []member{{"bin/gh", []byte("x")}})
	asExtract(t, archive, filepath.Join(store, "gh-def"))
	if string(readFile(t, filepath.Join(store, "node-abc", "bin", "node"))) != "other entry" || string(readFile(t, keep)) != "KEEP" {
		t.Fatal("extraction touched a neighbouring entry")
	}
}

func TestRestagingClearsAStaleTreeFirst(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "gh")
	writeFile(t, filepath.Join(dest, "STALE"), []byte("from an older version"), 0o666)
	archive := stTarGz(t, filepath.Join(tmp, "gh.tar.gz"), []member{{"bin/gh", []byte("x")}})
	asExtract(t, archive, dest)
	if zsLexists(filepath.Join(dest, "STALE")) {
		t.Fatal("stale file survived the restage")
	}
	if !asIsFile(filepath.Join(dest, "bin", "gh")) {
		t.Fatal("new layout missing")
	}
}

func TestUnsupportedArchiveNamesAreRefused(t *testing.T) {
	tmp := t.TempDir()
	weird := filepath.Join(tmp, "tool.rar")
	writeFile(t, weird, []byte("not really"), 0o666)
	if err := Extract(weird, filepath.Join(tmp, "dest")); err == nil || !strings.Contains(err.Error(), "unsupported archive") {
		t.Fatalf("expected unsupported archive, got %v", err)
	}
}
