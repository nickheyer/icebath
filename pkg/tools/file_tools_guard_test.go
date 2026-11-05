package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// File tool env whose terminal pool, cwd records, and task overrides are wiped when the test ends
func ftgEnv(t *testing.T) (*Env, string) {
	t.Helper()
	env, ws := newFileToolTestEnv(t)
	t.Cleanup(func() {
		termPool.Lock()
		envs := termPool.active
		termPool.active = map[string]Environment{}
		termPool.lastActivity = map[string]time.Time{}
		termPool.Unlock()
		for _, e := range envs {
			e.Cleanup(true)
		}
		sessionCwdRecords.Lock()
		sessionCwdRecords.m = map[string]string{}
		sessionCwdRecords.Unlock()
		taskEnvOverrides.Lock()
		taskEnvOverrides.m = map[string]map[string]any{}
		taskEnvOverrides.Unlock()
	})
	return env, ws
}

// Writes a file or fails the test
func ftgWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Contents of a file or a failed test
func ftgReadDisk(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Read double answering every path with fixed content, line total, and size
func ftgFakeRead(content string, totalLines int, fileSize int64) func(string, int, int) *ReadResult {
	return func(string, int, int) *ReadResult {
		return &ReadResult{Content: content, TotalLines: totalLines, FileSize: fileSize}
	}
}

// Decoded read_file result
func ftgRead(t *testing.T, env *Env, path string, offset, limit any, taskID string) map[string]any {
	t.Helper()
	return ftDecode(t, readFileTool(context.Background(), env, path, offset, limit, taskID))
}

// Decoded write_file result
func ftgWrite(t *testing.T, env *Env, path, content, taskID string) map[string]any {
	t.Helper()
	return ftDecode(t, writeFileTool(context.Background(), env, path, content, taskID, false, ""))
}

// Decoded replace mode patch result
func ftgPatchReplace(t *testing.T, env *Env, path, oldString, newString, taskID string) map[string]any {
	t.Helper()
	return ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "replace", path: path, oldString: &oldString, newString: &newString}, taskID, nil))
}

// Counts file backend lookups while still serving the real backend
func ftgCountFileOps(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := getFileOps
	getFileOps = func(env *Env, taskID string) (*fileOpsHandle, error) {
		n++
		return prev(env, taskID)
	}
	t.Cleanup(func() { getFileOps = prev })
	return &n
}

// Routes every task's file backend to a real local backend whose shell starts in the given directory
func ftgLocalBackendAt(t *testing.T, env *Env, cwd string) {
	t.Helper()
	l := NewLocalEnvironment(context.Background(), env, cwd, 60, nil, false)
	t.Cleanup(func() { l.Cleanup(false) })
	handle := newFileOpsHandle(env, l, "")
	prev := getFileOps
	getFileOps = func(*Env, string) (*fileOpsHandle, error) { return handle, nil }
	t.Cleanup(func() { getFileOps = prev })
}

// Writes file_read_max_chars into the profile config and drops the config cache
func ftgSetMaxReadChars(t *testing.T, env *Env, n int) {
	t.Helper()
	ftgWriteFile(t, filepath.Join(env.Home, "config.yaml"), fmt.Sprintf("file_read_max_chars: %d\n", n))
	config.ResetCache()
}

// Rewrites a file so its mtime provably differs from the stamp taken before the write
func ftgModifyExternally(t *testing.T, path, content string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := st.ModTime()
	ftgWriteFile(t, path, content)
	if st, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if st.ModTime().Equal(before) {
		bumped := before.Add(time.Second)
		if err := os.Chtimes(path, bumped, bumped); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs a read on a worker and fails when it is still running at the deadline
func ftgReadWithinDeadline(t *testing.T, env *Env, path, taskID string) map[string]any {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- readFileTool(context.Background(), env, path, nil, nil, taskID) }()
	select {
	case out := <-done:
		return ftDecode(t, out)
	case <-time.After(20 * time.Second):
		t.Fatalf("readFileTool(%q) still running after 20s, the read blocked", path)
	}
	return nil
}

// Error text of a result, empty when absent
func ftgErr(r map[string]any) string {
	s, _ := r["error"].(string)
	return s
}

// Blocked device verdict for a path, failing the test on a readlink encode error
func ftgBlocked(t *testing.T, p string) bool {
	t.Helper()
	blocked, err := isBlockedDevice(nil, p, "")
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

func TestFileToolsGuardBlockedDeviceDetection(t *testing.T) {
	for _, dev := range []string{"/dev/zero", "/dev/random", "/dev/urandom", "/dev/stdin",
		"/dev/tty", "/dev/console", "/dev/stdout", "/dev/stderr",
		"/dev/fd/0", "/dev/fd/1", "/dev/fd/2"} {
		if !ftgBlocked(t, dev) {
			t.Errorf("%s should be blocked", dev)
		}
	}
}

func TestFileToolsGuardSafeDeviceNotBlocked(t *testing.T) {
	if ftgBlocked(t, "/dev/null") || ftgBlocked(t, "/dev/sda1") {
		t.Fatal("/dev/null and /dev/sda1 must not be blocked")
	}
}

func TestFileToolsGuardProcFdBlocked(t *testing.T) {
	if !ftgBlocked(t, "/proc/self/fd/0") || !ftgBlocked(t, "/proc/12345/fd/2") {
		t.Fatal("proc fd aliases must be blocked")
	}
}

func TestFileToolsGuardProcSensitivePseudoFilesBlocked(t *testing.T) {
	for _, p := range []string{
		"/proc/self/environ", "/proc/12345/environ",
		"/proc/self/cmdline", "/proc/99/cmdline",
		"/proc/self/maps", "/proc/1/maps",
		"/proc/self/smaps", "/proc/12345/smaps",
		"/proc/self/smaps_rollup", "/proc/99/smaps_rollup",
		"/proc/self/numa_maps", "/proc/1/numa_maps",
		"/proc/self/mem", "/proc/12345/mem",
		"/proc/self/auxv", "/proc/1/auxv",
		"/proc/self/pagemap", "/proc/99/pagemap",
	} {
		if !ftgBlocked(t, p) {
			t.Errorf("%s should be blocked", p)
		}
	}
}

func TestFileToolsGuardProcTaskThreadSensitiveFilesBlocked(t *testing.T) {
	for _, p := range []string{
		"/proc/self/task/1234/maps", "/proc/self/task/1234/smaps", "/proc/self/task/1234/auxv",
		"/proc/self/task/1234/pagemap", "/proc/self/task/1234/environ",
	} {
		if !ftgBlocked(t, p) {
			t.Errorf("%s should be blocked", p)
		}
	}
}

func TestFileToolsGuardProcLegitimateFilesNotBlocked(t *testing.T) {
	for _, p := range []string{"/proc/cpuinfo", "/proc/meminfo", "/proc/uptime", "/proc/version"} {
		if ftgBlocked(t, p) {
			t.Errorf("%s should not be blocked", p)
		}
	}
}

func TestFileToolsGuardNormpathAliasToBlockedDeviceIsBlocked(t *testing.T) {
	if !ftgBlocked(t, "/dev/../dev/zero") || !ftgBlocked(t, "/dev/./urandom") {
		t.Fatal("normpath aliases of blocked devices must be blocked")
	}
}

func TestFileToolsGuardNormalFilesNotBlocked(t *testing.T) {
	if ftgBlocked(t, "/tmp/test.py") || ftgBlocked(t, "/home/user/.bashrc") {
		t.Fatal("ordinary files must not be blocked")
	}
}

func TestFileToolsGuardSymlinkToBlockedDeviceIsBlocked(t *testing.T) {
	link := filepath.Join(t.TempDir(), "zero-link")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	if !ftgBlocked(t, link) {
		t.Fatal("a symlink to /dev/zero must be blocked")
	}
}

func TestFileToolsGuardSymlinkToRegularFileNotBlocked(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "regular.txt"), filepath.Join(dir, "regular-link")
	ftgWriteFile(t, target, "safe\n")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if ftgBlocked(t, link) {
		t.Fatal("a symlink to a regular file must not be blocked")
	}
}

func TestFileToolsGuardReadFileToolRejectsDevice(t *testing.T) {
	env, _ := ftgEnv(t)
	r := ftgRead(t, env, "/dev/zero", nil, nil, "dev_test")
	if !strings.Contains(ftgErr(r), "device file") {
		t.Fatalf("expected device file error, got %v", r)
	}
}

func TestFileToolsGuardReadFileToolRejectsDeviceSymlinkBeforeIO(t *testing.T) {
	env, _ := ftgEnv(t)
	calls := ftgCountFileOps(t)
	link := filepath.Join(t.TempDir(), "zero-link")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	r := ftgRead(t, env, link, nil, nil, "dev_link_test")
	if !strings.Contains(ftgErr(r), "device file") {
		t.Fatalf("expected device file error, got %v", r)
	}
	if *calls != 0 {
		t.Fatalf("file backend looked up %d times before the device guard", *calls)
	}
}

func TestFileToolsGuardReadFileToolRejectsTaskCwdRelativeDeviceAliasSymlink(t *testing.T) {
	if _, err := os.Lstat("/dev/stdin"); err != nil {
		t.Fatal(err)
	}
	env, _ := ftgEnv(t)
	calls := ftgCountFileOps(t)
	tmp := t.TempDir()
	workspace, processCwd := filepath.Join(tmp, "workspace"), filepath.Join(tmp, "process")
	for _, d := range []string{workspace, processCwd} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/dev/../dev/stdin", filepath.Join(workspace, "stdin-link")); err != nil {
		t.Fatal(err)
	}
	env.Cwd = processCwd
	t.Setenv("TERMINAL_CWD", workspace)
	r := ftgRead(t, env, "stdin-link", nil, nil, "dev_rel_link_test")
	if !strings.Contains(ftgErr(r), "device file") {
		t.Fatalf("expected device file error, got %v", r)
	}
	if *calls != 0 {
		t.Fatalf("file backend looked up %d times before the device guard", *calls)
	}
}

