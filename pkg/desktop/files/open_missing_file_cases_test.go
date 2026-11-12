package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAMissingPathThrowsAMissingFileErrorInsteadOfReachingTheOS(t *testing.T) {
	err := AssertExistingPathForOpen(filepath.Join(t.TempDir(), "report.html"), "Open preview in browser")
	var ie *IpcPathError
	if !errors.As(err, &ie) || ie.Code != "missing-file" || !strings.Contains(ie.Message, "does not exist") || !strings.Contains(ie.Message, "another machine") {
		t.Fatal(err)
	}
}

func TestAPathUnderAMissingDirectoryENOTDIRAlsoReadsAsMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plain.txt"), "not a directory")
	if ErrorCode(AssertExistingPathForOpen(filepath.Join(dir, "plain.txt", "nested.txt"), "")) != "missing-file" {
		t.Fatal("ENOTDIR not missing")
	}
}

func TestAnExistingFilePassesTheGuard(t *testing.T) {
	real := filepath.Join(t.TempDir(), "report.html")
	writeFile(t, real, "<html></html>")
	if err := AssertExistingPathForOpen(real, "Open preview in browser"); err != nil {
		t.Fatal(err)
	}
}

func TestANonENOENTStatFailureIsNotConvertedIntoAFabricatedMiss(t *testing.T) {
	real := filepath.Join(t.TempDir(), "locked.txt")
	writeFile(t, real, "x")
	orig := statPath
	statPath = func(p string) (fs.FileInfo, error) {
		if p == real {
			return nil, &os.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return orig(p)
	}
	t.Cleanup(func() { statPath = orig })
	if ErrorCode(AssertExistingPathForOpen(real, "")) != "EACCES" {
		t.Fatal("EACCES fabricated as a miss")
	}
}

func TestThePurposeStringReachesTheUserFacingMessage(t *testing.T) {
	err := AssertExistingPathForOpen(filepath.Join(t.TempDir(), "gone.pdf"), "Open external file")
	if err == nil || !strings.Contains(err.Error(), "Open external file failed") {
		t.Fatal(err)
	}
}
