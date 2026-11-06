package home

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEnsureScratchDirHandsThePruneToOneChildPerProcess(t *testing.T) {
	spawned := scratchPruneSpawns(t)
	launch := filepath.Join(t.TempDir(), "launch")
	scratch, err := EnsureScratchDir(launch)
	if err != nil || scratch != filepath.Join(launch, "cache", "scratch") {
		t.Fatalf("EnsureScratchDir = %q, %v", scratch, err)
	}
	if st, err := os.Stat(scratch); err != nil || !st.IsDir() {
		t.Fatalf("scratch dir missing: %v", err)
	}
	if _, err := EnsureScratchDir(launch); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 1 || (*spawned)[0] != scratch {
		t.Fatalf("spawned %v, want one prune of %s", *spawned, scratch)
	}
	scratchPrunedOnce.Store(false)
	routed := filepath.Join(t.TempDir(), "routed")
	if got, _ := EnsureScratchDir(routed); got != filepath.Join(routed, "cache", "scratch") {
		t.Fatalf("routed scratch = %q", got)
	}
	if len(*spawned) != 2 || (*spawned)[1] != filepath.Join(routed, "cache", "scratch") {
		t.Fatalf("spawned %v", *spawned)
	}
}

func TestPruneStampThrottlesAcrossProcesses(t *testing.T) {
	spawned := scratchPruneSpawns(t)
	h := t.TempDir()
	scratch := testScratch(t, h)
	writeText(t, filepath.Join(scratch, scratchPruneStamp), "")
	if _, err := EnsureScratchDir(h); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 0 {
		t.Fatalf("a fresh stamp must throttle the prune, spawned %v", *spawned)
	}
	scratchPrunedOnce.Store(false)
	age(t, 2, filepath.Join(scratch, scratchPruneStamp))
	if _, err := EnsureScratchDir(h); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(scratch, scratchPruneStamp))
	if len(*spawned) != 1 || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("a stale stamp must be touched and the prune spawned, spawned %v", *spawned)
	}
}

func TestShortLivedProcessStillCompletesItsDuePrune(t *testing.T) {
	tmp := t.TempDir()
	launch, routed := filepath.Join(tmp, "launch"), filepath.Join(tmp, "routed")
	kept := idleEntry(t, testScratch(t, launch), "launch-lane")
	doomed := idleEntry(t, testScratch(t, routed), "routed-lane")
	shellEntry := idleEntry(t, testScratch(t, routed), "shell-lane")
	shell := testChild("shell", routed)
	shell.Dir = filepath.Join(shellEntry, "sub")
	shell.Env = append(shell.Env, "ICEBATH_HOME="+launch, "ICEBATH_HOME_TEST_CWD="+tmp)
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = shell.Wait(); close(done) }()
	defer func() { _ = shell.Process.Kill(); <-done }()
	logFile := filepath.Join(routed, "logs", "scratch-prune.log")
	waitUntil(t, 60*time.Second, "the detached prune removing the idle entry", func() bool {
		data, err := os.ReadFile(logFile)
		return err == nil && strings.Contains(string(data), "removed "+pathRepr(doomed)+" (")
	})
	if _, err := os.Stat(doomed); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("routed idle entry survived: %v", err)
	}
	for _, p := range []string{kept, shellEntry} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed", p)
		}
	}
	select {
	case <-done:
		t.Fatal("the user's shell was signalled")
	default:
	}
	if log := pruneLog(t, routed); !strings.Contains(log, "kept "+pathRepr(shellEntry)+" — live Icebath process pid="+strconv.Itoa(shell.Process.Pid)) {
		t.Fatal(log)
	}
}

