package cron

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const sweepWaitSeconds = 120.0

// Prunes stale worktrees and orphaned branches of one repo through the worktree_ops port, a seam the tests record
var worktreePruneStaleWorktrees = func(ctx context.Context, repo string) error {
	return tools.PruneStaleWorktrees(ctx, repo, 24)
}

// Installs the worktree registry recorder and the cron store readers the worktree port consults
func init() {
	tools.WorktreeRecordRepo = func(repoRoot string) { RecordWorktreeRepo(home.Process(), repoRoot) }
	tools.WorktreeCronStore.JobsFile = func() string { return openCronStore(home.Process(), nil).JobsFile() }
	tools.WorktreeCronStore.ParseJobs = func(path string) (any, error) {
		data, _, err := parseJobsFile(path)
		return data, err
	}
	tools.WorktreeCronStore.JobActive = rawJobActive
}

// Stored job dict the scheduler may fire that is not in a terminal state, read with Python truthiness
func rawJobActive(job *pyjson.Object) bool {
	if enabled, ok := job.Get("enabled"); ok && !pyrepr.Truthy(enabled) {
		return false
	}
	state, _ := job.Get("state")
	stateText := ""
	if state != nil {
		stateText = pyrepr.Str(state)
	}
	if pausedAt, _ := job.Get("paused_at"); pystr.Strip(stateText) == "paused" || pyrepr.Truthy(pausedAt) {
		return false
	}
	text, isText := state.(string)
	return !isText || (text != "completed" && text != "error")
}

var (
	sweepSlot         = make(chan struct{}, 1)
	lastWorktreePrune = struct {
		sync.Mutex
		at map[string]float64
	}{at: map[string]float64{}}
	lastWorktreeMaintenance = struct {
		sync.Mutex
		at map[string]float64
	}{at: map[string]float64{}}
)

// Takes the process wide prune slot, waiting at most the given seconds for the sweep ahead
func claimSweepSlot(waitSeconds *float64) bool {
	wait := sweepWaitSeconds
	if waitSeconds != nil {
		wait = *waitSeconds
	}
	select {
	case sweepSlot <- struct{}{}:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(time.Duration(wait * float64(time.Second)))
	defer timer.Stop()
	select {
	case sweepSlot <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// Frees the process wide prune slot
func releaseSweepSlot() {
	<-sweepSlot
}

// Repos no served profile pruned within the interval, stamped as claimed under their resolved path
func claimDueWorktreeRepos(repos []string, intervalSeconds float64) []string {
	now := tickMonotonic()
	var due []string
	lastWorktreePrune.Lock()
	defer lastWorktreePrune.Unlock()
	for _, repo := range repos {
		resolved, err := pypath.Realpath(repo, pypath.Lax)
		if err != nil {
			resolved = repo
		}
		key := pypath.Normcase(resolved)
		last, seen := lastWorktreePrune.at[key]
		if !seen || now-last >= intervalSeconds {
			lastWorktreePrune.at[key] = now
			due = append(due, repo)
		}
	}
	return due
}

// Prunes the due repos discover reports one sweep at a time, never propagating a failure
func (s *Store) runWorktreeSweep(ctx context.Context, discover func(context.Context) []string, intervalSeconds float64, waitSeconds *float64) {
	if !claimSweepSlot(waitSeconds) {
		s.log.Debug("Cron worktree maintenance skipped: another sweep is still running")
		return
	}
	defer releaseSweepSlot()
	defer func() {
		if r := recover(); r != nil {
			s.log.Debug(fmt.Sprintf("Cron worktree maintenance skipped: %v", r))
		}
	}()
	for _, repo := range claimDueWorktreeRepos(discover(ctx), intervalSeconds) {
		if err := worktreePruneStaleWorktrees(ctx, repo); err != nil {
			s.log.Debug(fmt.Sprintf("Cron worktree maintenance failed for %s: %s", repo, err))
		}
	}
}

// Registered repos, the install checkout and job workdir repo roots that hold a .worktrees folder
func (s *Store) worktreeMaintenanceRepos(ctx context.Context) []string {
	repos := map[string]bool{}
	for _, r := range worktreeRegisteredRepos(s.home) {
		repos[r] = true
	}
	if root := pm.InstallRoot(); root != "" {
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			repos[root] = true
		}
	}
	if jobs, err := s.LoadJobs(); err == nil {
		for _, job := range jobs {
			workdir := ""
			if job.Workdir != nil {
				workdir = strings.TrimSpace(*job.Workdir)
			}
			if workdir == "" || !isDir(workdir) {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cmd := exec.CommandContext(pctx, "git", "rev-parse", "--show-toplevel")
			cmd.Dir = workdir
			out, err := cmd.Output()
			cancel()
			if top := strings.TrimSpace(string(out)); err == nil && top != "" {
				repos[top] = true
			}
		}
	}
	var keep []string
	for r := range repos {
		if isDir(filepath.Join(r, ".worktrees")) {
			keep = append(keep, r)
		}
	}
	sort.Strings(keep)
	return keep
}

// Starts a throttled background worktree prune for the profile, never blocking the tick
func (s *Store) maybeRunWorktreeMaintenance(ctx context.Context) {
	h := s.Home()
	now := tickMonotonic()
	lastWorktreeMaintenance.Lock()
	last, seen := lastWorktreeMaintenance.at[h]
	if seen && now-last < worktreeMaintenanceIntervalSec {
		lastWorktreeMaintenance.Unlock()
		return
	}
	lastWorktreeMaintenance.at[h] = now
	lastWorktreeMaintenance.Unlock()
	go s.runWorktreeSweep(context.WithoutCancel(ctx), s.worktreeMaintenanceRepos, worktreeMaintenanceIntervalSec, nil)
}

// Whether the path names an existing directory, following symlinks
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
