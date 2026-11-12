package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Isolates fixture git from the machine's configuration and gives every commit an identity
func ugEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	for key, value := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid"} {
		t.Setenv(key, value)
	}
}

// Runs git in a fixture directory and returns its stdout, failing the test on a nonzero exit
func ugGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

// Writes text to a fixture file, creating its parent directories
func ugWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Reads a fixture file as text
func ugRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func ugChurnRepo(t *testing.T) string {
	t.Helper()
	ugEnv(t)
	repo := t.TempDir()
	ugGit(t, repo, "init", "-q")
	ugWrite(t, filepath.Join(repo, "package.json"), `{"workspaces": ["apps/*", "ui-tui"]}`)
	ugWrite(t, filepath.Join(repo, "package-lock.json"), "lock v1\n")
	for _, rel := range []string{"apps/desktop", "ui-tui", "vendor/foo"} {
		ugWrite(t, filepath.Join(repo, filepath.FromSlash(rel), "package.json"), "{}\n")
	}
	ugGit(t, repo, "add", ".")
	ugGit(t, repo, "commit", "-qm", "initial")
	return repo
}

func ugStillDirtyAfterCleanup(t *testing.T, repo string, dirty ...string) []string {
	t.Helper()
	for _, rel := range dirty {
		ugWrite(t, filepath.Join(repo, filepath.FromSlash(rel)), "dirty\n")
	}
	if err := discardLockfileChurn(context.Background(), io.Discard, []string{"git"}, repo, nil); err != nil {
		t.Fatal(err)
	}
	left := strings.Fields(ugGit(t, repo, "diff", "--name-only"))
	slices.Sort(left)
	return left
}

