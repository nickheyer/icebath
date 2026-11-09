package tools

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Random bytes that git cannot compress below their size
func cpRandom(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// One large oldest snapshot followed by five small ones in the lower hashed of two candidate projects
func cpHistory(t *testing.T) (*cpFixture, string, *CheckpointManager) {
	t.Helper()
	f := newCPFixture(t)
	a, b := filepath.Join(f.tmp, "project-a"), filepath.Join(f.tmp, "project-b")
	project := a
	if checkpointProjectHash(b) < checkpointProjectHash(a) {
		project = b
	}
	f.mkdir(project)
	m := f.mgr(true, 20, 0, 10)
	blob := filepath.Join(project, "old.bin")
	f.write(blob, cpRandom(t, 1_300_000))
	if !m.EnsureCheckpoint(project, "large-oldest") {
		t.Fatal("large snapshot not taken")
	}
	os.Remove(blob)
	for i := 0; i < 5; i++ {
		f.write(filepath.Join(project, "small.txt"), strconv.Itoa(i))
		m.NewTurn()
		if !m.EnsureCheckpoint(project, fmt.Sprintf("small-%d", i)) {
			t.Fatalf("small-%d not taken", i)
		}
	}
	return f, project, m
}

// Reasons small-4 down to small-0
func cpSmallReasons() []string {
	out := []string{}
	for i := 4; i >= 0; i-- {
		out = append(out, fmt.Sprintf("small-%d", i))
	}
	return out
}

func TestCheckpointMaintenanceSizeCapRetainsEverySnapshotAfterTheOnlyLargeOne(t *testing.T) {
	f, project, m := cpHistory(t)
	if checkpointDirSizeBytes(f.store()) <= 1024*1024 {
		t.Fatal("store not over the cap")
	}
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, Base: f.base, MaxTotalSizeMB: 1})
	if r.Errors != 0 || !reflect.DeepEqual(cpReasons(m, project), cpSmallReasons()) {
		t.Fatalf("history wrong: %+v %v\n%s", r, cpReasons(m, project), f.logs.String())
	}
	if checkpointDirSizeBytes(f.store()) > 1024*1024 || f.read(filepath.Join(project, "small.txt")) != "4" {
		t.Fatal("store still over the cap or working tree touched")
	}
}

func TestCheckpointSnapshotOverCapDropsOneRoundAndLeavesTheGCToThePrune(t *testing.T) {
	f, project, m := cpHistory(t)
	m.MaxTotalSizeMB = 1
	var gcCalls [][]string
	real := runCheckpointGit
	f.write(filepath.Join(project, "new.txt"), "new snapshot")
	m.NewTurn()
	runCheckpointGit = func(env *Env, args []string, store, wd string, o checkpointGitOpts) (bool, string, string) {
		if args[0] == "gc" {
			gcCalls = append(gcCalls, args)
		}
		return real(env, args, store, wd, o)
	}
	taken := m.EnsureCheckpoint(project, "newest")
	runCheckpointGit = real
	if !taken || len(gcCalls) != 0 || !f.exists(filepath.Join(f.store(), checkpointGCPendingName)) {
		t.Fatalf("take path ran gc or left no marker: %v %v", taken, gcCalls)
	}
	if want := append([]string{"newest"}, cpSmallReasons()...); !reflect.DeepEqual(cpReasons(m, project), want) {
		t.Fatalf("history wrong: %v", cpReasons(m, project))
	}
	if checkpointDirSizeBytes(f.store()) <= 1024*1024 {
		t.Fatal("objects reclaimed before the prune")
	}
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, Base: f.base, MaxTotalSizeMB: 1})
	if r.Errors != 0 || f.exists(filepath.Join(f.store(), checkpointGCPendingName)) || checkpointDirSizeBytes(f.store()) > 1024*1024 {
		t.Fatalf("prune did not reclaim: %+v", r)
	}
	if f.read(filepath.Join(project, "small.txt")) != "4" {
		t.Fatal("working tree touched")
	}
}

func TestCheckpointReclaimStopsBeforeTouchingAnotherProjectsHistory(t *testing.T) {
	f, first, m := cpHistory(t)
	other := filepath.Join(f.tmp, "project-a")
	if filepath.Base(first) == "project-a" {
		other = filepath.Join(f.tmp, "project-b")
	}
	f.mkdir(other)
	for i := 0; i < 3; i++ {
		f.write(filepath.Join(other, "small.txt"), strconv.Itoa(i))
		m.NewTurn()
		if !m.EnsureCheckpoint(other, fmt.Sprintf("other-%d", i)) {
			t.Fatal("other not taken")
		}
	}
	before := m.ListCheckpoints(other)
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, Base: f.base, MaxTotalSizeMB: 1})
	if r.Errors != 0 || !reflect.DeepEqual(m.ListCheckpoints(other), before) || len(m.ListCheckpoints(first)) != 5 {
		t.Fatalf("reclaim touched the wrong history: %+v", r)
	}
}