func TestLauncherAncestryStampIsBootRelativeForNonUTF8Name(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs start ticks and a writable comm are Linux facts")
	}
	ready := filepath.Join(t.TempDir(), "ready")
	child := testChild("rename", strings.Repeat("c3a9", 7)+"c3", ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	waitUntil(t, 30*time.Second, "the child renaming itself", func() bool { _, err := os.Stat(ready); return err == nil })
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(child.Process.Pid) + "/stat")
	if err != nil || !strings.Contains(string(stat), "\xc3)") {
		t.Fatalf("name is not cut mid UTF-8: %q %v", stat, err)
	}
	stamp, err := birthStamp(child.Process.Pid)
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	ticks, _ := strconv.ParseFloat(fields[19], 64)
	if err != nil || stamp != ticks {
		t.Fatalf("stamp %v (%v), want ticks %v", stamp, err, ticks)
	}
	if same, err := sameBirth(child.Process.Pid, stamp); err != nil || !same {
		t.Fatalf("sameBirth = %v, %v", same, err)
	}
}

func TestLineageRoundTripsThroughItsEncoding(t *testing.T) {
	chain, ok := LineageWithStartTimes()
	if !ok || len(chain) == 0 || chain[0].pid != os.Getpid() {
		t.Fatalf("lineage %v %v", chain, ok)
	}
	back, err := decodeLineage(encodeLineage(chain, true))
	if err != nil || len(back) != len(chain) || back[0] != chain[0] {
		t.Fatalf("decoded %v, %v", back, err)
	}
	if encodeLineage(nil, false) != "-" {
		t.Fatal("an unknown lineage must encode as a dash")
	}
	if _, err := decodeLineage("12"); err == nil {
		t.Fatal("a malformed lineage must fail")
	}
}

func TestDetachedPassFailureIsOneLogLine(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	if code := RunScratchPrune([]string{scratch, "x:1.0"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if log := pruneLog(t, h); !strings.Contains(log, "scratch prune: detached pass failed (ValueError: \"invalid literal for int() with base 10: 'x'\")") {
		t.Fatal(log)
	}
}

func TestPruneRemovesIdleEntriesAndKeepsTreesWrittenDeepInside(t *testing.T) {
	scratch := testScratch(t, t.TempDir())
	idle, live, fresh := filepath.Join(scratch, "idle"), filepath.Join(scratch, "live"), filepath.Join(scratch, "fresh.txt")
	deep := filepath.Join(live, "lane", "wt")
	writeText(t, filepath.Join(idle, "f"), "x")
	writeText(t, filepath.Join(deep, "log"), "x")
	writeText(t, fresh, "y")
	age(t, 30, idle, filepath.Join(idle, "f"), live, filepath.Join(live, "lane"), deep)
	mustPrune(t, scratch, 1)
	if _, err := os.Stat(idle); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("idle entry survived")
	}
	for _, p := range []string{live, fresh} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
}

func TestPruneIgnoresFreshenedHardlinksSharedWithALiveRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory entries report hard link counts only on POSIX")
	}
	tmp := t.TempDir()
	scratch := testScratch(t, tmp)
	livePack := filepath.Join(tmp, "live-repo", "pack-1.pack")
	writeText(t, livePack, strings.Repeat("P", 64))
	clone := filepath.Join(scratch, "clone", ".git", "objects", "pack")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(livePack, filepath.Join(clone, "pack-1.pack")); err != nil {
		t.Fatal(err)
	}
	age(t, 30, clone, filepath.Dir(clone), filepath.Dir(filepath.Dir(clone)), filepath.Join(scratch, "clone"))
	now := time.Now()
	_ = os.Chtimes(livePack, now, now)
	mustPrune(t, scratch, 1)
	if data, _ := os.ReadFile(livePack); string(data) != strings.Repeat("P", 64) {
		t.Fatal("live pack damaged")
	}
	if _, err := os.Stat(filepath.Join(scratch, "clone")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("idle clone survived")
	}
}

func TestScratchDirUsageBytesCountsFiles(t *testing.T) {
	scratch := testScratch(t, t.TempDir())
	writeText(t, filepath.Join(scratch, "a", "b"), "12345")
	writeText(t, filepath.Join(scratch, "c"), "123")
	if got := ScratchDirUsageBytes(scratch); got != 8 {
		t.Fatalf("usage %d, want 8", got)
	}
	if got := ScratchDirUsageBytes(filepath.Join(scratch, "missing")); got != 0 {
		t.Fatalf("missing dir usage %d", got)
	}
}

