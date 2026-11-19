package pm

import (
	"archive/tar"
	"path/filepath"
	"testing"
)

func TestAllTargetsIncludesBionic(t *testing.T) {
	count := 0
	for _, target := range AllTargets {
		if target == "linux-arm64-bionic" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("linux-arm64-bionic listed %d times", count)
	}
}

func TestDebRejectsTraversalBeforeTouchingOutside(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "escape")
	writeFile(t, sentinel, []byte("owned outside extraction"), 0o666)
	deb := filepath.Join(tmp, "evil.deb")
	buildDeb(t, deb, []member{{"Package", []byte("evil")}},
		[]*tar.Header{{Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg}},
		map[string][]byte{"../escape": []byte("overwrite")}, "gz")
	dsWantInstallError(t, (&DebPackage{}).Unpack(deb, filepath.Join(tmp, "staged"), "linux-arm64-bionic"), "unsafe|escape|traversal")
	if string(readFile(t, sentinel)) != "owned outside extraction" {
		t.Fatal("sentinel was overwritten")
	}
}
