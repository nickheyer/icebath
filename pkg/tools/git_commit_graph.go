package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var commitGraphWrite = []string{"commit-graph", "write", "--reachable", "--split", "--no-changed-paths", "--no-progress"}

const (
	commitGraphMaxAge      = 24 * 3600.0
	commitGraphTimeout     = 600.0
	commitGraphMinInterval = 6 * 3600.0
	commitGraphLock        = "icebath-commit-graph.lock"
	commitGraphFreshStamp  = "icebath-commit-graph.fresh"
	commitGraphKillGrace   = 5 * time.Second
	commitGraphLoggerName  = "hermes_cli.git_commit_graph"
)

// Hidden first argument the detached refresh child carries, dispatched before any other parsing
const CommitGraphRefreshEntry = "__icebath_commit_graph_refresh__"

// Latch proving this binary dispatches the entry, the only state in which a refresh may re-execute it
var commitGraphEntryDispatched atomic.Bool

// Slot file under an exclusive cross process lock, nil without error while another process holds it
func lockedSlot(lock string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if wait {
		if err := pyfs.LockFile(f); err != nil {
			f.Close()
			return nil, nil
		}
		return f, nil
	}
	ok, err := pyfs.TryLockFile(f)
	if err != nil || !ok {
		f.Close()
		return nil, nil
	}
	return f, nil
}

// Drops the slot lock and closes the file, the file itself is never unlinked
func closeSlot(f *os.File) {
	_ = pyfs.UnlockFile(f)
	_ = f.Close()
}

// Exactly one process per clone claims a maintenance job per interval, the slot stamped with its pid
func claimMaintenanceSlot(gitDir, lockName string, interval float64) (bool, error) {
	f, err := lockedSlot(filepath.Join(gitDir, lockName), false)
	if err != nil || f == nil {
		return false, err
	}
	defer closeSlot(f)
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() > 0 && nowSeconds()-float64(st.ModTime().UnixNano())/1e9 < interval {
		return false, nil
	}
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return false, err
	}
	return true, nil
}

// Gives back this process's claim so the next run retries, a claim another process made since is left alone
func releaseMaintenanceSlot(gitDir, lockName string) error {
	f, err := lockedSlot(filepath.Join(gitDir, lockName), true)
	if err != nil || f == nil {
		return err
	}
	defer closeSlot(f)
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if pystr.Strip(string(buf[:n])) == strconv.Itoa(os.Getpid()) {
		return f.Truncate(0)
	}
	return nil
}

// Runs a niced git whose whole process tree dies with the timeout or with this process, killed true on timeout
func runBoundedGit(repoRoot string, args []string, timeout float64, env map[string]string) (int, bool, error) {
	cmd := boundedGitCommand(args)
	cmd.Dir = repoRoot
	if env != nil {
		cmd.Env = mapToEnviron(env)
	}
	stderr, err := os.CreateTemp("", "icebath-git-stderr-")
	if err != nil {
		return 0, false, err
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()
	cmd.Stderr = stderr
	if err := SpawnOwned(cmd, "git "+args[0]); err != nil {
		return 0, false, err
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	killed := false
	select {
	case <-done:
	case <-timer.C:
		terminateProcessTreeGrace(cmd, done, commitGraphKillGrace)
		<-done
		killed = true
	}
	ReleaseProcessGroupOnceEmpty(cmd.Process.Pid, nil, time.Second)
	if killed {
		toolsLogger(nil).Warn(fmt.Sprintf("git %s in %s exceeded %ss; killed", args[0], repoRoot, strconv.FormatFloat(timeout, 'g', -1, 64)))
		return 0, true, nil
	}
	rc := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return 0, false, waitErr
		}
		rc = exit.ExitCode()
	}
	if rc != 0 {
		raw, _ := os.ReadFile(stderr.Name())
		if len(raw) > 4096 {
			raw = raw[len(raw)-4096:]
		}
		lines := pystr.Splitlines(pystr.Strip(pystr.DecodeUTF8Replace(raw)), false)
		cause := "no output"
		if len(lines) > 0 {
			cause = lines[len(lines)-1]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "fatal:") || strings.HasPrefix(lines[i], "error:") {
				cause = lines[i]
				break
			}
		}
		toolsLogger(nil).Warn(fmt.Sprintf("git %s in %s failed (exit %d): %s", args[0], repoRoot, rc, cause))
	}
	return rc, false, nil
}

