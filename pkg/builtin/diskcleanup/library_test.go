package diskcleanup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var bg = context.Background()

// Isolated home named .icebath under a temp dir
func isolate(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Writes a file creating its parents
func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Tracked entry in the stored key order
func entry(path, category, stamp string, size int64) Entry {
	return pyjson.ObjectOf("path", path, "category", category, "timestamp", stamp, "size", size)
}

// Timestamp a number of days in the past
func daysAgo(n int) string {
	return pydatetime.ISOFormatUTC(time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour))
}

func mustGuess(t *testing.T, p string) string {
	t.Helper()
	c, err := GuessCategory(bg, p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustQuick(t *testing.T) Summary {
	t.Helper()
	s, err := Quick(bg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustTrack(t *testing.T, p, cat string) bool {
	t.Helper()
	ok, err := Track(bg, p, cat, true)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func mustSave(t *testing.T, items ...Entry) {
	t.Helper()
	if err := SaveTracked(bg, items); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T) []Entry {
	t.Helper()
	items, err := LoadTracked(bg)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func gone(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, os.ErrNotExist)
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestIsSafePathAcceptsPathUnderHome(t *testing.T) {
	h := isolate(t)
	p := filepath.Join(h, "subdir", "file.txt")
	write(t, p, "x")
	if !IsSafePath(bg, p) {
		t.Fatal("path under home rejected")
	}
}

func TestTrackAcceptsTmpIcebathPathAfterPlatformResolution(t *testing.T) {
	isolate(t)
	temporary, err := os.MkdirTemp("/tmp", "icebath-disk-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(temporary) })
	if !mustTrack(t, temporary, "temp") {
		t.Fatal("tmp icebath dir not tracked")
	}
}

func TestIsSafePathRejectsOutsideHome(t *testing.T) {
	isolate(t)
	if IsSafePath(bg, "/etc/passwd") {
		t.Fatal("accepted /etc/passwd")
	}
}

func TestIsSafePathRejectsTmpTraversalAndNearPrefixes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX /tmp path contract")
	}
	isolate(t)
	if IsSafePath(bg, "/tmp/icebath-safe/../not-icebath/file") || IsSafePath(bg, "/tmp-near/icebath-safe/file") {
		t.Fatal("traversal or near prefix accepted")
	}
}

func TestIsSafePathRejectsFinalAndNestedSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink contract")
	}
	isolate(t)
	temporary, err := os.MkdirTemp("/tmp", "icebath-disk-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(temporary) })
	outsideDir, err := os.MkdirTemp("/tmp", "disk-cleanup-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outsideDir) })
	outsideFile := filepath.Join(outsideDir, "file.txt")
	write(t, outsideFile, "outside")
	finalLink := filepath.Join(temporary, "final-link")
	nestedLink := filepath.Join(temporary, "nested-link")
	if err := os.Symlink(outsideFile, finalLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, nestedLink); err != nil {
		t.Fatal(err)
	}
	if IsSafePath(bg, finalLink) || IsSafePath(bg, filepath.Join(nestedLink, "file.txt")) {
		t.Fatal("symlink escape accepted")
	}
}

func TestSymlinkLoopFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink contract")
	}
	isolate(t)
	loop := filepath.Join(t.TempDir(), "loop")
	os.Mkdir(loop, 0o755)
	first, second := filepath.Join(loop, "first"), filepath.Join(loop, "second")
	os.Symlink(second, first)
	os.Symlink(first, second)
	_ = IsSafePath(bg, first)
	if _, err := Track(bg, first, "temp", true); err != nil {
		t.Fatalf("loop raised %v", err)
	}
	saved := resolvePath
	resolvePath = func(string) (string, error) { return "", errors.New("ELOOP: too many levels of symbolic links") }
	t.Cleanup(func() { resolvePath = saved })
	if IsSafePath(bg, first) {
		t.Fatal("resolution error accepted")
	}
	if ok, err := Track(bg, first, "temp", true); ok || err != nil {
		t.Fatalf("track on resolution error %v %v", ok, err)
	}
}

func TestGuessCategoryNameRules(t *testing.T) {
	h := isolate(t)
	for name, want := range map[string]string{"test_foo.py": "test", "tmp_foo.log": "test", "mything.test.js": "test", "notes.md": ""} {
		p := filepath.Join(h, name)
		write(t, p, "x")
		if got := mustGuess(t, p); got != want {
			t.Fatalf("%s: %q want %q", name, got, want)
		}
	}
}

func TestGuessCategorySkipsProtectedTopLevelAndCron(t *testing.T) {
	h := isolate(t)
	logFile := filepath.Join(h, "logs", "test_log.txt")
	write(t, logFile, "x")
	run := filepath.Join(h, "cron", "output", "job_123", "run.md")
	write(t, run, "x")
	jobs := filepath.Join(h, "cronjobs", "jobs.json")
	write(t, jobs, "[]")
	if mustGuess(t, logFile) != "" || mustGuess(t, run) != "cron-output" || mustGuess(t, jobs) != "" {
		t.Fatal("protected or cron classification wrong")
	}
}

func TestEmptyDirSweepSkipsWorkspace(t *testing.T) {
	h := isolate(t)
	keep := filepath.Join(h, "workspace", "watch-battery", "data")
	os.MkdirAll(keep, 0o755)
	sweepable := filepath.Join(h, "pairing")
	os.Mkdir(sweepable, 0o755)
	sweepEmptyDirs(bg, h)
	if gone(keep) || !gone(sweepable) {
		t.Fatal("sweep touched workspace or kept an unprotected empty dir")
	}
}

func TestStaleCacheDirEntryIsSkippedButItsOldFilesStillAgeOut(t *testing.T) {
	h := isolate(t)
	cache := filepath.Join(h, "cache")
	os.MkdirAll(filepath.Join(cache, "terminal"), 0o755)
	oldFile := filepath.Join(cache, "scratch.txt")
	write(t, oldFile, "x")
	if mustGuess(t, cache) != "" || mustGuess(t, oldFile) != "temp" {
		t.Fatal("cache classification wrong")
	}
	old := daysAgo(8)
	mustSave(t, entry(cache, "temp", old, 0), entry(oldFile, "temp", old, 1))
	s := mustQuick(t)
	if gone(filepath.Join(cache, "terminal")) || !gone(oldFile) || s.Deleted != 1 || len(mustLoad(t)) != 0 {
		t.Fatalf("summary %+v", s)
	}
	log, _ := os.ReadFile(filepath.Join(h, "disk-cleanup", "cleanup.log"))
	if !strings.Contains(string(log), "SKIPPED") {
		t.Fatal("no SKIPPED line")
	}
}

func TestQuickDropsStaleTrackedWorktreeEntryInsteadOfDeleting(t *testing.T) {
	h := isolate(t)
	wt := filepath.Join(h, "worktrees", "repro-wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: /elsewhere/main/.git/worktrees/repro-wt\n")
	f := filepath.Join(wt, "test_durable.py")
	write(t, f, "x")
	if mustGuess(t, f) != "" {
		t.Fatal("worktree file classified")
	}
	mustSave(t, entry(f, "test", daysAgo(0), 1))
	s := mustQuick(t)
	if gone(f) || s.Deleted != 0 || len(mustLoad(t)) != 0 {
		t.Fatalf("summary %+v", s)
	}
}

func TestScratchOutsideGitTreesStillCleaned(t *testing.T) {
	h := isolate(t)
	os.Mkdir(filepath.Join(filepath.Dir(h), ".git"), 0o755)
	scratch := filepath.Join(h, "test_scratch.py")
	write(t, scratch, "x")
	if mustGuess(t, scratch) != "test" {
		t.Fatal("scratch not test")
	}
	mustSave(t, entry(scratch, "test", daysAgo(0), 1))
	if s := mustQuick(t); !gone(scratch) || s.Deleted != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestTrackedFileInHomeCheckoutIsNeverDisposable(t *testing.T) {
	h := isolate(t)
	runGit(t, "init", "-q", h)
	tracked := filepath.Join(h, "scripts", "test_committed.py")
	write(t, tracked, "x")
	scratch := filepath.Join(h, "test_untracked.py")
	write(t, scratch, "x")
	runGit(t, "-C", h, "add", "scripts/test_committed.py")
	inTracked, _ := insideGitWorktree(bg, tracked)
	inScratch, _ := insideGitWorktree(bg, scratch)
	if !inTracked || inScratch || mustGuess(t, tracked) != "" || mustGuess(t, scratch) != "test" {
		t.Fatal("checkout classification wrong")
	}
	for _, mode := range []string{"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"} {
		t.Setenv(mode, "1")
		if in, _ := insideGitWorktree(bg, tracked); !in {
			t.Fatal(mode)
		}
		os.Unsetenv(mode)
	}
	now := daysAgo(0)
	mustSave(t, entry(tracked, "test", now, 1), entry(scratch, "test", now, 1))
	if s := mustQuick(t); gone(tracked) || !gone(scratch) || s.Deleted != 1 {
		t.Fatalf("summary %+v", s)
	}
	write(t, scratch, "x")
	if mustGuess(t, scratch) != "test" {
		t.Fatal("rewritten scratch not test")
	}
	runGit(t, "-C", h, "add", "test_untracked.py")
	if mustGuess(t, scratch) != "" {
		t.Fatal("newly added file still disposable")
	}
	outer := filepath.Join(filepath.Dir(h), "outer")
	nested := filepath.Join(outer, ".icebath", "scripts", "test_x.py")
	write(t, nested, "x")
	runGit(t, "init", "-q", outer)
	runGit(t, "-C", outer, "add", ".")
	t.Setenv("ICEBATH_HOME", filepath.Join(outer, ".icebath"))
	if mustGuess(t, nested) != "" {
		t.Fatal("file tracked by an enclosing repo classified")
	}
}

// Writes tracked.json directly in the upstream JSON shape
func writeTrackedJSON(t *testing.T, h string, items ...Entry) string {
	t.Helper()
	list := make([]any, len(items))
	for i, e := range items {
		list[i] = e
	}
	text, err := pyjson.Dumps(list)
	if err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(h, "disk-cleanup", "tracked.json")
	write(t, tf, text)
	return tf
}

func TestQuickSkipsStaleCronOutputForJobsJSON(t *testing.T) {
	h := isolate(t)
	jobs := filepath.Join(h, "cron", "jobs.json")
	write(t, jobs, `{"jobs": []}`)
	tf := writeTrackedJSON(t, h, pyjson.ObjectOf("path", jobs, "category", "cron-output", "timestamp", "2025-01-01T00:00:00+00:00", "size", int64(123)))
	if s := mustQuick(t); s.Deleted != 0 || gone(jobs) {
		t.Fatalf("summary %+v", s)
	}
	data, _ := os.ReadFile(tf)
	if v, _ := pyjson.Loads(string(data)); len(v.([]any)) != 0 {
		t.Fatalf("remaining %s", data)
	}
}

func TestDryRunOmitsStaleCronOutput(t *testing.T) {
	h := isolate(t)
	jobs := filepath.Join(h, "cron", "jobs.json")
	write(t, jobs, "[]")
	writeTrackedJSON(t, h, pyjson.ObjectOf("path", jobs, "category", "cron-output", "timestamp", "2025-01-01T00:00:00+00:00", "size", int64(123)))
	auto, prompt, err := DryRun(bg)
	if err != nil || len(auto) != 0 || len(prompt) != 0 {
		t.Fatalf("%v %v %v", auto, prompt, err)
	}
}

func TestLegitimateCronOutputStillDeleted(t *testing.T) {
	h := isolate(t)
	run := filepath.Join(h, "cron", "output", "job_1", "run.md")
	write(t, run, "x")
	writeTrackedJSON(t, h, pyjson.ObjectOf("path", run, "category", "cron-output", "timestamp", daysAgo(20), "size", int64(10)))
	if s := mustQuick(t); s.Deleted != 1 || !gone(run) {
		t.Fatalf("summary %+v", s)
	}
}

func TestTrackThenQuickDeletesTest(t *testing.T) {
	h := isolate(t)
	p := filepath.Join(h, "test_a.py")
	write(t, p, "x")
	if !mustTrack(t, p, "test") {
		t.Fatal("not tracked")
	}
	if s := mustQuick(t); s.Deleted != 1 || !gone(p) {
		t.Fatalf("summary %+v", s)
	}
}

func TestForgetRemovesEntry(t *testing.T) {
	h := isolate(t)
	p := filepath.Join(h, "keep.tmp")
	write(t, p, "x")
	mustTrack(t, p, "temp")
	if n, err := Forget(bg, p); n != 1 || err != nil || gone(p) {
		t.Fatalf("forget %d %v", n, err)
	}
}

func TestEmptyStatus(t *testing.T) {
	isolate(t)
	s, err := GetStatus(bg)
	if err != nil || s.TotalTracked != 0 || len(s.Top10) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestStatusWithEntries(t *testing.T) {
	h := isolate(t)
	p := filepath.Join(h, "big.tmp")
	write(t, p, strings.Repeat("y", 100))
	mustTrack(t, p, "temp")
	s, err := GetStatus(bg)
	if err != nil || s.TotalTracked != 1 || len(s.Top10) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	rendered := FormatStatus(s)
	if !strings.Contains(rendered, "temp") || !strings.Contains(rendered, "big.tmp") {
		t.Fatal(rendered)
	}
}

func TestDryRunClassifiesByCategory(t *testing.T) {
	h := isolate(t)
	testF := filepath.Join(h, "test_x.py")
	write(t, testF, "x")
	big := filepath.Join(h, "big.bin")
	write(t, big, strings.Repeat("z", 10))
	mustTrack(t, testF, "test")
	mustTrack(t, big, "other")
	auto, _, err := DryRun(bg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range auto {
		if p, _ := strField(i, "path"); p == testF {
			found = true
		}
	}
	if !found {
		t.Fatal("test file not auto deleted")
	}
}

func TestProtectedCronPathsFollowTheActiveHome(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, h := range []string{a, b} {
		os.MkdirAll(filepath.Join(h, "cron"), 0o755)
	}
	ca, cb := home.WithOverride(bg, a), home.WithOverride(bg, b)
	if ok, _ := isProtectedCronPath(ca, filepath.Join(a, "cron")); !ok {
		t.Fatal("a cron unprotected under a")
	}
	if ok, _ := isProtectedCronPath(cb, filepath.Join(b, "cron")); !ok {
		t.Fatal("b cron unprotected under b")
	}
	if ok, _ := isProtectedCronPath(cb, filepath.Join(a, "cron")); ok {
		t.Fatal("a cron protected under b")
	}
}

func TestLoadTrackedRestoresBackupAndStartsFresh(t *testing.T) {
	h := isolate(t)
	p := filepath.Join(h, "keep.tmp")
	write(t, p, "x")
	mustTrack(t, p, "temp")
	mustTrack(t, p+"2", "temp")
	write(t, p+"2", "y")
	mustTrack(t, p+"2", "temp")
	tf := filepath.Join(h, "disk-cleanup", "tracked.json")
	write(t, tf, "{not json")
	if items := mustLoad(t); len(items) != 1 {
		t.Fatalf("restored %d", len(items))
	}
	os.Remove(tf + ".bak")
	if items := mustLoad(t); len(items) != 0 {
		t.Fatalf("fresh %d", len(items))
	}
	log, _ := os.ReadFile(filepath.Join(h, "disk-cleanup", "cleanup.log"))
	if !strings.Contains(string(log), "restored from .bak") || !strings.Contains(string(log), "no backup") {
		t.Fatal(string(log))
	}
}

func TestFmtSizeUnits(t *testing.T) {
	for n, want := range map[float64]string{0: "0.0 B", 1023: "1023.0 B", 1024: "1.0 KB", 1536: "1.5 KB", 500 * 1024 * 1024: "500.0 MB", 1 << 50: "1.0 PB"} {
		if got := FmtSize(n); got != want {
			t.Fatalf("%v: %q want %q", n, got, want)
		}
	}
}