func TestFileToolsGuardReadFileToolOnFifoErrorsInsteadOfBlocking(t *testing.T) {
	env, _ := ftgEnv(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v %s", err, out)
	}
	r := ftgReadWithinDeadline(t, env, fifo, "fifo_read_test")
	surface := ftgErr(r)
	if surface == "" {
		surface, _ = r["note"].(string)
	}
	if surface == "" {
		t.Fatalf("expected error or note, got %v", r)
	}
	if !strings.Contains(surface, "not a regular file") {
		t.Fatalf("surface %q", surface)
	}
}

func TestFileToolsGuardReadFileToolOnDirectoryErrorsInsteadOfBlocking(t *testing.T) {
	env, _ := ftgEnv(t)
	r := ftgReadWithinDeadline(t, env, t.TempDir(), "dir_read_test")
	if !strings.Contains(ftgErr(r), "not a regular file") {
		t.Fatalf("expected not a regular file error, got %v", r)
	}
}

func TestFileToolsGuardRegularFileStillReads(t *testing.T) {
	env, _ := ftgEnv(t)
	target := filepath.Join(t.TempDir(), "notes.txt")
	ftgWriteFile(t, target, "first line\nsecond line\n")
	r := ftgReadWithinDeadline(t, env, target, "regular_read_test")
	if _, ok := r["error"]; ok {
		t.Fatalf("unexpected error %v", r)
	}
	if content, _ := r["content"].(string); !strings.Contains(content, "second line") {
		t.Fatalf("content %v", r)
	}
}

func TestFileToolsGuardMissingFileStillReportsNotFound(t *testing.T) {
	env, _ := ftgEnv(t)
	r := ftgReadWithinDeadline(t, env, filepath.Join(t.TempDir(), "no-such-file.txt"), "missing_read_test")
	if _, ok := r["error"]; !ok {
		t.Fatalf("expected an error, got %v", r)
	}
	if strings.Contains(ftgErr(r), "not a regular file") {
		t.Fatalf("missing path must keep the not found wording, got %q", ftgErr(r))
	}
}

func TestFileToolsGuardOversizedMultilineReadTruncatedWithContinuation(t *testing.T) {
	env, ws := ftgEnv(t)
	ftgSetMaxReadChars(t, env, 1000)
	lines := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("%d|", i)+strings.Repeat("z", 98))
	}
	big := strings.Join(lines, "\n")
	installScriptedFileOps(t, env, &scriptedFileOps{readFileFn: ftgFakeRead(big, 50, int64(len(big)))}, nil)
	r := ftgRead(t, env, filepath.Join(ws, "huge.txt"), nil, nil, "big")
	if _, ok := r["error"]; ok {
		t.Fatalf("oversized read must not be rejected, got %v", r)
	}
	content, _ := r["content"].(string)
	if content == "" {
		t.Fatalf("content missing %v", r)
	}
	if r["truncated"] != true || r["truncated_by"] != "bytes" {
		t.Fatalf("truncation metadata %v", r)
	}
	next, ok := r["next_offset"].(float64)
	if !ok || next <= 1 {
		t.Fatalf("next_offset %v", r["next_offset"])
	}
	if n := len([]rune(content)); n > 1000 {
		t.Fatalf("content %d chars exceeds the budget", n)
	}
	if hint, _ := r["hint"].(string); !strings.Contains(hint, "offset") {
		t.Fatalf("hint %q", hint)
	}
}

func TestFileToolsGuardContentUnderLimitPasses(t *testing.T) {
	env, ws := ftgEnv(t)
	body := strings.Repeat("y", defaultMaxReadChars-1)
	installScriptedFileOps(t, env, &scriptedFileOps{readFileFn: ftgFakeRead(body, 1, int64(defaultMaxReadChars-1))}, nil)
	r := ftgRead(t, env, filepath.Join(ws, "justunder.txt"), nil, nil, "under")
	if _, ok := r["error"]; ok {
		t.Fatalf("unexpected error %v", r)
	}
	if _, ok := r["content"]; !ok {
		t.Fatalf("content missing %v", r)
	}
}