// Modification time of a regular file in unix seconds, false for anything else
func fileMtime(path string) (float64, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return float64(st.ModTime().UnixNano()) / 1e9, true
}

// No commit graph, or none written or confirmed current for a day
func commitGraphStale(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	var graphs []float64
	for _, p := range []string{filepath.Join(info, "commit-graph"), filepath.Join(info, "commit-graphs", "commit-graph-chain")} {
		if mtime, ok := fileMtime(p); ok {
			graphs = append(graphs, mtime)
		}
	}
	if len(graphs) == 0 {
		return true
	}
	newest := graphs[0]
	for _, m := range graphs[1:] {
		newest = max(newest, m)
	}
	if stamp, ok := fileMtime(filepath.Join(gitDir, commitGraphFreshStamp)); ok {
		newest = max(newest, stamp)
	}
	return nowSeconds()-newest > commitGraphMaxAge
}

// Whether the graph is stale and this process won the refresh slot
func claimCommitGraphRefresh(gitDir string) (bool, error) {
	if !commitGraphStale(gitDir) {
		return false, nil
	}
	return claimMaintenanceSlot(gitDir, commitGraphLock, commitGraphMinInterval)
}

// Whether core.commitGraph is false in the repo, global or system config read with the user's own config
func commitGraphDisabled(repoRoot string) bool {
	env := NoninteractiveGitEnv(nil)
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		delete(env, key)
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	probe := boundedProbeRun([]string{"git", "config", "--type=bool", "--get", "core.commitGraph"}, 10*time.Second, env, repoRoot, nil)
	return probe != nil && pystr.Strip(probe.Stdout) == "false"
}

// Git dir holding the object store, the dir a worktree or submodule .git file leads to, empty for no checkout
func commonGitDir(repoRoot string) (string, error) {
	return updaterecord.GitCommonDir(repoRoot)
}

// Clears what a killed writer stranded, then writes the graph bounded, niced and never lazy fetching
func refreshCommitGraph(repoRoot, gitDir string) error {
	if commitGraphDisabled(repoRoot) {
		toolsLogger(nil).Info(fmt.Sprintf("core.commitGraph=false in %s; not writing a commit-graph", repoRoot))
		return nil
	}
	ClearStaleObjectStoreLocks(gitDir)
	ClearStaleTmpPacks(gitDir, nil)
	toolsLogger(nil).Info("git commit-graph missing or stale — writing it in background")
	env := NoninteractiveGitEnv(nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	rc, killed, err := runBoundedGit(repoRoot, commitGraphWrite, commitGraphTimeout, env)
	if err != nil {
		return err
	}
	if killed || rc != 0 {
		return nil
	}
	if graphOnDisk(gitDir) {
		return touchFile(filepath.Join(gitDir, commitGraphFreshStamp))
	}
	toolsLogger(nil).Info(fmt.Sprintf("git commit-graph write in %s exited 0 but left no graph (no commits yet?)", repoRoot))
	return nil
}

// Creates the file or sets its modification time to now
func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// A split chain whose every layer file exists, or a monolithic graph
func graphOnDisk(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	raw, err := os.ReadFile(filepath.Join(info, "commit-graphs", "commit-graph-chain"))
	if err != nil || !isASCII(raw) {
		_, ok := fileMtime(filepath.Join(info, "commit-graph"))
		return ok
	}
	layers := strings.Fields(string(raw))
	if len(layers) == 0 {
		return false
	}
	for _, layer := range layers {
		if _, ok := fileMtime(filepath.Join(info, "commit-graphs", "graph-"+layer+".graph")); !ok {
			return false
		}
	}
	return true
}

// Whether every byte is seven bit ASCII, the ascii codec acceptance test
func isASCII(raw []byte) bool {
	for _, b := range raw {
		if b >= 0x80 {
			return false
		}
	}
	return true
}

// Log handler opening the home's log file only when there is a failure to record, a lost line is dropped
type logDirOnFailureHandler struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	attrs []slog.Attr
}