func TestPruneRecordsRemovalsAndKillsInScratchPruneLog(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	gone, stuck := filepath.Join(scratch, "idle-lane"), filepath.Join(scratch, "stuck-lane")
	for _, entry := range []string{gone, stuck} {
		writeText(t, filepath.Join(entry, "out.md"), "deliverable")
		age(t, 30, filepath.Join(entry, "out.md"), entry)
	}
	worker := startSleeper(t, gone)
	swap(t, &rmtreeStep, func(p string) []string {
		if realpath(p) == realpath(stuck) {
			return nil
		}
		return rmtreeCollectingPermissionErrors(p)
	})
	mustPrune(t, scratch, 1)
	if !exitedWithin(worker, 10*time.Second) {
		t.Fatal("worker living in the idle entry was not reaped")
	}
	log := pruneLog(t, h)
	for _, want := range []string{"removed " + pathRepr(gone) + " (", "sent TERM pid=" + strconv.Itoa(worker.Process.Pid) + " ", "could not fully remove " + pathRepr(stuck)} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in %s", want, log)
		}
	}
	if strings.Contains(log, "removed "+pathRepr(stuck)) {
		t.Fatal(log)
	}
}

func TestPruneLogKeepsRecordOfNonUTF8Name(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS and Windows filesystems reject non UTF-8 names")
	}
	h := t.TempDir()
	scratch := testScratch(t, h)
	raw := filepath.Join(scratch, "old-\xff")
	writeText(t, raw, "x")
	age(t, 30, raw)
	mustPrune(t, scratch, 1)
	if log := pruneLog(t, h); !strings.Contains(log, "removed ") || !strings.Contains(log, `old-\udcff`) {
		t.Fatal(log)
	}
}

func TestPruneLogEscapesNewlineInName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Windows rejects newlines in names")
	}
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "old\nscratch prune: removed fake-entry")
	writeText(t, entry, "x")
	age(t, 30, entry)
	mustPrune(t, scratch, 1)
	lines := strings.Split(strings.TrimRight(pruneLog(t, h), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `old\nscratch prune: removed fake-entry`) {
		t.Fatalf("%q", lines)
	}
}

func TestPruneLogKeepsRecordWhenAnotherPruneRotatedFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rename semantics")
	}
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "idle-lane")
	writeText(t, entry, "x")
	age(t, 30, entry)
	log := filepath.Join(h, "logs", "scratch-prune.log")
	writeText(t, log, strings.Repeat("x", 200))
	swap(t, &pruneLogMaxBytes, 100)
	var raced []string
	swap(t, &scratchRename, func(src, dst string) error {
		if src == log && len(raced) == 0 {
			raced = append(raced, dst)
			_ = os.Rename(src, dst)
		}
		return os.Rename(src, dst)
	})
	mustPrune(t, scratch, 1)
	if len(raced) == 0 {
		t.Fatal("rotation never ran")
	}
	matches, _ := filepath.Glob(log + "*")
	var records strings.Builder
	for _, m := range matches {
		data, _ := os.ReadFile(m)
		records.Write(data)
	}
	if !strings.Contains(records.String(), "removed "+pathRepr(entry)+" (") {
		t.Fatal(records.String())
	}
}

func TestPruneReleasesGitWorktreeRegistrationOfIdleEntry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tmp := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := filepath.Join(tmp, "repo")
	writeText(t, filepath.Join(repo, "f"), "x")
	git(repo, "init", "-q")
	git(repo, "add", "f")
	git(repo, "commit", "-q", "-m", "init")
	scratch := testScratch(t, tmp)
	tree := filepath.Join(scratch, "lane", "abwt")
	if err := os.MkdirAll(filepath.Dir(tree), 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "worktree", "add", "-q", "--detach", tree)
	var paths []string
	_ = filepath.WalkDir(filepath.Dir(tree), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 {
			paths = append(paths, p)
		}
		return nil
	})
	age(t, 30, paths...)
	mustPrune(t, scratch, 1)
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil || strings.Contains(string(out), tree) {
		t.Fatalf("registration kept: %s %v", out, err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("worktree survived")
	}
}

