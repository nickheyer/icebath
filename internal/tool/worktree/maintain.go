package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	packSprawlThreshold = 15
	repackTimeout       = 1800 * time.Second
	repackInterval      = 6 * time.Hour
	repackLock          = "icebath-repack.lock"
	graphMaxAge         = 24 * time.Hour
	graphTimeout        = 600 * time.Second
	graphInterval       = 6 * time.Hour
	graphLock           = "icebath-commit-graph.lock"
	graphFreshStamp     = "icebath-commit-graph.fresh"
	killGrace           = 5 * time.Second
	staleDebrisAge      = 10 * time.Minute
)

var (
	tmpPackPrefixes      = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_", ".tmp-"}
	objectStoreLockNames = []string{
		"objects/info/commit-graph.lock",
		"objects/info/commit-graphs/commit-graph-chain.lock",
		"objects/pack/multi-pack-index.lock",
		"objects/pack/multi-pack-index.d/multi-pack-index-chain.lock",
	}
	commitGraphWrite = []string{"commit-graph", "write", "--reachable", "--split", "--no-changed-paths", "--no-progress"}
	pathSplit        = regexp.MustCompile(`[\\/]`)
)

func commonGitDir(repoRoot string) (string, bool) {
	dot := filepath.Join(repoRoot, ".git")
	info, err := os.Stat(dot)
	if err != nil {
		return "", false
	}
	gitDir := dot
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return "", false
		}
		data, err := os.ReadFile(dot)
		if err != nil {
			return "", false
		}
		text := strings.TrimSpace(string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})))
		if !strings.HasPrefix(text, "gitdir:") {
			return "", false
		}
		gitDir = joinPath(repoRoot, strings.TrimSpace(strings.TrimPrefix(text, "gitdir:")))
	}
	common := filepath.Join(gitDir, "commondir")
	if ci, err := os.Stat(common); err == nil && ci.Mode().IsRegular() {
		data, err := os.ReadFile(common)
		if err != nil {
			return "", false
		}
		gitDir = joinPath(gitDir, strings.TrimSpace(string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))))
	}
	return filepath.Clean(gitDir), true
}

func (m *Manager) MaintainPacks(ctx context.Context, repoRoot string) error {
	if err := m.maintainPacks(ctx, repoRoot); err != nil {
		if fatal(ctx, err) {
			return err
		}
		slog.Debug("pack maintenance skipped", "error", err)
	}
	return nil
}

func (m *Manager) maintainPacks(ctx context.Context, repoRoot string) error {
	gitDir, ok := commonGitDir(repoRoot)
	if !ok {
		return nil
	}
	packs := filepath.Join(gitDir, "objects", "pack")
	if info, err := os.Stat(packs); err != nil || !info.IsDir() {
		return nil
	}
	found, err := filepath.Glob(filepath.Join(packs, "*.pack"))
	if err != nil {
		return err
	}
	if len(found) >= packSprawlThreshold {
		claimed, err := claimSlot(gitDir, repackLock, repackInterval)
		if err != nil {
			return err
		}
		if claimed {
			clearStaleTmpPacks(gitDir)
			slog.Info(fmt.Sprintf("git pack sprawl (%d packs) — repacking in background", len(found)))
			if _, _, err := m.runBounded(ctx, repoRoot, []string{"repack", "-d", "--geometric=2", "--write-midx", "--quiet"}, repackTimeout, nil); err != nil {
				return err
			}
			if _, err := m.git(ctx, repoRoot, 60*time.Second, m.noninteractiveEnv(ctx), "worktree", "prune"); err != nil {
				return err
			}
		}
	}
	stale := commitGraphStale(gitDir)
	if !stale {
		return nil
	}
	claimed, err := claimSlot(gitDir, graphLock, graphInterval)
	if err != nil || !claimed {
		return err
	}
	return m.refreshCommitGraph(ctx, repoRoot, gitDir)
}

func claimSlot(gitDir, name string, interval time.Duration) (bool, error) {
	lock, err := fileutil.TryOpenLock(filepath.Join(gitDir, name), fileutil.Range(0, 1), fileutil.LockPerm(0o644))
	if err != nil {
		if errors.Is(err, fileutil.ErrLocked) {
			return false, nil
		}
		return false, err
	}
	defer lock.Release()
	f := lock.File()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() > 0 && time.Since(info.ModTime()) < interval {
		return false, nil
	}
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0); err != nil {
		return false, err
	}
	return true, nil
}

