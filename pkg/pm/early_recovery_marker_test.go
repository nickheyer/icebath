package pm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInterruptedPullMarkerFollowsTheCheckoutGitDir(t *testing.T) {
	root := t.TempDir()
	got, err := InterruptedPullMarkerPath(root)
	if err != nil || got != filepath.Join(root, ".git", "icebath-update-pull") {
		t.Fatalf("plain checkout marker = %q, %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ = InterruptedPullMarkerPath(root); got != filepath.Join(root, ".git", InterruptedPullMarker) {
		t.Fatalf("directory git dir marker = %q", got)
	}
	linked := t.TempDir()
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("\xef\xbb\xbfgitdir: ../main/.git/worktrees/w \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = InterruptedPullMarkerPath(linked); got != filepath.Join(linked, "..", "main", ".git", "worktrees", "w", InterruptedPullMarker) {
		t.Fatalf("linked worktree marker = %q", got)
	}
	absolute := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+absolute), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = InterruptedPullMarkerPath(linked); got != filepath.Join(absolute, InterruptedPullMarker) {
		t.Fatalf("absolute gitdir marker = %q", got)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InterruptedPullMarkerPath(linked); err == nil {
		t.Fatal("an undecodable .git file must raise as read_text does")
	}
}
