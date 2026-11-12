package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Origin repo with a padded plugin.py and a clone of it, both with a git identity
func gitPullRepos(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	origin, checkout := filepath.Join(root, "origin"), filepath.Join(root, "checkout")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	c6Git(t, origin, "init", "-q", "-b", "main")
	c6Git(t, origin, "config", "user.email", "t@t")
	c6Git(t, origin, "config", "user.name", "t")
	pad := make([]string, 12)
	for i := range pad {
		pad[i] = "# pad " + strconv.Itoa(i)
	}
	c6Write(t, filepath.Join(origin, "plugin.py"), "VALUE = 1\n"+strings.Join(pad, "\n")+"\nOTHER = 'a'\n")
	c6Git(t, origin, "add", ".")
	c6Git(t, origin, "commit", "-qm", "init")
	c6Git(t, root, "clone", "-q", origin, checkout)
	c6Git(t, checkout, "config", "user.email", "t@t")
	c6Git(t, checkout, "config", "user.name", "t")
	return origin, checkout
}

// Replaces the plugin.py line starting with prefix and keeps the rest
func gitPullSetLine(t *testing.T, repo, prefix, line string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "plugin.py"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			lines[i] = line
		}
	}
	c6Write(t, filepath.Join(repo, "plugin.py"), strings.Join(lines, "\n")+"\n")
}

func gitPullRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func gitPullCtx() context.Context {
	return home.WithOverride(context.Background(), os.Getenv("ICEBATH_HOME"))
}