func commitGraphStale(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	var newest time.Time
	present := false
	for _, p := range []string{filepath.Join(info, "commit-graph"), filepath.Join(info, "commit-graphs", "commit-graph-chain")} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			present = true
			if st.ModTime().After(newest) {
				newest = st.ModTime()
			}
		}
	}
	if !present {
		return true
	}
	if st, err := os.Stat(filepath.Join(gitDir, graphFreshStamp)); err == nil && st.Mode().IsRegular() && st.ModTime().After(newest) {
		newest = st.ModTime()
	}
	return time.Since(newest) > graphMaxAge
}

func (m *Manager) commitGraphDisabled(ctx context.Context, repoRoot string) (bool, error) {
	env := m.noninteractiveEnv(ctx)
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		delete(env, envKey(key))
		if v, ok := os.LookupEnv(key); ok {
			env[envKey(key)] = v
		}
	}
	r, err := m.git(ctx, repoRoot, 10*time.Second, env, "config", "--type=bool", "--get", "core.commitGraph")
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	return r.out() == "false", nil
}

func (m *Manager) refreshCommitGraph(ctx context.Context, repoRoot, gitDir string) error {
	disabled, err := m.commitGraphDisabled(ctx, repoRoot)
	if err != nil {
		return err
	}
	if disabled {
		slog.Info("core.commitGraph=false; not writing a commit-graph", "repo", repoRoot)
		return nil
	}
	clearStaleObjectStoreLocks(gitDir)
	clearStaleTmpPacks(gitDir)
	slog.Info("git commit-graph missing or stale — writing it in background")
	env := m.noninteractiveEnv(ctx)
	env[envKey("GIT_NO_LAZY_FETCH")] = "1"
	code, finished, err := m.runBounded(ctx, repoRoot, commitGraphWrite, graphTimeout, env)
	if err != nil {
		return err
	}
	if !finished || code != 0 {
		return nil
	}
	if !graphOnDisk(gitDir) {
		slog.Info("git commit-graph write exited 0 but left no graph (no commits yet?)", "repo", repoRoot)
		return nil
	}
	return touch(filepath.Join(gitDir, graphFreshStamp))
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

func graphOnDisk(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	data, err := os.ReadFile(filepath.Join(info, "commit-graphs", "commit-graph-chain"))
	if err != nil || !isASCII(data) {
		st, err := os.Stat(filepath.Join(info, "commit-graph"))
		return err == nil && st.Mode().IsRegular()
	}
	layers := strings.Fields(string(data))
	if len(layers) == 0 {
		return false
	}
	for _, layer := range layers {
		st, err := os.Stat(filepath.Join(info, "commit-graphs", "graph-"+layer+".graph"))
		if err != nil || !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func (m *Manager) runBounded(ctx context.Context, repoRoot string, args []string, timeout time.Duration, env map[string]string) (int, bool, error) {
	if m.guard == nil {
		return 0, false, &MissingInterfaceError{Name: "proc.Guard"}
	}
	if m.ledger == nil {
		return 0, false, &MissingInterfaceError{Name: "proc.Ledger"}
	}
	if env == nil {
		env = m.processEnv(ctx)
	}
	name, argv := boundedArgv(args)
	bin := lookPath(env[envKey("PATH")], name)
	if bin == "" {
		return 0, false, &RunError{Command: name, Err: exec.ErrNotFound}
	}
	stderr := &tailBuffer{max: 4096}
	cmd := exec.Command(bin, argv...)
	cmd.Dir = repoRoot
	cmd.Env = environList(env)
	cmd.Stderr = stderr
	proc.Hide(cmd)
	if err := m.guard.StartOwned(ctx, cmd, m.ledger, "git "+args[0]); err != nil {
		if !errors.Is(err, proc.ErrUnguarded) || cmd.Process == nil {
			return 0, false, err
		}
		slog.Debug("bounded git runs without owner-death coverage", "error", err)
	}
	pid := cmd.Process.Pid
	id, idErr := proc.Identify(pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cleanup := context.WithoutCancel(ctx)
	kill := func() {
		if idErr == nil {
			if err := proc.KillTree(cleanup, id, killGrace); err != nil {
				slog.Debug("bounded git tree kill failed", "error", err)
			}
		} else if err := cmd.Process.Kill(); err != nil {
			slog.Debug("bounded git kill failed", "error", err)
		}
		<-done
	}
	release := func() {
		rctx, cancel := context.WithTimeout(cleanup, killGrace)
		defer cancel()
		m.guard.ReleaseWhenEmpty(rctx, pid)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		kill()
		release()
		slog.Warn(fmt.Sprintf("git %s in %s exceeded %gs; killed", args[0], repoRoot, timeout.Seconds()))
		return 0, false, nil
	case <-ctx.Done():
		kill()
		release()
		return 0, false, ctx.Err()
	}
	release()
	code := proc.ExitOf(cmd.ProcessState).Code
	if code != 0 {
		lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(stderr.String(), string(rune(0xFFFD)))), "\n")
		cause := "no output"
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			cause = lines[len(lines)-1]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "fatal:") || strings.HasPrefix(lines[i], "error:") {
				cause = lines[i]
				break
			}
		}
		slog.Warn(fmt.Sprintf("git %s in %s failed (exit %d): %s", args[0], repoRoot, code, cause))
	}
	return code, true, nil
}