// Env with a file on disk whose every read is answered by the scripted content
func ftgDedupFixture(t *testing.T, name, disk, served string, size int64) (*Env, string, *scriptedFileOps) {
	t.Helper()
	env, _ := ftgEnv(t)
	p := filepath.Join(t.TempDir(), name)
	ftgWriteFile(t, p, disk)
	ops := &scriptedFileOps{readFileFn: ftgFakeRead(served, 1, size)}
	installScriptedFileOps(t, env, ops, nil)
	return env, p, ops
}

func TestFileToolsGuardSecondReadReturnsDedupStub(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "dedup_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	r1 := ftgRead(t, env, p, nil, nil, "dup")
	if _, ok := r1["dedup"]; ok {
		t.Fatalf("first read must carry content, got %v", r1)
	}
	r2 := ftgRead(t, env, p, nil, nil, "dup")
	if r2["dedup"] != true || r2["status"] != "unchanged" {
		t.Fatalf("second read should return the dedup stub, got %v", r2)
	}
	if msg, _ := r2["message"].(string); !strings.Contains(msg, "unchanged") {
		t.Fatalf("message %q", msg)
	}
	if r2["content_returned"] != false {
		t.Fatalf("content_returned %v", r2["content_returned"])
	}
	if _, ok := r2["content"]; ok {
		t.Fatalf("stub must not carry content %v", r2)
	}
}

func TestFileToolsGuardBackgroundReviewForkGetsContentAndReadMarkNotStub(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "dedup_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	var marks []string
	prev := MarkBackgroundReviewSkillRead
	MarkBackgroundReviewSkillRead = func(_ *Env, resolved string) { marks = append(marks, resolved) }
	t.Cleanup(func() { MarkBackgroundReviewSkillRead = prev })
	ftgRead(t, env, p, nil, nil, "dup")
	marks = nil
	env.Values.Store(writeOriginValuesKey, backgroundReviewOrigin)
	fork := ftgRead(t, env, p, nil, nil, "dup")
	env.Values.Delete(writeOriginValuesKey)
	if _, ok := fork["dedup"]; ok {
		t.Fatalf("review fork got a dedup stub %v", fork)
	}
	if _, ok := fork["content"]; !ok {
		t.Fatalf("review fork got no content %v", fork)
	}
	if !slices.Contains(marks, resolveNonStrict(p)) {
		t.Fatalf("review fork read was not marked, marks %v", marks)
	}
}

func TestFileToolsGuardWriteRejectsInternalReadStatusText(t *testing.T) {
	env, _ := ftgEnv(t)
	p := filepath.Join(t.TempDir(), "dedup_test.txt")
	ftgWriteFile(t, p, "line one\nline two\n")
	wrote := false
	installScriptedFileOps(t, env, &scriptedFileOps{writeFileFn: func(string, string, *string) *WriteResult {
		wrote = true
		return &WriteResult{}
	}}, nil)
	r := ftgWrite(t, env, p, readDedupStatusMessage, "guard")
	if _, ok := r["error"]; !ok {
		t.Fatalf("internal status text must be refused, got %v", r)
	}
	if wrote {
		t.Fatal("backend write ran for internal status text")
	}
}

func TestFileToolsGuardDifferentTaskNotDeduped(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "dedup_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, nil, nil, "task_a")
	if r := ftgRead(t, env, p, nil, nil, "task_b"); r["dedup"] == true {
		t.Fatalf("other task must not be deduped %v", r)
	}
}

func TestFileToolsGuardThirdReadIsBlocked(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	r1 := ftgRead(t, env, p, nil, nil, "loop")
	if _, ok := r1["dedup"]; ok {
		t.Fatalf("first read %v", r1)
	}
	if _, ok := r1["error"]; ok {
		t.Fatalf("first read %v", r1)
	}
	r2 := ftgRead(t, env, p, nil, nil, "loop")
	if r2["dedup"] != true {
		t.Fatalf("second read %v", r2)
	}
	if _, ok := r2["error"]; ok {
		t.Fatalf("second read %v", r2)
	}
	r3 := ftgRead(t, env, p, nil, nil, "loop")
	e := ftgErr(r3)
	if !strings.Contains(e, "BLOCKED") || !strings.Contains(e, "STOP") {
		t.Fatalf("second dedup stub should be BLOCKED, got %v", r3)
	}
	if r3["already_read"] != float64(3) {
		t.Fatalf("already_read %v", r3["already_read"])
	}
	if _, ok := r3["dedup"]; ok {
		t.Fatalf("loop breaker must not be a dedup stub %v", r3)
	}
}

func TestFileToolsGuardSubsequentReadsStayBlocked(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, nil, nil, "loop")
	ftgRead(t, env, p, nil, nil, "loop")
	if r3 := ftgRead(t, env, p, nil, nil, "loop"); ftgErr(r3) == "" {
		t.Fatalf("third read %v", r3)
	}
	for i := 0; i < 5; i++ {
		if rN := ftgRead(t, env, p, nil, nil, "loop"); !strings.Contains(ftgErr(rN), "BLOCKED") {
			t.Fatalf("read %d reverted from BLOCKED %v", i+4, rN)
		}
	}
}

func TestFileToolsGuardFileModificationClearsBlock(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, nil, nil, "loop")
	ftgRead(t, env, p, nil, nil, "loop")
	if r3 := ftgRead(t, env, p, nil, nil, "loop"); ftgErr(r3) == "" {
		t.Fatalf("third read %v", r3)
	}
	time.Sleep(50 * time.Millisecond)
	ftgWriteFile(t, p, "brand new content\n")
	r4 := ftgRead(t, env, p, nil, nil, "loop")
	if _, ok := r4["error"]; ok {
		t.Fatalf("changed file must read again %v", r4)
	}
	if _, ok := r4["dedup"]; ok {
		t.Fatalf("changed file must read again %v", r4)
	}
}

func TestFileToolsGuardOtherToolCallClearsHits(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, nil, nil, "loop")
	ftgRead(t, env, p, nil, nil, "loop")
	NotifyOtherToolCall("loop")
	r3 := ftgRead(t, env, p, nil, nil, "loop")
	if r3["dedup"] != true {
		t.Fatalf("expected a stub again %v", r3)
	}
	if _, ok := r3["error"]; ok {
		t.Fatalf("expected a stub not a block %v", r3)
	}
}

func TestFileToolsGuardDifferentRangesTrackedIndependently(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, 1, 100, "loop")
	ftgRead(t, env, p, 1, 100, "loop")
	if r3 := ftgRead(t, env, p, 1, 100, "loop"); ftgErr(r3) == "" {
		t.Fatalf("third read of the range %v", r3)
	}
	if other := ftgRead(t, env, p, 1, 200, "loop"); ftgErr(other) != "" {
		t.Fatalf("other range must read %v", other)
	}
}