func TestWriteDuringSizeWalkIsRescued(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "sized-lane")
	payload := filepath.Join(entry, "payload")
	writeText(t, payload, "OLD")
	age(t, 48, payload, entry)
	swap(t, &treeBytesStep, func(p string) int64 {
		writeText(t, payload, "FRESH")
		return treeBytes(p)
	})
	mustPrune(t, scratch, 0)
	if data, _ := os.ReadFile(payload); string(data) != "FRESH" {
		t.Fatal("write during size walk was destroyed")
	}
	if log := pruneLog(t, h); !strings.Contains(log, "rescued "+pathRepr(entry)+" ") {
		t.Fatal(log)
	}
}

// Prune of scratch with the reap step replaced by during, returning the log text
func pruneWithReap(t *testing.T, h, scratch string, during func()) string {
	t.Helper()
	swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { during(); return 0, nil })
	mustPrune(t, scratch, 0)
	return pruneLog(t, h)
}

func TestMidpruneResumedWriteIsRescued(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "resumed-lane")
	writeText(t, filepath.Join(entry, "old-output.md"), "stale content\n")
	age(t, 30, filepath.Join(entry, "old-output.md"), entry)
	fresh := filepath.Join(entry, "resumed-work.md")
	log := pruneWithReap(t, h, scratch, func() { writeText(t, fresh, "FRESH RESUMED WORK\n") })
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh mid-prune write was destroyed")
	}
	if !strings.Contains(log, "rescued "+pathRepr(entry)+" ") || strings.Contains(log, "removed "+pathRepr(entry)+" (") {
		t.Fatal(log)
	}
}

func TestMidpruneNestedOverwriteIsRescuedByDescendingRecheck(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "nested-resume-lane")
	deep := filepath.Join(entry, "lane", "wt")
	payload := filepath.Join(deep, "old-output.md")
	writeText(t, payload, "stale content\n")
	age(t, 30, payload, deep, filepath.Dir(deep), entry)
	log := pruneWithReap(t, h, scratch, func() {
		if err := os.WriteFile(payload, []byte("FRESH OVERWRITE, DEEP\n"), 0o644); err != nil {
			t.Error(err)
		}
	})
	if data, _ := os.ReadFile(payload); string(data) != "FRESH OVERWRITE, DEEP\n" {
		t.Fatal("deep mid-prune overwrite was destroyed")
	}
	if !strings.Contains(log, "rescued "+pathRepr(entry)+" ") || strings.Contains(log, "removed "+pathRepr(entry)+" (") {
		t.Fatal(log)
	}
}

func TestMidpruneVanishedEntryIsRecordedNotCounted(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "doomed-lane")
	writeText(t, filepath.Join(entry, "old-output.md"), "stale content\n")
	age(t, 30, filepath.Join(entry, "old-output.md"), entry)
	log := pruneWithReap(t, h, scratch, func() { _ = os.RemoveAll(entry) })
	if !strings.Contains(log, "entry "+pathRepr(entry)+" vanished") || strings.Contains(log, "rescued "+pathRepr(entry)+" ") || strings.Contains(log, "re-scan failed") {
		t.Fatal(log)
	}
}

func TestPostremovalUnreadableEntryIsNotCountedAsRemoved(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	entry := filepath.Join(scratch, "residue-lane")
	payload := filepath.Join(entry, "result.txt")
	writeText(t, payload, "still here")
	age(t, 30, payload, entry)
	deny := false
	swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { return 0, nil })
	swap(t, &rmtreeStep, func(string) []string { deny = true; return nil })
	swap(t, &scratchLstat, func(p string) (fs.FileInfo, error) {
		if deny && p == entry {
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrPermission}
		}
		return os.Lstat(p)
	})
	mustPrune(t, scratch, 0)
	if data, _ := os.ReadFile(payload); string(data) != "still here" {
		t.Fatal("entry damaged")
	}
	log := pruneLog(t, h)
	if strings.Contains(log, "removed "+pathRepr(entry)+" (") || !strings.Contains(log, "could not confirm removal") || strings.Contains(log, "could not fully remove") {
		t.Fatal(log)
	}
}
