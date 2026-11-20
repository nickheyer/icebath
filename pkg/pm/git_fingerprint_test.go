package pm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes a file under root creating its parent directories
func writeGitFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadGitRevisionFingerprint(t *testing.T) {
	sha := strings.Repeat("a", 40)
	t.Run("loose_ref", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
		writeGitFile(t, repo, ".git/refs/heads/main", sha+"\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:refs/heads/main:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("bom_and_crlf_are_stripped", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "\xef\xbb\xbfref: refs/heads/main\r\n")
		writeGitFile(t, repo, ".git/refs/heads/main", "\xef\xbb\xbf"+sha+"\r\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:refs/heads/main:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("packed_ref", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
		writeGitFile(t, repo, ".git/packed-refs", "# pack-refs with: peeled fully-peeled sorted\n"+
			strings.Repeat("c", 40)+" refs/heads/other\n"+sha+" refs/heads/main\n^"+strings.Repeat("d", 40)+"\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:refs/heads/main:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("unresolved_ref", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "ref: refs/heads/gone\n")
		writeGitFile(t, repo, ".git/packed-refs", sha+" refs/heads/main\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:refs/heads/gone:unresolved" {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("detached_head", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", sha+"\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:HEAD:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("worktree_gitdir_file_uses_common_dir_refs", func(t *testing.T) {
		base := t.TempDir()
		main := filepath.Join(base, "main")
		wt := filepath.Join(base, "wt")
		writeGitFile(t, main, ".git/worktrees/wt/HEAD", "ref: refs/heads/feature\n")
		writeGitFile(t, main, ".git/worktrees/wt/commondir", "../..\n")
		writeGitFile(t, main, ".git/refs/heads/feature", sha+"\n")
		writeGitFile(t, wt, ".git", "gitdir: ../main/.git/worktrees/wt\n")
		fp, ok := ReadGitRevisionFingerprint(wt)
		if !ok || fp != "git:refs/heads/feature:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("worktree_packed_ref_in_common_dir", func(t *testing.T) {
		base := t.TempDir()
		main := filepath.Join(base, "main")
		wt := filepath.Join(base, "wt")
		writeGitFile(t, main, ".git/worktrees/wt/HEAD", "ref: refs/heads/feature\n")
		writeGitFile(t, main, ".git/worktrees/wt/commondir", "../..\n")
		writeGitFile(t, main, ".git/packed-refs", sha+" refs/heads/feature\n")
		writeGitFile(t, wt, ".git", "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")
		fp, ok := ReadGitRevisionFingerprint(wt)
		if !ok || fp != "git:refs/heads/feature:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("worktree_loose_ref_wins_over_common_dir", func(t *testing.T) {
		base := t.TempDir()
		main := filepath.Join(base, "main")
		wt := filepath.Join(base, "wt")
		writeGitFile(t, main, ".git/worktrees/wt/HEAD", "ref: refs/bisect/bad\n")
		writeGitFile(t, main, ".git/worktrees/wt/commondir", "../..\n")
		writeGitFile(t, main, ".git/worktrees/wt/refs/bisect/bad", strings.Repeat("e", 40)+"\n")
		writeGitFile(t, main, ".git/refs/bisect/bad", sha+"\n")
		writeGitFile(t, wt, ".git", "gitdir: ../main/.git/worktrees/wt\n")
		fp, ok := ReadGitRevisionFingerprint(wt)
		if !ok || fp != "git:refs/bisect/bad:"+strings.Repeat("e", 40) {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("blank_commondir_keeps_gitdir", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
		writeGitFile(t, repo, ".git/commondir", "  \n")
		writeGitFile(t, repo, ".git/packed-refs", sha+" refs/heads/main\n")
		fp, ok := ReadGitRevisionFingerprint(repo)
		if !ok || fp != "git:refs/heads/main:"+sha {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("missing_git_dir", func(t *testing.T) {
		if fp, ok := ReadGitRevisionFingerprint(t.TempDir()); ok || fp != "" {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("ref_naming_a_directory_is_unreadable", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git/HEAD", "ref: refs/heads\n")
		writeGitFile(t, repo, ".git/refs/heads/main", sha+"\n")
		if fp, ok := ReadGitRevisionFingerprint(repo); ok || fp != "" {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
	t.Run("gitdir_file_without_gitdir_line_is_unreadable", func(t *testing.T) {
		repo := t.TempDir()
		writeGitFile(t, repo, ".git", "something: else\n")
		if fp, ok := ReadGitRevisionFingerprint(repo); ok || fp != "" {
			t.Fatalf("got %q %v", fp, ok)
		}
	})
}