func TestFileToolsGuardResetFileDedupClearsHits(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "loop_test.txt", "line one\nline two\n", "line one\nline two\n", 20)
	ftgRead(t, env, p, nil, nil, "loop")
	ftgRead(t, env, p, nil, nil, "loop")
	if r3 := ftgRead(t, env, p, nil, nil, "loop"); ftgErr(r3) == "" {
		t.Fatalf("third read %v", r3)
	}
	ResetFileDedup("loop")
	r4 := ftgRead(t, env, p, nil, nil, "loop")
	if _, ok := r4["error"]; ok {
		t.Fatalf("post compaction read %v", r4)
	}
	if _, ok := r4["dedup"]; ok {
		t.Fatalf("post compaction read %v", r4)
	}
	if _, ok := r4["content"]; !ok {
		t.Fatalf("post compaction read %v", r4)
	}
	if r5 := ftgRead(t, env, p, nil, nil, "loop"); r5["dedup"] != true {
		t.Fatalf("next unchanged read should dedup %v", r5)
	}
}

func TestFileToolsGuardFirstPostCompactionReadRecoversExactContent(t *testing.T) {
	env, p, _ := ftgDedupFixture(t, "compress_test.txt", "original content\n", "SECRET_EXACT_LINE=42\n", 21)
	ftgRead(t, env, p, nil, nil, "comp")
	if r := ftgRead(t, env, p, nil, nil, "comp"); r["dedup"] != true {
		t.Fatalf("should dedup before reset %v", r)
	}
	ResetFileDedup("comp")
	post := ftgRead(t, env, p, nil, nil, "comp")
	if _, ok := post["dedup"]; ok {
		t.Fatalf("first read after reset %v", post)
	}
	if content, _ := post["content"].(string); !strings.Contains(content, "SECRET_EXACT_LINE=42") {
		t.Fatalf("first read after reset %v", post)
	}
	if again := ftgRead(t, env, p, nil, nil, "comp"); again["dedup"] != true {
		t.Fatalf("later read should dedup %v", again)
	}
}

func TestFileToolsGuardCustomConfigLowersLimit(t *testing.T) {
	env, ws := ftgEnv(t)
	ftgSetMaxReadChars(t, env, 50)
	installScriptedFileOps(t, env, &scriptedFileOps{readFileFn: ftgFakeRead(strings.Repeat("x", 60), 1, 60)}, nil)
	r := ftgRead(t, env, filepath.Join(ws, "cfgtest.txt"), nil, nil, "cfg1")
	if _, ok := r["error"]; ok {
		t.Fatalf("unexpected error %v", r)
	}
	if r["truncated"] != true || r["truncated_by"] != "bytes" {
		t.Fatalf("truncation metadata %v", r)
	}
	if content, _ := r["content"].(string); len([]rune(content)) > 50 {
		t.Fatalf("content %d chars exceeds 50", len([]rune(content)))
	}
}

func TestFileToolsGuardCustomConfigRaisesLimit(t *testing.T) {
	env, ws := ftgEnv(t)
	ftgSetMaxReadChars(t, env, 500_000)
	body := strings.Repeat("y", 200_000)
	installScriptedFileOps(t, env, &scriptedFileOps{readFileFn: ftgFakeRead(body, 1, 200_000)}, nil)
	r := ftgRead(t, env, filepath.Join(ws, "cfgtest2.txt"), nil, nil, "cfg2")
	if _, ok := r["error"]; ok {
		t.Fatalf("unexpected error %v", r)
	}
	if content, _ := r["content"].(string); content != body {
		t.Fatalf("content of %d chars, want the full 200000", len(content))
	}
}

func TestFileToolsGuardWriteInvalidatesDedupSameSecond(t *testing.T) {
	env, _ := ftgEnv(t)
	p := filepath.Join(t.TempDir(), "write_dedup.txt")
	ftgWriteFile(t, p, "original content\n")
	ops := &scriptedFileOps{
		readFileFn:  ftgFakeRead("original content\n", 1, 18),
		writeFileFn: func(string, string, *string) *WriteResult { return &WriteResult{} },
	}
	installScriptedFileOps(t, env, ops, nil)
	if r1 := ftgRead(t, env, p, nil, nil, "wr"); r1["dedup"] == true {
		t.Fatalf("first read %v", r1)
	}
	ftgWrite(t, env, p, "new content\n", "wr")
	ops.readFileFn = ftgFakeRead("new content\n", 1, 13)
	r2 := ftgRead(t, env, p, nil, nil, "wr")
	if r2["dedup"] == true {
		t.Fatalf("read after write must not return dedup stub %v", r2)
	}
	if _, ok := r2["content"]; !ok {
		t.Fatalf("read after write %v", r2)
	}
}

func TestFileToolsGuardWriteInvalidatesAllOffsets(t *testing.T) {
	env, _ := ftgEnv(t)
	p := filepath.Join(t.TempDir(), "write_dedup.txt")
	ftgWriteFile(t, p, "original content\n")
	installScriptedFileOps(t, env, &scriptedFileOps{
		readFileFn:  ftgFakeRead("line1\nline2\nline3\n", 3, 20),
		writeFileFn: func(string, string, *string) *WriteResult { return &WriteResult{} },
	}, nil)
	ftgRead(t, env, p, 1, 100, "off")
	ftgRead(t, env, p, 50, 100, "off")
	ftgRead(t, env, p, 1, 500, "off")
	if w := ftgWrite(t, env, p, "replaced\n", "off"); ftgErr(w) != "" {
		t.Fatalf("write %v", w)
	}
	if r1 := ftgRead(t, env, p, 1, 100, "off"); r1["dedup"] == true {
		t.Fatalf("offset=1 should not dedup after write %v", r1)
	}
	if r2 := ftgRead(t, env, p, 50, 100, "off"); r2["dedup"] == true {
		t.Fatalf("offset=50 should not dedup after write %v", r2)
	}
}

// Env with a file on disk for the staleness tests
func ftgStaleFixture(t *testing.T, name, content string) (*Env, string, string) {
	t.Helper()
	env, _ := ftgEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	ftgWriteFile(t, p, content)
	return env, dir, p
}

func TestFileToolsGuardNoWarningWhenFileUnchanged(t *testing.T) {
	env, _, p := ftgStaleFixture(t, "stale_test.txt", "original content\n")
	installScriptedFileOps(t, env, &scriptedFileOps{
		readFileFn:  ftgFakeRead("original content\n", 1, 18),
		writeFileFn: func(string, string, *string) *WriteResult { return &WriteResult{BytesWritten: 10} },
	}, nil)
	ftgRead(t, env, p, nil, nil, "t1")
	r := ftgWrite(t, env, p, "new content", "t1")
	if _, ok := r["_warning"]; ok {
		t.Fatalf("unexpected warning %v", r)
	}
	if _, ok := r["error"]; ok {
		t.Fatalf("unexpected error %v", r)
	}
}

