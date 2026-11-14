package pm

import (
	"archive/tar"
	"encoding/json"
	"path/filepath"
	"strings"
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

func TestTermuxDockerRowPinsDigest(t *testing.T) {
	var lock struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(readFile(t, LockfilePath()), &lock); err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Version   string                       `json:"version"`
		Artifacts map[string]map[string]string `json:"artifacts"`
	}
	if err := json.Unmarshal(lock.Packages["termux-docker"], &pkg); err != nil {
		t.Fatal(err)
	}
	version := pkg.Version
	if !strings.HasPrefix(version, "sha256:") || len(version) != 7+64 {
		t.Fatalf("termux-docker version %q is not a digest", version)
	}
	if url := pkg.Artifacts["linux-arm64-bionic"]["url"]; url != "docker://termux/termux-docker@"+version {
		t.Fatalf("termux-docker row url %q", url)
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