func gitProgram(arg0 string) bool {
	parts := pathSplit.Split(strings.TrimSpace(arg0), -1)
	name := strings.TrimSuffix(strings.ToLower(parts[len(parts)-1]), ".exe")
	return name == "git" || strings.HasPrefix(name, "git-")
}

func gitProcRunning() bool {
	table, err := proc.ReadTable()
	if err != nil {
		slog.Debug("git process scan failed; assuming git may be running", "error", err)
		return true
	}
	for _, entry := range table {
		name, err := proc.Name(entry.PID)
		if err != nil {
			if !proc.Exists(entry.PID) {
				continue
			}
			slog.Debug("git process scan failed; assuming git may be running", "error", err)
			return true
		}
		if gitProgram(name) {
			return true
		}
	}
	return false
}

func sweepStale(directory string, candidates func() []string, skipMsg string, logRemoved func(path string, size int64)) []string {
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return nil
	}
	if gitProcRunning() {
		slog.Debug(skipMsg)
		return nil
	}
	cutoff := time.Now().Add(-staleDebrisAge)
	var removed []string
	for _, entry := range candidates() {
		info, err := os.Stat(entry)
		if err != nil {
			if !isNotExist(err) {
				slog.Warn("Could not clear git debris (skipping)", "path", entry, "error", err)
			}
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		prepareUnlink(entry)
		if err := os.Remove(entry); err != nil {
			slog.Warn("Could not clear git debris (skipping)", "path", entry, "error", err)
			continue
		}
		removed = append(removed, entry)
		logRemoved(entry, info.Size())
	}
	return removed
}

func clearStaleObjectStoreLocks(gitDir string) []string {
	return sweepStale(gitDir, func() []string {
		out := make([]string, 0, len(objectStoreLockNames))
		for _, name := range objectStoreLockNames {
			out = append(out, filepath.Join(gitDir, filepath.FromSlash(name)))
		}
		return out
	}, "git process running; skipping stale-lock sweep", func(path string, _ int64) {
		slog.Info("Removed stale git lock", "path", path)
	})
}

func clearStaleTmpPacks(repoRoot string) []string {
	packs := packDir(repoRoot)
	graphs := filepath.Join(filepath.Dir(packs), "info", "commit-graphs")
	return sweepStale(packs, func() []string {
		var found []string
		for _, dir := range []struct {
			path     string
			prefixes []string
		}{{packs, tmpPackPrefixes}, {graphs, []string{"tmp_graph_"}}} {
			names, err := readDirNames(dir.path)
			if err != nil {
				continue
			}
			for _, name := range names {
				for _, prefix := range dir.prefixes {
					if strings.HasPrefix(name, prefix) {
						found = append(found, filepath.Join(dir.path, name))
						break
					}
				}
			}
		}
		return found
	}, "git process running; skipping tmp-pack sweep", func(path string, size int64) {
		slog.Info("Removed aborted-write git debris", "path", path, "bytes", size)
	})
}

func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