func TestFileToolsGuardWriteFileRefusesBeforeMutationWhenModifiedExternally(t *testing.T) {
	env, _, p := ftgStaleFixture(t, "stale_test.txt", "original content\n")
	if r := ftgRead(t, env, p, nil, nil, "t1"); ftgErr(r) != "" {
		t.Fatalf("read %v", r)
	}
	ftgModifyExternally(t, p, "someone else changed this\n")
	refused := ftgWrite(t, env, p, "new content\n", "t1")
	if refused["stale_write_blocked"] != true {
		t.Fatalf("expected a stale write refusal %v", refused)
	}
	if got := ftgReadDisk(t, p); got != "someone else changed this\n" {
		t.Fatalf("external edit lost, disk %q", got)
	}
	if r := ftgRead(t, env, p, nil, nil, "t1"); ftgErr(r) != "" {
		t.Fatalf("re-read %v", r)
	}
	if written := ftgWrite(t, env, p, "merged\n", "t1"); ftgErr(written) != "" {
		t.Fatalf("write after re-read %v", written)
	}
	if got := ftgReadDisk(t, p); got != "merged\n" {
		t.Fatalf("disk %q", got)
	}
}

func TestFileToolsGuardWriteFileRequiresFullUnredactedReadOfExistingFile(t *testing.T) {
	env, dir, p := ftgStaleFixture(t, "stale_test.txt", "original content\n")
	if refused := ftgWrite(t, env, p, "x\n", "t2"); refused["stale_write_blocked"] != true {
		t.Fatalf("never read file must be refused %v", refused)
	}
	if patched := ftgPatchReplace(t, env, p, "original", "patched", "t2"); ftgErr(patched) != "" {
		t.Fatalf("patch %v", patched)
	}
	if r := ftgWrite(t, env, p, "x\n", "t2"); r["stale_write_blocked"] != true {
		t.Fatalf("blind patched file must be refused %v", r)
	}
	ftgWriteFile(t, p, "one\ntwo\nthree\n")
	if r := ftgRead(t, env, p, 1, 1, "t2"); ftgErr(r) != "" {
		t.Fatalf("partial read %v", r)
	}
	if r := ftgWrite(t, env, p, "x\n", "t2"); r["stale_write_blocked"] != true {
		t.Fatalf("partially read file must be refused %v", r)
	}
	secret := "ghp_" + strings.Repeat("A", 40)
	ftgWriteFile(t, p, "token="+secret+"\n")
	t.Setenv("ICEBATH_REDACT_SECRETS", "true")
	read := ftgRead(t, env, p, nil, nil, "t2")
	if content, ok := read["content"].(string); !ok || strings.Contains(content, secret) {
		t.Fatalf("secret reached the model or no content %v", read)
	}
	if refused := ftgWrite(t, env, p, "token=\u00abredacted:ghp_\u2026\u00bb\n", "t2"); refused["stale_write_blocked"] != true {
		t.Fatalf("redacted read must not bless an overwrite %v", refused)
	}
	if got := ftgReadDisk(t, p); got != "token="+secret+"\n" {
		t.Fatalf("credential file clobbered, disk %q", got)
	}
	newPath := filepath.Join(dir, "brand_new.txt")
	if r := ftgWrite(t, env, newPath, "one\n", "t2"); ftgErr(r) != "" {
		t.Fatalf("new file write %v", r)
	}
	if r := ftgWrite(t, env, newPath, "two\n", "t2"); ftgErr(r) != "" {
		t.Fatalf("own write must be a baseline %v", r)
	}
	if got := ftgReadDisk(t, newPath); got != "two\n" {
		t.Fatalf("disk %q", got)
	}
}

// Lines of the form prefix i for i from one through n, each newline terminated
func ftgNumberedLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

func TestFileToolsGuardPagedReadOfLargeFileIsAFullBaselineThatSurvivesCompaction(t *testing.T) {
	env, _, p := ftgStaleFixture(t, "stale_test.txt", "original content\n")
	ftgWriteFile(t, p, ftgNumberedLines("line", 2500))
	if first := ftgRead(t, env, p, nil, nil, "t3"); first["truncated"] != true {
		t.Fatalf("first page should be truncated %v", first)
	}
	if r := ftgWrite(t, env, p, "x\n", "t3"); r["stale_write_blocked"] != true {
		t.Fatalf("one page is not a full read %v", r)
	}
	if r := ftgRead(t, env, p, 2001, nil, "t3"); ftgErr(r) != "" {
		t.Fatalf("second page %v", r)
	}
	ResetFileDedup("t3")
	if written := ftgWrite(t, env, p, "merged\n", "t3"); ftgErr(written) != "" {
		t.Fatalf("paged full read must allow the write %v", written)
	}
	if got := ftgReadDisk(t, p); got != "merged\n" {
		t.Fatalf("disk %q", got)
	}
	ftgWriteFile(t, p, ftgNumberedLines("line", 2500))
	ftgRead(t, env, p, nil, nil, "t3")
	ftgModifyExternally(t, p, ftgNumberedLines("other", 2500))
	ftgRead(t, env, p, 2001, nil, "t3")
	refused := ftgWrite(t, env, p, "x\n", "t3")
	if refused["stale_write_blocked"] != true {
		t.Fatalf("edit between pages must void the baseline %v", refused)
	}
	if strings.Contains(ftgErr(refused), "Warning:") {
		t.Fatalf("refusal carries a warning prefix %q", ftgErr(refused))
	}
}