func TestDirtyWorkspaceManifestPreservesRootLock(t *testing.T) {
	repo := ugChurnRepo(t)
	got := ugStillDirtyAfterCleanup(t, repo, "apps/desktop/package.json", "package-lock.json")
	if want := []string{"apps/desktop/package.json", "package-lock.json"}; !slices.Equal(got, want) {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
}

func TestManifestOutsideWorkspaceGraphDoesNotProtectRootLock(t *testing.T) {
	repo := ugChurnRepo(t)
	got := ugStillDirtyAfterCleanup(t, repo, "vendor/foo/package.json", "package-lock.json")
	if want := []string{"vendor/foo/package.json"}; !slices.Equal(got, want) {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
}

func TestUnsupportedWorkspacesGlobStillDiscardsRootLockChurn(t *testing.T) {
	repo := ugChurnRepo(t)
	ugWrite(t, filepath.Join(repo, "package.json"), `{"workspaces": "apps/*"}`)
	ugGit(t, repo, "commit", "-qam", "string workspaces")
	got := ugStillDirtyAfterCleanup(t, repo, "vendor/foo/package.json", "package-lock.json")
	if want := []string{"vendor/foo/package.json"}; !slices.Equal(got, want) {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
}

// A fork checkout with no upstream remote, its own home, a scripted terminal and a counted line input
type ugFork struct {
	cwd, marker string
	inputs      int
}

func ugForkWithoutUpstream(t *testing.T, stdinTTY, stdoutTTY bool, answer string) *ugFork {
	t.Helper()
	ugEnv(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	fork := &ugFork{cwd: t.TempDir(), marker: filepath.Join(home, skipUpstreamPromptFile)}
	ugGit(t, fork.cwd, "init", "-q", "-b", "main")
	savedIn, savedOut, savedInput := updateStdinIsTTY, updateStdoutIsTTY, updateInput
	updateStdinIsTTY = func() bool { return stdinTTY }
	updateStdoutIsTTY = func(io.Writer) bool { return stdoutTTY }
	updateInput = func(string) (string, error) {
		fork.inputs++
		return answer, nil
	}
	t.Cleanup(func() { updateStdinIsTTY, updateStdoutIsTTY, updateInput = savedIn, savedOut, savedInput })
	return fork
}

func (f *ugFork) remotes(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(ugGit(t, f.cwd, "remote"))
}

func TestUpstreamPromptNonInteractiveAssumeYesSkipsPromptWithoutTouchingRemotes(t *testing.T) {
	fork := ugForkWithoutUpstream(t, true, true, "")
	checked, err := syncWithUpstreamIfNeeded(context.Background(), io.Discard, []string{"git"}, fork.cwd, true, nil, nil)
	if err != nil || checked {
		t.Fatalf("checked = %v, err = %v", checked, err)
	}
	if fork.inputs != 0 || fork.remotes(t) != "" || statExists(fork.marker) {
		t.Fatalf("inputs %d, remotes %q, marker %v", fork.inputs, fork.remotes(t), statExists(fork.marker))
	}
}

func TestUpstreamPromptNonInteractiveNonTtySkipsPromptWithoutPersistingDecline(t *testing.T) {
	for _, tty := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		fork := ugForkWithoutUpstream(t, tty[0], tty[1], "")
		checked, err := syncWithUpstreamIfNeeded(context.Background(), io.Discard, []string{"git"}, fork.cwd, false, nil, nil)
		if err != nil || checked {
			t.Fatalf("%v: checked = %v, err = %v", tty, checked, err)
		}
		if fork.inputs != 0 || fork.remotes(t) != "" || statExists(fork.marker) {
			t.Fatalf("%v: inputs %d, remotes %q, marker %v", tty, fork.inputs, fork.remotes(t), statExists(fork.marker))
		}
	}
}

func TestUpstreamPromptNonInteractiveGatewayPromptRoutesThroughInputFn(t *testing.T) {
	fork := ugForkWithoutUpstream(t, false, false, "")
	var prompts [][2]string
	gwInput := func(prompt, def string) (string, error) {
		prompts = append(prompts, [2]string{prompt, def})
		return "n", nil
	}
	if _, err := syncWithUpstreamIfNeeded(context.Background(), io.Discard, []string{"git"}, fork.cwd, false, gwInput, nil); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0][1] != "n" {
		t.Fatalf("prompts = %v", prompts)
	}
	if fork.inputs != 0 || fork.remotes(t) != "" || !statExists(fork.marker) {
		t.Fatalf("inputs %d, remotes %q, marker %v", fork.inputs, fork.remotes(t), statExists(fork.marker))
	}
}

func TestUpstreamPromptNonInteractiveInteractiveDeclineStillPersistsMarker(t *testing.T) {
	fork := ugForkWithoutUpstream(t, true, true, "n")
	if _, err := syncWithUpstreamIfNeeded(context.Background(), io.Discard, []string{"git"}, fork.cwd, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fork.inputs != 1 || fork.remotes(t) != "" || !statExists(fork.marker) {
		t.Fatalf("inputs %d, remotes %q, marker %v", fork.inputs, fork.remotes(t), statExists(fork.marker))
	}
}

func TestUpstreamPromptNonInteractiveInteractiveAcceptAddsUpstream(t *testing.T) {
	fork := ugForkWithoutUpstream(t, true, true, "y")
	if _, err := syncWithUpstreamIfNeeded(context.Background(), io.Discard, []string{"git"}, fork.cwd, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ugGit(t, fork.cwd, "remote", "get-url", "upstream")); got != officialRepoURL {
		t.Fatalf("upstream = %q, want %q", got, officialRepoURL)
	}
	if statExists(fork.marker) {
		t.Fatal("an accepted prompt persisted the skip marker")
	}
}

// A checkout with LF in the index, CRLF written by git's own checkout and core.autocrlf still true
func ugManagedRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	ugEnv(t)
	repo := filepath.Join(t.TempDir(), "managed")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, repo, "init")
	for name, body := range files {
		ugWrite(t, filepath.Join(repo, filepath.FromSlash(name)), body)
	}
	ugGit(t, repo, "-c", "core.autocrlf=false", "add", "-A")
	ugGit(t, repo, "commit", "-m", "init")
	ugGit(t, repo, "config", "core.autocrlf", "true")
	for name := range files {
		if err := os.Remove(filepath.Join(repo, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	ugGit(t, repo, "checkout", "--", ".")
	bumped := time.Now().Add(5 * time.Second)
	for name := range files {
		if err := os.Chtimes(filepath.Join(repo, filepath.FromSlash(name)), bumped, bumped); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// Paths that differ from the index once the tree is read with autocrlf off
func ugEOLDirty(t *testing.T, repo string) []string {
	t.Helper()
	dirty := strings.Fields(ugGit(t, repo, "-c", "core.autocrlf=false", "diff", "--name-only"))
	slices.Sort(dirty)
	return dirty
}

// The checkout's own core.autocrlf setting, empty when it has none
func ugAutocrlf(repo string) string {
	cmd := exec.Command("git", "config", "--local", "--get", "core.autocrlf")
	cmd.Dir = repo
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func ugNormalize(repo string, gitCmd ...string) {
	if len(gitCmd) == 0 {
		gitCmd = []string{"git"}
	}
	normalizeManagedEOL(context.Background(), io.Discard, gitCmd, repo, nil)
}

func TestChurnInvisibleUnderAutocrlfTrueIsStillFound(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\ny = 2\n"})
	if status := strings.TrimSpace(ugGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("status = %q", status)
	}
	if !strings.Contains(ugRead(t, filepath.Join(repo, "a.py")), "\r\n") {
		t.Fatal("the fixture has no CRLF churn")
	}

	ugNormalize(repo)

	if dirty := ugEOLDirty(t, repo); len(dirty) != 0 {
		t.Fatalf("dirty = %v", dirty)
	}
	if strings.Contains(ugRead(t, filepath.Join(repo, "a.py")), "\r\n") {
		t.Fatal("the churn was not cleared")
	}
	if got := ugAutocrlf(repo); got != "false" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestChurnIsClearedAndThePinIsPersisted(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\n", "b.md": "# Title\n\nbody\n"})
	ugNormalize(repo)
	if dirty := ugEOLDirty(t, repo); len(dirty) != 0 {
		t.Fatalf("dirty = %v", dirty)
	}
	if strings.Contains(ugRead(t, filepath.Join(repo, "a.py")), "\r\n") {
		t.Fatal("the churn was not cleared")
	}
	if got := ugAutocrlf(repo); got != "false" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestRealEditsSurviveEvenWhenLineEndingsAlsoFlipped(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"churn.py": "y = 2\n", "both.py": "z = 3\n"})
	ugWrite(t, filepath.Join(repo, "both.py"), "z = 3\r\nz += 1\r\n")
	ugNormalize(repo)
	if dirty := ugEOLDirty(t, repo); !slices.Equal(dirty, []string{"both.py"}) {
		t.Fatalf("dirty = %v", dirty)
	}
	if got := ugRead(t, filepath.Join(repo, "both.py")); got != "z = 3\r\nz += 1\r\n" {
		t.Fatalf("both.py = %q", got)
	}
	if got := ugAutocrlf(repo); got != "false" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestPinAloneIsWrittenWhenThereIsNoChurn(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\n"})
	ugWrite(t, filepath.Join(repo, "a.py"), "x = 1\n")
	ugNormalize(repo)
	if dirty := ugEOLDirty(t, repo); len(dirty) != 0 {
		t.Fatalf("dirty = %v", dirty)
	}
	if got := ugAutocrlf(repo); got != "false" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestAlreadyPinnedCheckoutIsUntouched(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\n"})
	ugGit(t, repo, "config", "core.autocrlf", "false")
	ugNormalize(repo)
	if dirty := ugEOLDirty(t, repo); !slices.Equal(dirty, []string{"a.py"}) {
		t.Fatalf("dirty = %v", dirty)
	}
}

func TestAutocrlfInputIsLeftAlone(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\n"})
	ugGit(t, repo, "config", "core.autocrlf", "input")
	ugNormalize(repo)
	if got := ugAutocrlf(repo); got != "input" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestChurnAcrossMoreFilesThanFitInOneArgv(t *testing.T) {
	files := map[string]string{}
	for i := range 1200 {
		files[fmt.Sprintf("pkg/mod_%04d.py", i)] = fmt.Sprintf("VALUE = %d\n", i)
	}
	repo := ugManagedRepo(t, files)
	if dirty := ugEOLDirty(t, repo); len(dirty) != len(files) {
		t.Fatalf("%d of %d files carry churn", len(dirty), len(files))
	}
	ugNormalize(repo)
	if dirty := ugEOLDirty(t, repo); len(dirty) != 0 {
		t.Fatalf("%d files still dirty", len(dirty))
	}
	if got := ugAutocrlf(repo); got != "false" {
		t.Fatalf("autocrlf = %q", got)
	}
}

func TestNonRepoIsIgnored(t *testing.T) {
	ugEnv(t)
	var out bytes.Buffer
	normalizeManagedEOL(context.Background(), &out, []string{"git"}, t.TempDir(), nil)
	if out.Len() != 0 {
		t.Fatalf("output: %q", out.String())
	}
}

// A clone parked on old-feature cut from the first commit while origin/main moved two commits on
func ugRepoPair(t *testing.T) string {
	t.Helper()
	ugEnv(t)
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	origin, clone := filepath.Join(tmp, "origin"), filepath.Join(tmp, "clone")
	for _, dir := range []string{home, origin} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", home)
	ugGit(t, origin, "init", "-q", "-b", "main")
	ugGit(t, origin, "config", "user.email", "test@example.com")
	ugGit(t, origin, "config", "user.name", "Test")
	ugWrite(t, filepath.Join(origin, "a.txt"), "one\n")
	ugGit(t, origin, "add", "a.txt")
	ugGit(t, origin, "commit", "-qm", "c1")
	ugGit(t, tmp, "clone", "-q", origin, clone)
	ugGit(t, clone, "config", "user.email", "test@example.com")
	ugGit(t, clone, "config", "user.name", "Test")
	ugGit(t, clone, "checkout", "-qb", "old-feature")
	ugWrite(t, filepath.Join(origin, "a.txt"), "two\n")
	ugGit(t, origin, "commit", "-aqm", "c2")
	ugWrite(t, filepath.Join(origin, "b.txt"), "three\n")
	ugGit(t, origin, "add", "b.txt")
	ugGit(t, origin, "commit", "-qm", "c3")
	ugGit(t, clone, "fetch", "-q", "origin", "main")
	return clone
}

func ugAssess(t *testing.T, clone, target string) (bool, string) {
	t.Helper()
	safe, reason, err := assessParkedBranchSwitch(context.Background(), []string{"git"}, clone, "old-feature", target)
	if err != nil {
		t.Fatal(err)
	}
	return safe, reason
}

func TestCleanFullyMergedBranchIsSafeToSwitch(t *testing.T) {
	clone := ugRepoPair(t)
	if safe, reason := ugAssess(t, clone, "main"); !safe || reason != "" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestDirtyTreeBlocksAutoSwitch(t *testing.T) {
	clone := ugRepoPair(t)
	ugWrite(t, filepath.Join(clone, "a.txt"), "local edit\n")
	if safe, reason := ugAssess(t, clone, "main"); safe || reason != "dirty" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestUntrackedFileBlocksAutoSwitch(t *testing.T) {
	clone := ugRepoPair(t)
	ugWrite(t, filepath.Join(clone, "scratch.py"), "wip\n")
	if safe, reason := ugAssess(t, clone, "main"); safe || reason != "dirty" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestUnmergedCommitsSwitchWithKeptNotice(t *testing.T) {
	clone := ugRepoPair(t)
	ugWrite(t, filepath.Join(clone, "feature.txt"), "unmerged work\n")
	ugGit(t, clone, "add", "feature.txt")
	ugGit(t, clone, "commit", "-qm", "feature work")
	if safe, reason := ugAssess(t, clone, "main"); !safe || reason != "unmerged:1" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestEquivalentCherryPickedCommitIsStillSafe(t *testing.T) {
	clone := ugRepoPair(t)
	ugGit(t, clone, "cherry-pick", "origin/main~1")
	if safe, reason := ugAssess(t, clone, "main"); !safe || reason != "" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestConfigOptOutBlocksAutoSwitch(t *testing.T) {
	clone := ugRepoPair(t)
	ugWrite(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), "updates:\n  auto_switch_parked_branch: false\n")
	if safe, reason := ugAssess(t, clone, "main"); safe || reason != "disabled" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestMissingOriginRefIsUnverifiable(t *testing.T) {
	clone := ugRepoPair(t)
	if safe, reason := ugAssess(t, clone, "no-such-branch"); safe || reason != "unverifiable" {
		t.Fatalf("safe = %v, reason = %q", safe, reason)
	}
}

func TestBranchHeadSuffixEmptyOnNonRepo(t *testing.T) {
	ugEnv(t)
	if got := branchHeadSuffix(context.Background(), []string{"git"}, filepath.Join(t.TempDir(), "not-a-repo")); got != "" {
		t.Fatalf("suffix = %q", got)
	}
}
