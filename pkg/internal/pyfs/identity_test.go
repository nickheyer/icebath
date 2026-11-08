package pyfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLstatIdentityMatchesHardlinksAndSeparatesFiles(t *testing.T) {
	dir := t.TempDir()
	a, b, alias := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "alias")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(a, alias); err != nil {
		t.Fatal(err)
	}
	devA, inoA, err := LstatIdentity(a)
	if err != nil || inoA == 0 {
		t.Fatalf("identity of a: %d %v", inoA, err)
	}
	devL, inoL, err := LstatIdentity(alias)
	if err != nil || devL != devA || inoL != inoA {
		t.Fatalf("hardlink identity %d/%d differs from %d/%d: %v", devL, inoL, devA, inoA, err)
	}
	devB, inoB, err := LstatIdentity(b)
	if err != nil || (devB == devA && inoB == inoA) {
		t.Fatalf("distinct file shares identity %d/%d: %v", devB, inoB, err)
	}
	if _, _, err := LstatIdentity(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path error %v", err)
	}
}

func TestLstatIdentityDoesNotFollowAFinalLink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	devT, inoT, err := LstatIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	devL, inoL, err := LstatIdentity(link)
	if err != nil || (devL == devT && inoL == inoT) {
		t.Fatalf("link identity %d/%d equals its target: %v", devL, inoL, err)
	}
}