func TestFileToolsGuardRelativePathUsesRecordedSessionCwdForStalenessTracking(t *testing.T) {
	env, dir, _ := ftgStaleFixture(t, "stale_test.txt", "original content\n")
	startDir, liveDir := filepath.Join(dir, "start"), filepath.Join(dir, "worktree")
	for _, d := range []string{startDir, liveDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	liveFile := filepath.Join(liveDir, "shared.txt")
	ftgWriteFile(t, filepath.Join(startDir, "shared.txt"), "start copy\n")
	ftgWriteFile(t, liveFile, "live copy\n")
	wrote := false
	installScriptedFileOps(t, env, &scriptedFileOps{
		readFileFn: ftgFakeRead("live copy\n", 1, 10),
		writeFileFn: func(string, string, *string) *WriteResult {
			wrote = true
			return &WriteResult{}
		},
	}, nil)
	RecordSessionCwd(env, "live_task", liveDir)
	t.Cleanup(func() { ClearSessionCwd(env, "live_task") })
	t.Setenv("TERMINAL_CWD", startDir)
	ftgRead(t, env, "shared.txt", nil, nil, "live_task")
	time.Sleep(50 * time.Millisecond)
	ftgWriteFile(t, liveFile, "live copy modified elsewhere\n")
	r := ftgWrite(t, env, "shared.txt", "replacement", "live_task")
	if r["stale_write_blocked"] != true {
		t.Fatalf("expected a stale write refusal %v", r)
	}
	if wrote {
		t.Fatal("stale content was written")
	}
}

// Scripted backend whose reads serve fixed content and whose replace patches succeed with a diff
func ftgPatchOps(content string, size int64) *scriptedFileOps {
	return &scriptedFileOps{
		readFileFn: ftgFakeRead(content, 1, size),
		patchReplaceFn: func(string, string, string, bool, *editProbe) *PatchResult {
			return &PatchResult{Success: true, Diff: "--- a\n+++ b\n@@ ...\n"}
		},
	}
}

func TestFileToolsGuardPatchWarnsOnStaleFile(t *testing.T) {
	env, _, p := ftgStaleFixture(t, "patch_test.txt", "original line\n")
	installScriptedFileOps(t, env, ftgPatchOps("original line\n", 15), nil)
	ftgRead(t, env, p, nil, nil, "p1")
	time.Sleep(50 * time.Millisecond)
	ftgWriteFile(t, p, "externally modified\n")
	r := ftgPatchReplace(t, env, p, "original", "patched", "p1")
	if w, _ := r["_warning"].(string); !strings.Contains(w, "modified since you last read") {
		t.Fatalf("expected a staleness warning %v", r)
	}
}

func TestFileToolsGuardPatchNoWarningWhenFresh(t *testing.T) {
	env, _, p := ftgStaleFixture(t, "patch_test.txt", "original line\n")
	installScriptedFileOps(t, env, ftgPatchOps("original line\n", 15), nil)
	ftgRead(t, env, p, nil, nil, "p2")
	if r := ftgPatchReplace(t, env, p, "original", "patched", "p2"); r["_warning"] != nil {
		t.Fatalf("unexpected warning %v", r)
	}
}

// Workspace and decoy checkouts with the decoy as the launch directory and no session cwd recorded
func ftgIsolatedCwd(t *testing.T) (*Env, string, string) {
	t.Helper()
	env, _ := ftgEnv(t)
	tmp := t.TempDir()
	workspace, decoy := filepath.Join(tmp, "workspace"), filepath.Join(tmp, "decoy")
	for _, d := range []string{workspace, decoy} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ftgWriteFile(t, filepath.Join(workspace, "target.py"), "WORKSPACE_ORIGINAL\n")
	ftgWriteFile(t, filepath.Join(decoy, "target.py"), "DECOY_ORIGINAL\n")
	env.Cwd = decoy
	t.Setenv("TERMINAL_CWD", "")
	return env, workspace, decoy
}

// Resolves a path for a task or fails the test
func ftgResolve(t *testing.T, env *Env, taskID, path string) string {
	t.Helper()
	got, err := resolvePathForTaskID(env, taskID, path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFileToolsGuardRelativeTerminalCwdAnchorsToAbsoluteNotProcessCwd(t *testing.T) {
	env, _, decoy := ftgIsolatedCwd(t)
	t.Setenv("TERMINAL_CWD", ".")
	resolved := ftgResolve(t, env, "default", "target.py")
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolution base leaked a relative path %q", resolved)
	}
	if want := resolveNonStrict(filepath.Join(decoy, "target.py")); resolved != want {
		t.Fatalf("resolved %q want %q", resolved, want)
	}
}

func TestFileToolsGuardLiveTrackingCwdWinsOverRelativeTerminalCwd(t *testing.T) {
	env, workspace, _ := ftgIsolatedCwd(t)
	t.Setenv("TERMINAL_CWD", ".")
	RecordSessionCwd(env, "default", workspace)
	if got := ftgResolve(t, env, "default", "target.py"); got != filepath.Join(workspace, "target.py") {
		t.Fatalf("resolved %q", got)
	}
}

func TestFileToolsGuardAbsoluteTerminalCwdUsedVerbatim(t *testing.T) {
	env, workspace, _ := ftgIsolatedCwd(t)
	t.Setenv("TERMINAL_CWD", workspace)
	if got := ftgResolve(t, env, "default", "target.py"); got != filepath.Join(workspace, "target.py") {
		t.Fatalf("resolved %q", got)
	}
}

// Host project directory plus a host symlink at a container looking mount path pointing at it
func ftgHostSymlinkedMount(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	hostProject, mount := filepath.Join(tmp, "host-project"), filepath.Join(tmp, "workspace-projects")
	if err := os.Mkdir(hostProject, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostProject, mount); err != nil {
		t.Fatal(err)
	}
	return hostProject, mount
}

func TestFileToolsGuardContainerAbsoluteInputPathDoesNotFollowHostSymlink(t *testing.T) {
	env, _ := ftgEnv(t)
	hostProject, mount := ftgHostSymlinkedMount(t)
	envConfigFn = func(*Env) (map[string]any, error) { return map[string]any{"env_type": "docker"}, nil }
	containerPath := mount + "/oilsands-sim/README.md"
	resolved := ftgResolve(t, env, "default", containerPath)
	if resolved != containerPath {
		t.Fatalf("resolved %q want %q", resolved, containerPath)
	}
	if resolved == filepath.Join(hostProject, "oilsands-sim", "README.md") {
		t.Fatal("container path followed the host symlink")
	}
}

func TestFileToolsGuardContainerPathNormalizationUsesPosixPathSyntax(t *testing.T) {
	if got := posixNormpath("/workspace/projects/foo/../bar"); got != "/workspace/projects/bar" {
		t.Fatalf("normalized %q", got)
	}
}

func TestFileToolsGuardContainerRelativePathKeepsContainerCwdSymlink(t *testing.T) {
	env, _ := ftgEnv(t)
	hostProject, mount := ftgHostSymlinkedMount(t)
	envConfigFn = func(*Env) (map[string]any, error) { return map[string]any{"env_type": "docker"}, nil }
	RecordSessionCwd(env, "default", mount)
	resolved := ftgResolve(t, env, "default", "oilsands-sim/README.md")
	if resolved != mount+"/oilsands-sim/README.md" {
		t.Fatalf("resolved %q", resolved)
	}
	if resolved == filepath.Join(hostProject, "oilsands-sim", "README.md") {
		t.Fatal("container cwd followed the host symlink")
	}
}

func TestFileToolsGuardResolutionBaseAlwaysAbsoluteNoTerminalCwd(t *testing.T) {
	env, _, decoy := ftgIsolatedCwd(t)
	resolved := ftgResolve(t, env, "default", "target.py")
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolution base leaked a relative path %q", resolved)
	}
	if want := resolveNonStrict(filepath.Join(decoy, "target.py")); resolved != want {
		t.Fatalf("resolved %q want %q", resolved, want)
	}
}

func TestFileToolsGuardWarningFiresWhenRelativePathEscapesWorkspace(t *testing.T) {
	env, workspace, decoy := ftgIsolatedCwd(t)
	RecordSessionCwd(env, "default", workspace)
	warn := pathResolutionWarningFor(env, "default", "target.py", filepath.Join(decoy, "target.py"))
	if warn == "" {
		t.Fatal("expected a divergence warning")
	}
	if !strings.Contains(warn, decoy) || !strings.Contains(warn, workspace) {
		t.Fatalf("warning %q", warn)
	}
}

func TestFileToolsGuardWarningFiresFromTerminalCwdWhenRegistryEmpty(t *testing.T) {
	env, workspace, decoy := ftgIsolatedCwd(t)
	t.Setenv("TERMINAL_CWD", workspace)
	escaping, err := filepath.Rel(workspace, filepath.Join(decoy, "target.py"))
	if err != nil {
		t.Fatal(err)
	}
	resolved := ftgResolve(t, env, "default", escaping)
	warn := pathResolutionWarningFor(env, "default", escaping, resolved)
	if warn == "" {
		t.Fatal("expected a divergence warning")
	}
	if !strings.Contains(warn, workspace) {
		t.Fatalf("warning %q", warn)
	}
}

func TestFileToolsGuardUnregisteredSessionNeverInheritsAnotherSessionsRecord(t *testing.T) {
	env, _ := ftgEnv(t)
	tmp := t.TempDir()
	wtA, wtB, mainDir := filepath.Join(tmp, "wt_a"), filepath.Join(tmp, "wt_b"), filepath.Join(tmp, "main")
	for _, d := range []string{wtA, wtB, mainDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		ftgWriteFile(t, filepath.Join(d, "target.py"), filepath.Base(d)+"\n")
	}
	env.Cwd = mainDir
	t.Setenv("TERMINAL_CWD", "")
	if err := RegisterTaskEnvOverrides(env, "sess-a", map[string]any{"cwd": wtA}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTaskEnvOverrides(env, "sess-b", map[string]any{"cwd": wtB}); err != nil {
		t.Fatal(err)
	}
	cacheFakeEnv("default", newFakeTermEnv(wtB, nil))
	resolved := ftgResolve(t, env, "sess-c", "target.py")
	if strings.HasPrefix(resolved, wtA) || strings.HasPrefix(resolved, wtB) {
		t.Fatalf("session c inherited another session's cwd %q", resolved)
	}
	if want := resolveNonStrict(filepath.Join(mainDir, "target.py")); resolved != want {
		t.Fatalf("resolved %q want %q", resolved, want)
	}
}

func TestFileToolsGuardV4APatchAppliesToResolvedWorkspaceNotBackendCwd(t *testing.T) {
	env, workspace, decoy := ftgIsolatedCwd(t)
	taskID := "sess-v4a"
	if err := RegisterTaskEnvOverrides(env, taskID, map[string]any{"cwd": workspace}); err != nil {
		t.Fatal(err)
	}
	ftgLocalBackendAt(t, env, decoy)
	patch := "*** Begin Patch\n*** Update File: target.py\n@@\n-WORKSPACE_ORIGINAL\n+WORKSPACE_PATCHED\n*** End Patch\n"
	out := ftDecode(t, patchTool(context.Background(), env, patchArgs{mode: "patch", patch: patch}, taskID, nil))
	expected := resolveNonStrict(filepath.Join(workspace, "target.py"))
	if truthy(out["error"]) {
		t.Fatalf("patch %v", out)
	}
	if out["resolved_path"] != expected {
		t.Fatalf("resolved_path %v want %q", out["resolved_path"], expected)
	}
	if mod, _ := json.Marshal(out["files_modified"]); string(mod) != `["`+expected+`"]` {
		t.Fatalf("files_modified %s", mod)
	}
	if got := ftgReadDisk(t, filepath.Join(workspace, "target.py")); got != "WORKSPACE_PATCHED\n" {
		t.Fatalf("workspace file %q", got)
	}
	if got := ftgReadDisk(t, filepath.Join(decoy, "target.py")); got != "DECOY_ORIGINAL\n" {
		t.Fatalf("decoy file %q", got)
	}
}

func TestFileToolsGuardV4ADeleteAndMoveActOnASymlinkNotItsTarget(t *testing.T) {
	cases := []struct {
		name, header  string
		safeRoot      bool
		content, dest string
	}{
		{"delete", "*** Delete File: local.yaml", false, "shared: true\n", ""},
		{"delete_trailing_sep", "*** Delete File: local.yaml/", false, "shared: true\n", ""},
		{"delete_trailing_dot", "*** Delete File: local.yaml/.", false, "shared: true\n", ""},
		{"move", "*** Move File: local.yaml -> old.yaml", false, "shared: true\n", "old.yaml"},
		{"update_then_move", "*** Update File: local.yaml\n@@\n-shared: true\n+shared: false\n*** Move File: local.yaml -> old.yaml", false, "shared: false\n", "old.yaml"},
		{"delete_outside_safe_root", "*** Delete File: local.yaml", true, "shared: true\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, workspace, _ := ftgIsolatedCwd(t)
			taskID := "sess-v4a-link"
			if err := RegisterTaskEnvOverrides(env, taskID, map[string]any{"cwd": workspace}); err != nil {
				t.Fatal(err)
			}
			ftgLocalBackendAt(t, env, workspace)
			if err := os.Mkdir(filepath.Join(workspace, "safe"), 0o755); err != nil {
				t.Fatal(err)
			}
			target, link := filepath.Join(workspace, "safe", "base.yaml"), filepath.Join(workspace, "local.yaml")
			ftgWriteFile(t, target, "shared: true\n")
			if err := os.Symlink("safe/base.yaml", link); err != nil {
				t.Fatal(err)
			}
			if tc.safeRoot {
				t.Setenv("ICEBATH_WRITE_SAFE_ROOT", filepath.Join(workspace, "safe"))
			}
			raw := handlePatch(context.Background(), &Call{Name: "patch", Args: map[string]any{
				"mode": "patch", "patch": "*** Begin Patch\n" + tc.header + "\n*** End Patch\n"}, TaskID: taskID, Env: env})
			out := ftDecode(t, raw)
			st, err := os.Lstat(target)
			if err != nil || !st.Mode().IsRegular() {
				t.Fatalf("target is no longer a regular file: %v %v", st, err)
			}
			if got := strings.TrimPrefix(ftgReadDisk(t, target), "\ufeff"); got != tc.content {
				t.Fatalf("target content %q want %q", got, tc.content)
			}
			if tc.safeRoot {
				if out["success"] == true || !strings.Contains(raw, "ICEBATH_WRITE_SAFE_ROOT") {
					t.Fatalf("delete outside the safe root must be refused %v", out)
				}
				if lst, err := os.Lstat(link); err != nil || lst.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("link was removed: %v", err)
				}
				return
			}
			if out["success"] != true {
				t.Fatalf("patch %v", out)
			}
			if _, err := os.Lstat(link); !os.IsNotExist(err) {
				t.Fatalf("link entry still present: %v", err)
			}
			modified, _ := out["files_modified"].([]any)
			if !slices.Contains(modified, any(link)) {
				t.Fatalf("files_modified %v lacks %q", out["files_modified"], link)
			}
			if tc.dest != "" {
				if got, err := os.Readlink(filepath.Join(workspace, tc.dest)); err != nil || got != "safe/base.yaml" {
					t.Fatalf("moved link %q %v", got, err)
				}
			}
		})
	}
}