func TestGitPullPluginDirAutostashDirtyCheckoutPullsAndReappliesLocalEdit(t *testing.T) {
	origin, checkout := gitPullRepos(t)
	gitPullSetLine(t, origin, "VALUE", "VALUE = 2")
	c6Git(t, origin, "commit", "-qam", "bump value")
	gitPullSetLine(t, checkout, "OTHER", "OTHER = 'local'")
	ok, msg, err := gitPullPluginDir(gitPullCtx(), checkout)
	if err != nil || !ok {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	content := gitPullRead(t, filepath.Join(checkout, "plugin.py"))
	if !strings.Contains(content, "VALUE = 2") || !strings.Contains(content, "OTHER = 'local'") || !strings.Contains(msg, "re-applied") {
		t.Fatalf("content %q msg %q", content, msg)
	}
	if list := c6Git(t, checkout, "stash", "list"); list != "" {
		t.Fatalf("stash list %q", list)
	}
}

func TestGitPullPluginDirAutostashConflictingLocalEditIsPreservedInStash(t *testing.T) {
	origin, checkout := gitPullRepos(t)
	gitPullSetLine(t, origin, "VALUE", "VALUE = 2")
	c6Git(t, origin, "commit", "-qam", "bump value")
	gitPullSetLine(t, checkout, "VALUE", "VALUE = 99")
	ok, msg, err := gitPullPluginDir(gitPullCtx(), checkout)
	if err != nil || !ok {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	content := gitPullRead(t, filepath.Join(checkout, "plugin.py"))
	if strings.Contains(content, "<<<<<<<") || !strings.Contains(content, "VALUE = 2") || !strings.Contains(msg, "preserved in git stash") {
		t.Fatalf("content %q msg %q", content, msg)
	}
	if list := c6Git(t, checkout, "stash", "list"); !strings.Contains(list, "icebath-plugin-update-autostash") {
		t.Fatalf("stash list %q", list)
	}
	if diff := c6Git(t, checkout, "stash", "show", "-p", "stash@{0}"); !strings.Contains(diff, "VALUE = 99") {
		t.Fatalf("stash diff %q", diff)
	}
}

func TestGitPullPluginDirAutostashUntrackedLocalFileSurvivesUpdate(t *testing.T) {
	origin, checkout := gitPullRepos(t)
	gitPullSetLine(t, origin, "VALUE", "VALUE = 2")
	c6Git(t, origin, "commit", "-qam", "bump value")
	c6Write(t, filepath.Join(checkout, "local_notes.txt"), "keep me\n")
	ok, msg, err := gitPullPluginDir(gitPullCtx(), checkout)
	if err != nil || !ok {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	if notes := gitPullRead(t, filepath.Join(checkout, "local_notes.txt")); notes != "keep me\n" {
		t.Fatalf("notes %q", notes)
	}
	if !strings.Contains(gitPullRead(t, filepath.Join(checkout, "plugin.py")), "VALUE = 2") {
		t.Fatal("update did not land")
	}
}

func TestGitPullPluginDirAutostashCleanCheckoutUnchangedBehavior(t *testing.T) {
	_, checkout := gitPullRepos(t)
	ok, msg, err := gitPullPluginDir(gitPullCtx(), checkout)
	if err != nil || !ok || !strings.Contains(msg, "Already up to date") {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	if list := c6Git(t, checkout, "stash", "list"); list != "" {
		t.Fatalf("stash list %q", list)
	}
}

func TestGitPullPluginDirAutostashAutostashAddressesGitByShaNeverBraceSelector(t *testing.T) {
	origin, checkout := gitPullRepos(t)
	gitPullSetLine(t, origin, "VALUE", "VALUE = 2")
	c6Git(t, origin, "commit", "-qam", "bump value")
	gitPullSetLine(t, checkout, "OTHER", "OTHER = 'local'")
	var argvLog [][]string
	real := runPluginGit
	t.Cleanup(func() { runPluginGit = real })
	runPluginGit = func(ctx context.Context, gitExe, target string, timeout time.Duration, authURL string, args ...string) (*gitResult, error) {
		argvLog = append(argvLog, append([]string{}, args...))
		return real(ctx, gitExe, target, timeout, authURL, args...)
	}
	ok, msg, err := gitPullPluginDir(gitPullCtx(), checkout)
	if err != nil || !ok || !strings.Contains(msg, "re-applied") {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	if list := c6Git(t, checkout, "stash", "list"); list != "" {
		t.Fatalf("stash list %q", list)
	}
	var applied [][]string
	for _, args := range argvLog {
		for _, arg := range args {
			if strings.ContainsAny(arg, "{}") {
				t.Fatalf("brace selector in %v", argvLog)
			}
		}
		if len(args) >= 2 && args[0] == "stash" && args[1] == "apply" {
			applied = append(applied, args)
		}
	}
	if len(applied) != 1 || len(applied[0]) < 3 || len(applied[0][2]) != 40 {
		t.Fatalf("applied %v", applied)
	}
}

func TestAutostashDirtyTreePromotesIntentToAddEntries(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	repo := t.TempDir()
	c6Git(t, repo, "init", "-q", "-b", "main")
	c6Git(t, repo, "config", "user.email", "t@example.com")
	c6Git(t, repo, "config", "user.name", "t")
	c6Write(t, filepath.Join(repo, "README.md"), "plugin\n")
	c6Git(t, repo, "add", "-A")
	c6Git(t, repo, "commit", "-qm", "init")
	c6Write(t, filepath.Join(repo, "local_patch.py"), "PATCHED = True\n")
	c6Git(t, repo, "add", "-N", "local_patch.py")
	if status := c6Git(t, repo, "status", "--porcelain"); status != "A local_patch.py" {
		t.Fatalf("status %q", status)
	}
	stashed, problem, err := autostashDirtyTree(gitPullCtx(), "git", repo)
	if err != nil || problem != "" {
		t.Fatalf("the plugin autostash must not be blocked by i-t-a entries: %q %v", problem, err)
	}
	if want := c6Git(t, repo, "rev-parse", "refs/stash"); stashed != want {
		t.Fatalf("stashed %q want %q", stashed, want)
	}
	if status := c6Git(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("status %q", status)
	}
}
