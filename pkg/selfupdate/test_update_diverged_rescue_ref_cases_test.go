package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Commits name with text in repo and returns the new HEAD
func drCommit(t *testing.T, repo, name, text string) string {
	t.Helper()
	ugWrite(t, filepath.Join(repo, name), text)
	ugGit(t, repo, "add", name)
	ugGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "add "+name)
	return strings.TrimSpace(ugGit(t, repo, "rev-parse", "HEAD"))
}

// Checkout whose HEAD is strictly behind origin/main, and that HEAD
func drFastForwardCheckout(t *testing.T) (string, string) {
	t.Helper()
	ugEnv(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	upstream := filepath.Join(tmp, "linear-upstream")
	if err := os.Mkdir(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, upstream, "init", "-q", "-b", "main")
	first := drCommit(t, upstream, "shared.txt", "shared\n")
	drCommit(t, upstream, "upstream-only.txt", "upstream\n")
	checkout := filepath.Join(tmp, "linear-checkout")
	ugGit(t, tmp, "clone", "-q", upstream, checkout)
	ugGit(t, checkout, "reset", "-q", "--hard", first)
	return checkout, first
}

// Rescue refs of a checkout by name
func drRescueRefs(t *testing.T, checkout string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	out := ugGit(t, checkout, "for-each-ref", "--format=%(refname) %(objectname)", "refs/icebath-update-backups/")
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs
}

func TestOperationalFfFailurePreservesGitErrorWithoutReset(t *testing.T) {
	checkout, before := drFastForwardCheckout(t)
	ucPatchRoot(t, checkout)
	saved := updateGitRunner
	t.Cleanup(func() { updateGitRunner = saved })
	updateGitRunner = func(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error) {
		if len(args) >= 2 && args[0] == "merge" && args[1] == "--ff-only" {
			return &CompletedProcess{Args: slices.Concat(gitCmd, args), ReturnCode: 128,
				Stderr: "fatal: unable to read tree: RPC failed; transfer closed"}, nil
		}
		return saved(ctx, gitCmd, args, cwd, check, network)
	}
	var out bytes.Buffer
	_, err := pullUpdates(context.Background(), &out, []string{"git"}, "main", "", pullOptions{})
	var exit *pm.SystemExit
	if !errors.As(err, &exit) {
		t.Fatalf("want SystemExit, got %v\n%s", err, out.String())
	}
	if exit.Code != 1 {
		t.Fatalf("exit code %d", exit.Code)
	}
	if head := strings.TrimSpace(ugGit(t, checkout, "rev-parse", "HEAD")); head != before {
		t.Fatalf("HEAD moved to %s, want %s", head, before)
	}
	if refs := drRescueRefs(t, checkout); len(refs) != 0 {
		t.Fatalf("rescue refs = %v", refs)
	}
	text := out.String()
	if !strings.Contains(text, "RPC failed; transfer closed") || !strings.Contains(text, "HEAD is still an ancestor of origin/main") {
		t.Fatal(text)
	}
	if strings.Contains(text, "Local history has diverged") || strings.Contains(text, "git reset --hard") {
		t.Fatal(text)
	}
}