func TestFileToolsGuardContainerRelativePathWithoutARecordedCwdUsesTheBackendCwd(t *testing.T) {
	for _, tc := range []struct{ name, liveCwd string }{{"no-env", ""}, {"shared-env-moved", "/srv/project-a"}} {
		t.Run(tc.name, func(t *testing.T) {
			env, _, decoy := ftgIsolatedCwd(t)
			envConfigFn = func(*Env) (map[string]any, error) { return map[string]any{"env_type": "modal", "cwd": "/root"}, nil }
			if tc.liveCwd != "" {
				live := newFakeTermEnv(tc.liveCwd, nil)
				live.envType = "modal"
				cacheFakeEnv("default", live)
			}
			resolved := ftgResolve(t, env, "b", "notes/todo.md")
			commandCwd := resolveCommandCwd(env, "", "/root", "b", "modal", "", nil)
			if resolved != "/root/notes/todo.md" {
				t.Fatalf("resolved %q", resolved)
			}
			if posixDirname(posixDirname(resolved)) != commandCwd {
				t.Fatalf("resolved %q is not under the command cwd %q", resolved, commandCwd)
			}
			if strings.HasPrefix(resolved, decoy) {
				t.Fatalf("resolved against the launch directory %q", resolved)
			}
		})
	}
}

