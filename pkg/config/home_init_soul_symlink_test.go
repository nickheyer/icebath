package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var soulTestSubdirs = []string{"cron", "sessions", "logs", "memories"}

func TestInitializeHomeReplacesUnwritableSoulSymlink(t *testing.T) {
	for _, tc := range []struct{ id, target string }{{"cyclic", "SOUL.md"}, {"dangling", "missing-dir/SOUL.md"}} {
		t.Run(tc.id, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), ".icebath")
			if err := os.Mkdir(h, 0o755); err != nil {
				t.Fatal(err)
			}
			soul := filepath.Join(h, "SOUL.md")
			if err := os.Symlink(filepath.Join(h, tc.target), soul); err != nil {
				t.Fatal(err)
			}
			if err := InitializeHome(h, soulTestSubdirs, map[string]bool{}); err != nil {
				t.Fatal(err)
			}
			if st, _ := os.Lstat(soul); st.Mode()&os.ModeSymlink != 0 {
				t.Fatal("still a symlink")
			}
			if got, _ := os.ReadFile(soul); string(got) != DefaultIdentity {
				t.Fatalf("soul %q", got)
			}
			entries, _ := os.ReadDir(h)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".SOUL.md.") {
					t.Fatalf("leftover temp %s", e.Name())
				}
			}
		})
	}
}

func TestSoulSymlinkToCustomizedFileIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "shared-identity.md")
	os.WriteFile(target, []byte("custom identity\n"), 0o644)
	soul := filepath.Join(h, "SOUL.md")
	if err := os.Symlink(target, soul); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultSoulMD(h); err != nil {
		t.Fatal(err)
	}
	if err := InitializeHome(h, soulTestSubdirs, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(soul); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
	if got, _ := os.ReadFile(soul); string(got) != "custom identity\n" {
		t.Fatalf("soul %q", got)
	}
}

// Upgrades a seeded text through a working link and leaves an undecodable file untouched
func TestEnsureDefaultSoulWritesThroughAWorkingLinkAndSkipsUndecodableFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "seeded.md")
	os.WriteFile(target, []byte("\uFEFF"+DefaultIdentity+"\r\n"), 0o644)
	soul := filepath.Join(dir, "SOUL.md")
	os.Symlink(target, soul)
	if err := ensureDefaultSoulMD(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(soul); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("working link replaced")
	}
	if got, _ := os.ReadFile(target); string(got) != DefaultIdentity {
		t.Fatalf("target not upgraded %q", got)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "SOUL.md"), []byte{0xff, 0xfe}, 0o644)
	if err := ensureDefaultSoulMD(other); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(other, "SOUL.md")); string(got) != "\xff\xfe" {
		t.Fatalf("undecodable file rewritten %q", got)
	}
}