func TestCheckpointMaintenanceStopsOnFailedGitWithoutLosingASecondSnapshot(t *testing.T) {
	for _, failure := range []string{"gc", "reflog", "for-each-ref", "rev-list", "log", "update-ref", "delete-ref"} {
		t.Run(failure, func(t *testing.T) {
			f, project, m := cpHistory(t)
			retention := 0
			meta := checkpointProjectMetaPath(f.store(), checkpointProjectHash(project))
			if failure == "delete-ref" {
				state := cpReadJSON(t, meta)
				state["last_touch"] = 1
				cpWriteJSON(t, meta, state)
				retention = 1
			}
			var rewrites [][]string
			real := runCheckpointGit
			runCheckpointGit = func(env *Env, args []string, store, wd string, o checkpointGitOpts) (bool, string, string) {
				if args[0] == failure || (failure == "delete-ref" && len(args) > 1 && args[0] == "update-ref" && args[1] == "-d") {
					return false, "", "injected Git failure"
				}
				ok, out, errText := real(env, args, store, wd, o)
				if args[0] == "update-ref" && ok {
					rewrites = append(rewrites, args)
				}
				return ok, out, errText
			}
			r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: retention, Base: f.base, MaxTotalSizeMB: 1})
			runCheckpointGit = real
			if r.Errors == 0 || len(rewrites) > 1 || len(m.ListCheckpoints(project)) < 5 {
				t.Fatalf("failure %s not contained: %+v rewrites=%d", failure, r, len(rewrites))
			}
			if failure == "delete-ref" && (!f.exists(meta) || r.DeletedStale != 0) {
				t.Fatalf("failed ref delete still removed metadata: %+v", r)
			}
		})
	}
}

func TestCheckpointSnapshotPruningStopsAtAFailedCountRewrite(t *testing.T) {
	f, project, m := cpHistory(t)
	m.MaxSnapshots = 5
	m.MaxTotalSizeMB = 1
	f.write(filepath.Join(project, "later.txt"), "newest")
	m.NewTurn()
	real := runCheckpointGit
	var changed [][]string
	cpSwap(t, &runCheckpointGit, func(env *Env, args []string, store, wd string, o checkpointGitOpts) (bool, string, string) {
		if args[0] == "commit-tree" && o.ExtraEnv != nil {
			return false, "", "injected rewrite failure"
		}
		ok, out, errText := real(env, args, store, wd, o)
		if args[0] == "update-ref" && ok {
			changed = append(changed, args)
		}
		return ok, out, errText
	})
	if !m.EnsureCheckpoint(project, "latest") {
		t.Fatal("latest not taken")
	}
	if len(changed) != 1 || !strings.Contains(f.logs.String(), "injected rewrite failure") {
		t.Fatalf("trim failure not contained: %v\n%s", changed, f.logs.String())
	}
	ok, out, _ := real(f.env, []string{"rev-list", "--count", checkpointRefName(checkpointProjectHash(project))}, f.store(), project, checkpointGitOpts{})
	if n, _ := strconv.Atoi(out); !ok || n != 7 {
		t.Fatalf("history trimmed after a failed rewrite: %q", out)
	}
}

func TestCheckpointRestoreKeepsItsTargetAliveWhileTakingTheSafetySnapshot(t *testing.T) {
	f, project, m := cpHistory(t)
	cps := m.ListCheckpoints(project)
	target := cps[len(cps)-1].Hash
	m.MaxSnapshots = 1
	f.write(filepath.Join(project, "small.txt"), "changed before restore")
	if r := m.Restore(project, target, "", false); !cpSuccess(r) {
		t.Fatalf("restore failed: %v", r)
	}
	if st, err := os.Stat(filepath.Join(project, "old.bin")); err != nil || st.Size() != 1_300_000 {
		t.Fatal("restore target lost to the safety snapshot trim")
	}
}