// Env with an existing profile home under the icebath home and a separate process HOME directory
func ftgProfileHomeEnv(t *testing.T) (*Env, string, string) {
	t.Helper()
	env, _ := ftgEnv(t)
	profileHome := filepath.Join(env.Home, "home")
	processHome := filepath.Join(t.TempDir(), "process_home")
	for _, d := range []string{profileHome, processHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", processHome)
	t.Setenv("TERMINAL_HOME_MODE", "profile")
	return env, profileHome, processHome
}

func TestFileToolsGuardTildeExpandsToProfileHome(t *testing.T) {
	env, profileHome, _ := ftgProfileHomeEnv(t)
	if got := expandTildeFor(env, "~/scratch/file.txt"); got != profileHome+"/scratch/file.txt" {
		t.Fatalf("expanded %q", got)
	}
}

func TestFileToolsGuardEmptyPathUnchanged(t *testing.T) {
	env, _, _ := ftgProfileHomeEnv(t)
	if got := expandTildeFor(env, ""); got != "" {
		t.Fatalf("expanded %q", got)
	}
}

func TestFileToolsGuardRelativeTildeResolvesToProfileHome(t *testing.T) {
	env, profileHome, _ := ftgProfileHomeEnv(t)
	resolved := ftgResolve(t, env, "test", "~/test_file.txt")
	if !strings.HasPrefix(resolved, profileHome) || strings.Contains(resolved, "process_home") {
		t.Fatalf("resolved %q", resolved)
	}
}

func TestFileToolsGuardAbsoluteTildeInWorkspaceRoot(t *testing.T) {
	env, profileHome, processHome := ftgProfileHomeEnv(t)
	resolved := ftgResolve(t, env, "test", "~/data/config.json")
	if !strings.Contains(resolved, profileHome) || strings.Contains(resolved, processHome) {
		t.Fatalf("resolved %q", resolved)
	}
}

// Decoded search_files result for one scripted backend outcome
func ftgSearch(t *testing.T, env *Env, result *SearchResult, a searchArgs, taskID string) map[string]any {
	t.Helper()
	installScriptedFileOps(t, env, &scriptedFileOps{searchFn: func(string, string, string, string, int, int, string, int, string) *SearchResult {
		return result
	}}, nil)
	return ftDecode(t, searchTool(context.Background(), env, a, taskID))
}

func TestFileToolsGuardSearchTimeoutPartialResultsThroughTheTool(t *testing.T) {
	cases := []struct {
		name, target, outputMode string
		result                   *SearchResult
	}{
		{"files_content", "files", "content", &SearchResult{Files: []string{"src/a.py", "src/b.py"}, TotalCount: 2, Truncated: true, LimitReason: "search_timeout"}},
		{"content_files_only", "content", "files_only", &SearchResult{Files: []string{"src/a.py", "src/b.py"}, TotalCount: 2, Truncated: true, LimitReason: "search_timeout"}},
		{"content_content", "content", "content", &SearchResult{Matches: []SearchMatch{{Path: "src/a.py", LineNumber: 10, Content: "foo"}, {Path: "src/b.py", LineNumber: 20, Content: "foo"}}, TotalCount: 2, Truncated: true, LimitReason: "search_timeout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := ftgEnv(t)
			r := ftgSearch(t, env, tc.result, searchArgs{pattern: "foo", target: tc.target, path: "/big", limit: 50, offset: 0, outputMode: tc.outputMode, order: "discovery"}, "timeout")
			if _, ok := r["error"]; ok {
				t.Fatalf("timed out search must not error %v", r)
			}
			if r["truncated"] != true || r["total_count_is_lower_bound"] != true || r["limit_reason"] != "search_timeout" {
				t.Fatalf("timeout shape %v", r)
			}
			if tc.target == "content" && tc.outputMode == "content" {
				matches, _ := r["matches"].([]any)
				var paths []string
				for _, m := range matches {
					row, _ := m.(map[string]any)
					paths = append(paths, fmt.Sprint(row["path"]))
					if strings.Contains(fmt.Sprint(row["content"]), "timed out") {
						t.Fatalf("timeout marker leaked into %v", row)
					}
				}
				if strings.Join(paths, ",") != "src/a.py,src/b.py" {
					t.Fatalf("matches %v", r["matches"])
				}
			} else {
				files, _ := json.Marshal(r["files"])
				if string(files) != `["src/a.py","src/b.py"]` {
					t.Fatalf("files %s", files)
				}
			}
			if hint, _ := r["_hint"].(string); hint != "Results truncated. Use offset=50 to see more, or narrow with a more specific pattern or file_glob." {
				t.Fatalf("hint %q", hint)
			}
		})
	}
}

func TestFileToolsGuardSearchHardErrorThroughTheTool(t *testing.T) {
	env, _ := ftgEnv(t)
	r := ftgSearch(t, env, &SearchResult{Error: "Search failed: rg: regex parse error:"}, searchArgs{pattern: "[", target: "content", path: "/big", limit: 50, offset: 0, outputMode: "content", order: "discovery"}, "rgerr")
	if r["error"] != "Search failed: rg: regex parse error:" {
		t.Fatalf("error %v", r)
	}
	if _, ok := r["limit_reason"]; ok {
		t.Fatalf("hard failure carries a limit_reason %v", r)
	}
}
