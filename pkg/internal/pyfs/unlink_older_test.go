package pyfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func agedFile(t *testing.T, p string, age float64, now float64) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(0, int64((now-age)*1e9))
	os.Chtimes(p, ts, ts)
}

func TestUnlinkFilesOlderThanRemovesOnlyMatchingOldFiles(t *testing.T) {
	dir := t.TempDir()
	now := float64(time.Now().Unix())
	agedFile(t, filepath.Join(dir, "old.json"), 120, now)
	agedFile(t, filepath.Join(dir, "fresh.json"), 10, now)
	agedFile(t, filepath.Join(dir, "old.txt"), 120, now)
	sub := filepath.Join(dir, "olddir.json")
	os.Mkdir(sub, 0o755)
	ts := time.Unix(int64(now-120), 0)
	os.Chtimes(sub, ts, ts)
	if n := UnlinkFilesOlderThan(dir, "*.json", 60, &now); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.json")); !os.IsNotExist(err) {
		t.Fatal("old.json survived")
	}
	for _, keep := range []string{"fresh.json", "old.txt", "olddir.json"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("%s removed", keep)
		}
	}
}

func TestUnlinkFilesOlderThanMissingDirectoryIsZero(t *testing.T) {
	if n := UnlinkFilesOlderThan(filepath.Join(t.TempDir(), "absent"), "*", 0, nil); n != 0 {
		t.Fatalf("removed %d", n)
	}
}

func TestUnlinkFilesOlderThanFailureSkipsThatFileAndKeepsSweeping(t *testing.T) {
	dir := t.TempDir()
	now := float64(time.Now().Unix())
	agedFile(t, filepath.Join(dir, "a.json"), 120, now)
	agedFile(t, filepath.Join(dir, "b.json"), 120, now)
	prev := unlinkSweepEntry
	unlinkSweepEntry = func(p string) error {
		if filepath.Base(p) == "a.json" {
			return &os.PathError{Op: "unlink", Path: p, Err: errors.New("locked")}
		}
		return os.Remove(p)
	}
	t.Cleanup(func() { unlinkSweepEntry = prev })
	if n := UnlinkFilesOlderThan(dir, "*.json", 60, &now); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal("stuck file vanished")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.json")); !os.IsNotExist(err) {
		t.Fatal("later file survived")
	}
}
