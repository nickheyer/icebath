package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateBinaryReplacesTheTreeBinaryWithTheBranchRelease(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	goos, goarch := hostKey()
	name := BinaryAssetName(goos, goarch)
	f := newReleaseFixture(t, []fixtureRelease{
		{Tag: "v1.1.0", Target: "main", Published: "2026-10-01T00:00:00Z", Assets: map[string][]byte{name: fixtureBinary(t, "v1.1.0")}},
		{Tag: "v1.2.0", Target: "main", Published: "2026-11-01T00:00:00Z", Assets: map[string][]byte{name: fixtureBinary(t, "v1.2.0")}},
	}, nil)
	dir := t.TempDir()
	if err := os.WriteFile(InstallBinaryPath(dir), fixtureBinary(t, "v1.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstallMethod(dir, MethodScript); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got, err := UpdateBinary(context.Background(), dir, f.repo, "", "main", &out)
	if err != nil || !got.Changed || got.Tag != "v1.2.0" {
		t.Fatalf("update %+v %v\n%s", got, err, out.String())
	}
	if InstalledRelease(context.Background(), InstallBinaryPath(dir)).Tag != "v1.2.0" {
		t.Fatal("binary not replaced")
	}
	if stamp, _ := os.ReadFile(filepath.Join(dir, InstallMethodFile)); string(stamp) != "script\n" {
		t.Fatalf("stamp %q", stamp)
	}
	if _, err := os.Stat(InstallBinaryPath(dir) + ".download"); !os.IsNotExist(err) {
		t.Fatal("staged download left behind")
	}
	out.Reset()
	again, err := UpdateBinary(context.Background(), dir, f.repo, "", "main", &out)
	if err != nil || again.Changed || again.Tag != "v1.2.0" || out.Len() != 0 {
		t.Fatalf("second update %+v %v %q", again, err, out.String())
	}
	if _, err := UpdateBinary(context.Background(), dir, f.repo, "", "feature", &out); err == nil || !strings.Contains(err.Error(), "follows branch feature") {
		t.Fatalf("unknown branch %v", err)
	}
}

func TestUpdateBinaryRefusesAReleaseWithoutAChecksum(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := newReleaseFixture(t, []fixtureRelease{
		{Tag: "v1.2.0", Target: "main", Published: "2026-11-01T00:00:00Z", Assets: map[string][]byte{"icebath-plan9-amd64": []byte("other")}},
	}, nil)
	dir := t.TempDir()
	before := fixtureBinary(t, "v1.1.0")
	if err := os.WriteFile(InstallBinaryPath(dir), before, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateBinary(context.Background(), dir, f.repo, "", "main", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "lists no checksum") {
		t.Fatalf("a release without a checksum for this host must not install: %v", err)
	}
	if now, _ := os.ReadFile(InstallBinaryPath(dir)); !bytes.Equal(now, before) {
		t.Fatal("installed binary changed")
	}
}

func TestInstallLadderStampsItsMethodBesideTheBinary(t *testing.T) {
	homeDir, dir := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	binary := filepath.Join(t.TempDir(), "icebath")
	if err := os.WriteFile(binary, fixtureBinary(t, "v1.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{MethodScript, MethodSetupApp} {
		r, err := NewRunner(Options{Dir: dir, Binary: binary, Version: "v1.2.0", InstallMethod: method}, func(Event) {})
		if err != nil {
			t.Fatal(err)
		}
		r.stdout, r.stderr = &bytes.Buffer{}, &bytes.Buffer{}
		if err := r.stageRepository(context.Background()); err != nil {
			t.Fatal(err)
		}
		if stamp, _ := os.ReadFile(filepath.Join(dir, InstallMethodFile)); string(stamp) != method+"\n" {
			t.Fatalf("%s stamp %q", method, stamp)
		}
	}
}