// Writes the checkpoints section of the profile config
func cpWriteCheckpointConfig(t *testing.T, home string, enabled bool, capMB int) {
	t.Helper()
	os.MkdirAll(home, 0o755)
	body := fmt.Sprintf("checkpoints:\n  enabled: %v\n  max_total_size_mb: %d\n", enabled, capMB)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointFootprintNoticeOnlyWhenEnabledAndOverCap(t *testing.T) {
	f := newCPFixture(t)
	work := filepath.Join(f.tmp, "proj")
	f.write(filepath.Join(work, "blob.bin"), cpRandom(t, 2*1024*1024))
	cpWriteCheckpointConfig(t, f.env.Home, true, 1)
	if !f.mgr(true, 20, 1, 10).EnsureCheckpoint(work, "seed") {
		t.Fatal("seed not taken")
	}
	cpSwap(t, &runCheckpointGit, func(*Env, []string, string, string, checkpointGitOpts) (bool, string, string) {
		panic("A storage notice must not inspect commit history")
	})
	notice := CheckpointFootprintNotice(f.env)
	if notice == "" || !strings.Contains(notice, "across 1 project(s)") {
		t.Fatalf("notice wrong: %q", notice)
	}
	cpWriteCheckpointConfig(t, f.env.Home, true, 500)
	if n := CheckpointFootprintNotice(f.env); n != "" {
		t.Fatalf("healthy store nagged: %q", n)
	}
	cpWriteCheckpointConfig(t, f.env.Home, false, 1)
	if n := CheckpointFootprintNotice(f.env); n != "" {
		t.Fatalf("disabled checkpoints nagged: %q", n)
	}
}

func TestCheckpointAutoPruneFromConfigHonoursTheSection(t *testing.T) {
	f := newCPFixture(t)
	os.MkdirAll(f.env.Home, 0o755)
	os.WriteFile(filepath.Join(f.env.Home, "config.yaml"), []byte("checkpoints:\n  auto_prune: false\n"), 0o644)
	if out := AutoPruneCheckpointsFromConfig(f.env); !out.Skipped || out.Error != "" {
		t.Fatalf("disabled auto prune ran: %+v", out)
	}
	os.WriteFile(filepath.Join(f.env.Home, "config.yaml"), []byte("checkpoints:\n  auto_prune: true\n  retention_days: soon\n"), 0o644)
	if out := AutoPruneCheckpointsFromConfig(f.env); !out.Skipped || !strings.Contains(out.Error, "invalid literal for int()") {
		t.Fatalf("bad retention not reported: %+v", out)
	}
	orphan := cpSeedLegacyRepo(t, f.base, strings.Repeat("abcd", 4), filepath.Join(f.tmp, "gone"), nil)
	os.WriteFile(filepath.Join(f.env.Home, "config.yaml"), []byte("checkpoints:\n  auto_prune: true\n"), 0o644)
	out := AutoPruneCheckpointsFromConfig(f.env)
	if out.Skipped || out.Result == nil || out.Result.DeletedOrphan != 0 || !f.exists(orphan) {
		t.Fatalf("unattended prune deleted an orphan: %+v", out)
	}
}

// Child half of the store ownership test, pausing after write-tree until the parent says publish
func TestCheckpointStoreOwnershipHelper(t *testing.T) {
	if os.Getenv("CP_OWNERSHIP_HELPER") != "1" {
		t.Skip("helper process only")
	}
	base, project := os.Getenv("CP_BASE"), os.Getenv("CP_PROJECT")
	checkpointBaseFn = func(*Env) string { return base }
	real := runCheckpointGit
	stdin := bufio.NewReader(os.Stdin)
	runCheckpointGit = func(env *Env, args []string, store, wd string, o checkpointGitOpts) (bool, string, string) {
		ok, out, errText := real(env, args, store, wd, o)
		if args[0] == "write-tree" && ok {
			fmt.Fprintln(os.Stdout, "tree-written")
			os.Stdout.Sync()
			stdin.ReadString('\n')
		}
		return ok, out, errText
	}
	if !NewCheckpointManager(nil, CheckpointManagerOptions{Enabled: true, MaxSnapshots: 20}).EnsureCheckpoint(project, "concurrent") {
		os.Exit(3)
	}
}

func TestCheckpointActiveSnapshotOwnsStoreUntilItsRefIsPublished(t *testing.T) {
	f := newCPFixture(t)
	project := filepath.Join(f.tmp, "concproj")
	f.write(filepath.Join(project, "initial.txt"), "initial")
	m := f.mgr(true, 20, 0, 10)
	if !m.EnsureCheckpoint(project, "initial") {
		t.Fatal("initial not taken")
	}
	f.write(filepath.Join(project, "concurrent.txt"), "new unpublished bytes")
	child := exec.Command(os.Args[0], "-test.run=^TestCheckpointStoreOwnershipHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "CP_OWNERSHIP_HELPER=1", "CP_BASE="+f.base, "CP_PROJECT="+project)
	stdin, _ := child.StdinPipe()
	stdout, _ := child.StdoutPipe()
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
		io.Copy(io.Discard, stdout)
	}()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "tree-written" {
			t.Fatalf("child did not pause: %q %s", line, stderr.String())
		}
	case <-time.After(20 * time.Second):
		child.Process.Kill()
		t.Fatal("child never wrote its tree")
	}
	r, _ := PruneCheckpoints(f.env, CheckpointPruneOptions{RetentionDays: 0, DeleteOrphans: true, Base: f.base})
	cleared := ClearAllCheckpoints(f.env, f.base)
	io.WriteString(stdin, "publish\n")
	stdin.Close()
	werr := child.Wait()
	if r.Errors == 0 {
		t.Fatal("maintenance did not refuse an active store writer")
	}
	if cleared.Deleted {
		t.Fatal("clear removed a store an active writer owns")
	}
	if werr != nil {
		t.Fatalf("child failed: %v %s", werr, stderr.String())
	}
	if got := cpReasons(m, project); !reflect.DeepEqual(got, []string{"concurrent", "initial"}) {
		t.Fatalf("history wrong: %v", got)
	}
}

