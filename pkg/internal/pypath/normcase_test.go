package pypath

import (
	"runtime"
	"testing"
)

func TestNormcaseMatchesCPython(t *testing.T) {
	if got := NtNormcase(`C:/Repo/Notes.MD`); got != `c:\repo\notes.md` {
		t.Fatalf("ntpath: %q", got)
	}
	if runtime.GOOS != "windows" {
		if got := Normcase("/Repo/Notes.md"); got != "/Repo/Notes.md" {
			t.Fatalf("posixpath: %q", got)
		}
	}
}