// Python logging level name of a slog level
func pyLevelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARNING"
	case level >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

// Only warnings and above reach the file, the root logger level of an unconfigured interpreter
func (h *logDirOnFailureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

// Appends the record as an asctime, level, logger name and message line, creating the log dir on first use
func (h *logDirOnFailureHandler) Handle(_ context.Context, r slog.Record) error {
	name := "root"
	for _, a := range h.attrs {
		if a.Key == "logger" {
			name = a.Value.String()
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			name = a.Value.String()
		}
		return true
	})
	stamp := fmt.Sprintf("%s,%03d", r.Time.Format("2006-01-02 15:04:05"), r.Time.Nanosecond()/1e6)
	line := fmt.Sprintf("%s %s %s: %s\n", stamp, pyLevelName(r.Level), name, r.Message)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		if _, err := home.Mkdir(filepath.Dir(h.path)); err != nil {
			return nil
		}
		f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil
		}
		h.file = f
	}
	_, _ = h.file.WriteString(line)
	return nil
}

// Handler sharing the file with the extra attributes carried into every line
func (h *logDirOnFailureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logDirOnFailureHandler{path: h.path, file: h.file, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

// Groups never change the line format
func (h *logDirOnFailureHandler) WithGroup(string) slog.Handler { return h }

// Body of the detached child, a failure is appended to the home's agent.log and nothing is raised
func refreshInChild(repoRoot, gitDir, logFile string) {
	logger := slog.New(&logDirOnFailureHandler{path: logFile}).With("logger", commitGraphLoggerName)
	slog.SetDefault(logger)
	if err := refreshCommitGraph(repoRoot, gitDir); err != nil {
		logger.Warn(fmt.Sprintf("Background commit-graph refresh of %s failed\n%s", repoRoot, err))
	}
}

// Runs the refresh and exits when argv carries the entry, otherwise records that this binary handles it
func DispatchCommitGraphRefreshEntry() {
	commitGraphEntryDispatched.Store(true)
	if len(os.Args) > 4 && os.Args[1] == CommitGraphRefreshEntry {
		refreshInChild(os.Args[2], os.Args[3], os.Args[4])
		os.Exit(0)
	}
}

// Starts the bounded commit graph refresh in a detached child when due, a failed spawn gives the slot back
func SpawnCommitGraphRefresh(ctx context.Context, repoRoot string) (bool, error) {
	if !commitGraphEntryDispatched.Load() {
		panic("commit-graph refresh would re-execute a binary that never called tools.DispatchCommitGraphRefreshEntry, call it first in main or TestMain")
	}
	gitDir, err := commonGitDir(repoRoot)
	if err != nil {
		return false, err
	}
	if gitDir == "" || !isDirFollow(filepath.Join(gitDir, "objects")) {
		return false, nil
	}
	claimed, err := claimCommitGraphRefresh(gitDir)
	if err != nil || !claimed {
		return false, err
	}
	logFile := filepath.Join(home.Dir(ctx), "logs", "agent.log")
	start := func(breakaway bool) (*exec.Cmd, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(exe, CommitGraphRefreshEntry, repoRoot, gitDir, logFile)
		cmd.Dir = repoRoot
		cmd.SysProcAttr = home.DetachAttr(breakaway)
		return cmd, cmd.Start()
	}
	cmd, err := start(true)
	if err != nil && runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
		cmd, err = start(false)
	}
	if err != nil {
		if rerr := releaseMaintenanceSlot(gitDir, commitGraphLock); rerr != nil {
			return false, errors.Join(err, rerr)
		}
		return false, err
	}
	go func() { _ = cmd.Wait() }()
	return true, nil
}
