package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// An origin and its clone parked on a feature branch cut at the first commit, origin main two commits ahead
func ucRepoPair(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	origin, clone := filepath.Join(tmp, "origin"), filepath.Join(tmp, "clone")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ucGit(t, origin, "init", "-q", "-b", "main")
	ucGit(t, origin, "config", "user.email", "test@example.com")
	ucGit(t, origin, "config", "user.name", "Test")
	write(filepath.Join(origin, "a.txt"), "one\n")
	ucGit(t, origin, "add", "a.txt")
	ucGit(t, origin, "commit", "-qm", "c1")
	ucGit(t, tmp, "clone", "-q", origin, clone)
	ucGit(t, clone, "config", "user.email", "test@example.com")
	ucGit(t, clone, "config", "user.name", "Test")
	ucGit(t, clone, "checkout", "-qb", "old-feature")
	write(filepath.Join(origin, "a.txt"), "two\n")
	ucGit(t, origin, "commit", "-aqm", "c2")
	write(filepath.Join(origin, "b.txt"), "three\n")
	ucGit(t, origin, "add", "b.txt")
	ucGit(t, origin, "commit", "-qm", "c3")
	ucGit(t, clone, "fetch", "-q", "origin", "main")
	return clone
}

// Runs the update implementation over repo and returns its output and exit status
func ucRunImpl(t *testing.T, repo string, args map[string]any) (string, int) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ucPatchRoot(t, repo)
	saved := installRepairIsWindows
	installRepairIsWindows = func() bool { return false }
	t.Cleanup(func() { installRepairIsWindows = saved })
	var out bytes.Buffer
	err := cmdUpdateImpl(context.Background(), &out, args, false)
	var exit *pm.SystemExit
	if !errors.As(err, &exit) {
		t.Fatalf("the update did not exit: %v\n%s", err, out.String())
	}
	return out.String(), exit.Code
}

func TestUpdateRefusesStoppedRebaseWithoutMovingHead(t *testing.T) {
	repo := ucRepoPair(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ucGit(t, repo, "add", "a.txt")
	ucGit(t, repo, "commit", "-qm", "topic")
	if _, code := ucRun(t, repo, "rebase", "--merge", "origin/main"); code == 0 {
		t.Fatal("the fixture rebase did not stop")
	}
	rebaseDir := filepath.Join(repo, ".git", "rebase-merge")
	if info, err := os.Stat(rebaseDir); err != nil || !info.IsDir() {
		t.Fatal("no rebase-merge state")
	}
	if got := ucGit(t, repo, "branch", "--show-current"); got != "" {
		t.Fatalf("branch = %q", got)
	}
	headBefore := ucGit(t, repo, "rev-parse", "HEAD")
	out, code := ucRunImpl(t, repo, map[string]any{"branch": nil, "yes": true, "force": false, "force_venv": false})
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if got := ucGit(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD moved to %s", got)
	}
	if info, err := os.Stat(rebaseDir); err != nil || !info.IsDir() {
		t.Fatal("the rebase state was removed")
	}
	if got := ucGit(t, repo, "stash", "list"); got != "" {
		t.Fatalf("stash list = %q", got)
	}
	for _, part := range []string{"Git rebase is in progress", repo, "git rebase --abort"} {
		if !strings.Contains(out, part) {
			t.Fatalf("output lacks %q:\n%s", part, out)
		}
	}
}

func TestUpdateReportsGitAmAbortForApplyState(t *testing.T) {
	repo := ucRepoPair(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ucGit(t, repo, "add", "a.txt")
	ucGit(t, repo, "commit", "-qm", "topic")
	patch := filepath.Join(filepath.Dir(repo), "upstream.patch")
	text, _ := ucRun(t, repo, "format-patch", "-1", "origin/main~1", "--stdout")
	if err := os.WriteFile(patch, []byte(text+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := ucRun(t, repo, "am", patch); code == 0 {
		t.Fatal("the fixture am did not stop")
	}
	applying := filepath.Join(repo, ".git", "rebase-apply", "applying")
	if info, err := os.Stat(applying); err != nil || !info.Mode().IsRegular() {
		t.Fatal("no rebase-apply/applying state")
	}
	headBefore := ucGit(t, repo, "rev-parse", "HEAD")
	out, code := ucRunImpl(t, repo, map[string]any{"branch": nil, "yes": true, "force": false, "force_venv": false})
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if got := ucGit(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD moved to %s", got)
	}
	if info, err := os.Stat(applying); err != nil || !info.Mode().IsRegular() {
		t.Fatal("the am state was removed")
	}
	for _, part := range []string{"Git am is in progress", "git am --abort"} {
		if !strings.Contains(out, part) {
			t.Fatalf("output lacks %q:\n%s", part, out)
		}
	}
}