func TestCheckpointClearAllRemovesTreeWithReadOnlyObject(t *testing.T) {
	root := filepath.Join(cpTempDir(t), "checkpoints")
	objDir := filepath.Join(root, ".git", "objects", "4b")
	os.MkdirAll(objDir, 0o755)
	obj := filepath.Join(objDir, "825dc642cb6eb9a060e54bf8d69288fbee4904")
	os.WriteFile(obj, []byte("blob"), 0o644)
	os.Chmod(obj, 0o444)
	os.Chmod(objDir, 0o555)
	t.Cleanup(func() { os.Chmod(objDir, 0o755) })
	if st, _ := os.Stat(obj); st.Mode()&0o200 != 0 {
		t.Fatal("object still writable")
	}
	if out := ClearAllCheckpoints(nil, root); !out.Deleted {
		t.Fatalf("read-only tree not cleared: %+v", out)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
}

func TestCheckpointRmtreeRemovesReadOnlyFileInWritableDirectory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Git for Windows shape")
	}
	root := filepath.Join(cpTempDir(t), "plugins", "demo")
	objDir := filepath.Join(root, ".git", "objects", "4b")
	os.MkdirAll(objDir, 0o755)
	obj := filepath.Join(objDir, "825dc642cb6eb9a060e54bf8d69288fbee4904")
	os.WriteFile(obj, []byte("blob"), 0o644)
	os.Chmod(obj, 0o444)
	if err := rmtreeReadonly(root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
}

func TestCheckpointRmtreeNonPermissionFailuresPropagate(t *testing.T) {
	dir := cpTempDir(t)
	var attempts []string
	cpSwap(t, &rmtreeRemove, func(p string) error {
		attempts = append(attempts, p)
		return &os.PathError{Op: "rmdir", Path: p, Err: syscall.ENOTEMPTY}
	})
	err := rmtreeReadonly(dir, false)
	if !errors.Is(err, syscall.ENOTEMPTY) || len(attempts) != 1 {
		t.Fatalf("non permission failure retried or swallowed: %v %v", err, attempts)
	}
	if rmtreeReadonly(dir, true) != nil {
		t.Fatal("ignore errors still returned an error")
	}
}

func TestCheckpointRmtreeRefusesSymlinkRoot(t *testing.T) {
	tmp := cpTempDir(t)
	target := filepath.Join(tmp, "target")
	os.MkdirAll(target, 0o755)
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := rmtreeReadonly(link, false); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink root not refused: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("symlink target removed")
	}
}

func TestCheckpointSizefmtBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1234567: "1.2 MB", 5 * 1024 * 1024 * 1024: "5.0 GB", 3 * 1024 * 1024 * 1024 * 1024: "3.0 TB"} {
		if got := sizefmtBytes(n); got != want {
			t.Errorf("sizefmtBytes(%d) = %q want %q", n, got, want)
		}
	}
}

func TestSizefmtBytesExportsTheHumanSize(t *testing.T) {
	if SizefmtBytes(0) != sizefmtBytes(0) || SizefmtBytes(1536) != sizefmtBytes(1536) {
		t.Fatal("the export must read the same formatter")
	}
}
