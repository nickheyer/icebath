package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// A clone with this many packs is repacked
	packSprawl    = 15
	repackTimeout = 30 * time.Minute
	// Fewest time between two repacks, and two commit graph writes, of one clone by any process
	slotInterval = 6 * time.Hour
	repackSlot   = "icebath-repack.lock"
	graphSlot    = "icebath-commit-graph.lock"
	// A commit graph not written or found current for this long is written again
	graphAge     = 24 * time.Hour
	graphTimeout = 10 * time.Minute
	// Stamp of the last time the commit graph was found current
	graphFresh = "icebath-commit-graph.fresh"
)

// Arguments of a split commit graph write that never computes changed paths or prints progress
var graphWrite = []string{"commit-graph", "write", "--reachable", "--split", "--no-changed-paths", "--no-progress"}

// Claims a maintenance job of the clone for this process: exactly one process per clone gets it per interval, the slot stamped with the
// process id; false while another holds the slot or ran the job within the interval
func claim(gitDir, slot string, interval time.Duration) (bool, error) {
	f, ok, err := fsx.OpenLocked(filepath.Join(gitDir, slot), false)
	if err != nil || !ok {
		return false, err
	}
	defer f.Close()
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
	_, err = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return err == nil, err
}

// Gives back this process's claim of a slot so the next run retries, a claim another process made since left alone
func unclaim(gitDir, slot string) error {
	f, _, err := fsx.OpenLocked(filepath.Join(gitDir, slot), true)
	if err != nil {
		return err
	}
	defer f.Close()
	owner, err := io.ReadAll(io.LimitReader(f, 32))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(owner)) == strconv.Itoa(os.Getpid()) {
		return f.Truncate(0)
	}
	return nil
}

// Runs a git that yields to every other process, its whole process tree stopped with the timeout or with this process: its exit status,
// and whether it was stopped. A failure is logged with the last line of its errors
func bounded(ctx context.Context, repo string, args []string, timeout time.Duration, env map[string]string) (int, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, prefix := nicely()
	cmd := exec.CommandContext(ctx, name, append(prefix, args...)...)
	cmd.Dir = repo
	if env != nil {
		cmd.Env = Environ(env)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	group(cmd)
	lower(cmd)
	owned(cmd)
	cmd.WaitDelay = 5 * time.Second
	runtime.LockOSThread()
	err := cmd.Run()
	runtime.UnlockOSThread()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.Warn("git "+args[0]+" exceeded its time; killed", "repo", repo, "timeout", timeout)
		return 0, true, nil
	}
	code := 0
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exit.ExitCode()
	} else if err != nil {
		return 0, false, err
	}
	if code != 0 {
		tail := stderr.Bytes()
		tail = tail[max(0, len(tail)-4096):]
		lines := Lines(strings.ToValidUTF8(string(tail), "�"))
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
		slog.Warn("git "+args[0]+" failed", "repo", repo, "code", code, "cause", cause)
	}
	return code, false, nil
}

// Keeps a clone many processes share fast: a background repack once its packs sprawl, and a fresh commit graph, each claimed by one
// process per interval; every failure logged, none returned
func Maintain(ctx context.Context, repo string) {
	if err := maintain(ctx, repo); err != nil {
		slog.Debug("pack maintenance skipped", "repo", repo, "error", err)
	}
}

func maintain(ctx context.Context, repo string) error {
	gitDir := CommonDir(ctx, repo)
	if gitDir == "" {
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
	if len(found) >= packSprawl {
		claimed, err := claim(gitDir, repackSlot, slotInterval)
		if err != nil {
			return err
		}
		if claimed {
			SweepTemp(gitDir, StaleAge)
			slog.Info("git pack sprawl — repacking in background", "packs", len(found), "repo", repo)
			if _, _, err := bounded(ctx, repo, []string{"repack", "-d", "--geometric=2", "--write-midx", "--quiet"}, repackTimeout, nil); err != nil {
				return err
			}
			if _, err := Run(ctx, repo, Env(ctx, nil), time.Minute, nil, "worktree", "prune"); err != nil {
				return err
			}
		}
	}
	if !stale(gitDir) {
		return nil
	}
	claimed, err := claim(gitDir, graphSlot, slotInterval)
	if err != nil || !claimed {
		return err
	}
	if err := graph(ctx, repo, gitDir); err != nil {
		return errors.Join(err, unclaim(gitDir, graphSlot))
	}
	return nil
}

// Modification time of a regular file, false for anything else
func modified(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// Whether the clone has no commit graph, or none written or found current for a day
func stale(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	var newest time.Time
	found := false
	for _, path := range []string{filepath.Join(info, "commit-graph"), filepath.Join(info, "commit-graphs", "commit-graph-chain")} {
		if when, ok := modified(path); ok {
			found = true
			if when.After(newest) {
				newest = when
			}
		}
	}
	if !found {
		return true
	}
	if when, ok := modified(filepath.Join(gitDir, graphFresh)); ok && when.After(newest) {
		newest = when
	}
	return time.Since(newest) > graphAge
}

// Whether core.commitGraph is false for the repository, read with the user's own git config
func graphOff(ctx context.Context, repo string) bool {
	env := Env(ctx, nil)
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		delete(env, key)
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	env["GIT_NO_LAZY_FETCH"] = "1"
	result, err := Run(ctx, repo, env, 10*time.Second, nil, "config", "--type=bool", "--get", "core.commitGraph")
	return err == nil && strings.TrimSpace(result.Stdout) == "false"
}

// Clears what a killed writer left, then writes the commit graph, bounded, yielding and never fetching what a partial clone lacks
func graph(ctx context.Context, repo, gitDir string) error {
	if graphOff(ctx, repo) {
		slog.Info("core.commitGraph=false; not writing a commit-graph", "repo", repo)
		return nil
	}
	SweepObjectLocks(gitDir)
	SweepTemp(gitDir, StaleAge)
	slog.Info("git commit-graph missing or stale — writing it in background", "repo", repo)
	env := Env(ctx, nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	code, killed, err := bounded(ctx, repo, graphWrite, graphTimeout, env)
	if err != nil || killed || code != 0 {
		return err
	}
	if !written(gitDir) {
		slog.Info("git commit-graph write exited 0 but left no graph (no commits yet?)", "repo", repo)
		return nil
	}
	fresh := filepath.Join(gitDir, graphFresh)
	f, err := os.OpenFile(fresh, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(fresh, now, now)
}

// Whether a commit graph is on disk: a split chain whose every layer file exists, or a single graph
func written(gitDir string) bool {
	info := filepath.Join(gitDir, "objects", "info")
	chain, err := os.ReadFile(filepath.Join(info, "commit-graphs", "commit-graph-chain"))
	if err != nil || bytes.ContainsFunc(chain, func(r rune) bool { return r >= 0x80 }) {
		_, ok := modified(filepath.Join(info, "commit-graph"))
		return ok
	}
	layers := strings.Fields(string(chain))
	if len(layers) == 0 {
		return false
	}
	for _, layer := range layers {
		if _, ok := modified(filepath.Join(info, "commit-graphs", "graph-"+layer+".graph")); !ok {
			return false
		}
	}
	return true
}
