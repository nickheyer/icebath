package worktree

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type candidate struct {
	path  string
	name  string
	mtime float64
	force bool
}

type classified struct {
	candidate
	verdict string
	lock    string
}

func sortedEntries(dir string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	slices.Sort(names)
	return names, nil
}

func pruneCandidates(worktreesDir string, maxAgeHours, now float64) ([]candidate, error) {
	names, err := sortedEntries(worktreesDir)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, name := range names {
		path := filepath.Join(worktreesDir, name)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || kanbanTree.MatchString(name) {
			continue
		}
		tier := PruneTierHours(name, maxAgeHours)
		mtime := epoch(info.ModTime())
		if mtime > now-tier*3600 {
			continue
		}
		out = append(out, candidate{path: path, name: name, mtime: mtime, force: mtime <= now-tier*3*3600})
	}
	return out, nil
}

func (m *Manager) classifyOne(ctx context.Context, repoRoot string, c candidate, cache *verdictCache, heads func() (map[string]string, bool, error)) (*classified, error) {
	dirty, err := m.isDirty(ctx, c.path, repoRoot, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if dirty {
		return &classified{candidate: c, verdict: "dirty"}, nil
	}
	keepBranch := false
	unpushed, err := m.hasUnpushedCommits(ctx, c.path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if unpushed {
		merged, err := m.commitsAllMergedUpstream(ctx, c.path, 30*time.Second, 20, cache)
		if err != nil {
			return nil, err
		}
		if !merged {
			if merged, err = m.branchPRMerged(ctx, c.path, 15*time.Second, cache); err != nil {
				return nil, err
			}
		}
		if !merged {
			remote, _, err := heads()
			if err != nil {
				return nil, err
			}
			exact, err := m.branchPushedExact(ctx, c.path, remote, 10*time.Second)
			if err != nil {
				return nil, err
			}
			if !exact {
				return &classified{candidate: c, verdict: "unpushed"}, nil
			}
		}
		keepBranch = !merged
	}
	lock, err := m.lockState(ctx, repoRoot, c.path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if lock == lockLive {
		return &classified{candidate: c, verdict: "locked-live"}, nil
	}
	verdict := "reap"
	if keepBranch {
		verdict = "reap-keep-branch"
	}
	return &classified{candidate: c, verdict: verdict, lock: lock}, nil
}

func (m *Manager) classifyCandidates(ctx context.Context, repoRoot string, candidates []candidate) ([]*classified, error) {
	cache, err := m.loadMergeCache(ctx)
	if err != nil {
		return nil, err
	}
	sizeBefore := cache.size()
	var headsMu sync.Mutex
	memo := &headsMemo{}
	heads := func() (map[string]string, bool, error) {
		headsMu.Lock()
		defer headsMu.Unlock()
		if !memo.done {
			h, known, err := m.fetchRemoteHeads(ctx, repoRoot, 10*time.Second)
			if err != nil {
				return nil, false, err
			}
			memo.done, memo.heads, memo.known = true, h, known
		}
		return memo.heads, memo.known, nil
	}
	workers := max(1, min(8, runtime.NumCPU(), len(candidates)))
	results := make([]*classified, len(candidates))
	errs := make([]error, len(candidates))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], errs[i] = m.classifyOne(ctx, repoRoot, c, cache, heads)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if cache.size() != sizeBefore {
		if err := m.saveMergeCache(ctx, cache); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (m *Manager) reapVerdicts(ctx context.Context, repoRoot string, verdicts []*classified, staleCutoff float64) ([]string, map[string]bool, error) {
	var preserved []string
	kept := map[string]bool{}
	for _, v := range verdicts {
		if reason, ok := archiveReasons[v.verdict]; ok {
			if v.mtime <= staleCutoff {
				preserved = append(preserved, fmt.Sprintf("%s (%s)", v.name, reason))
			}
			continue
		}
		if v.verdict == "locked-live" {
			slog.Debug("Skipping live-locked worktree", "name", v.name)
			continue
		}
		blocked, err := m.RemovalBlocked(ctx, v.path)
		if err != nil {
			return nil, nil, err
		}
		if blocked != nil {
			preserved = append(preserved, fmt.Sprintf("%s (%s)", v.name, blocked.Reason))
			continue
		}
		if v.lock == lockDead {
			if err := m.gitQuiet(ctx, repoRoot, 10*time.Second, nil, "Failed to unlock dead worktree "+v.name, "worktree", "unlock", v.path); err != nil {
				return nil, nil, err
			}
		}
		if err := m.reapOne(ctx, repoRoot, v, kept); err != nil {
			if fatal(ctx, err) {
				return nil, nil, err
			}
			slog.Debug("Failed to prune worktree", "name", v.name, "error", err)
		}
	}
	return preserved, kept, nil
}

func (m *Manager) reapOne(ctx context.Context, repoRoot string, v *classified, kept map[string]bool) error {
	br, err := m.git(ctx, v.path, 5*time.Second, nil, "branch", "--show-current")
	if err != nil {
		return err
	}
	branch := br.out()
	removed, err := m.git(ctx, repoRoot, 15*time.Second, nil, "worktree", "remove", v.path, "--force")
	if err != nil {
		return err
	}
	if !removed.ok() {
		slog.Debug("Failed to remove worktree", "name", v.name, "stderr", removed.errText())
		return nil
	}
	switch {
	case branch != "" && v.verdict == "reap-keep-branch":
		kept[branch] = true
	case branch != "":
		if _, err := m.git(ctx, repoRoot, 10*time.Second, m.noninteractiveEnv(ctx), "branch", "-D", branch); err != nil {
			return err
		}
	}
	slog.Debug("Pruned stale worktree", "name", v.name, "force", v.force)
	return nil
}

func (m *Manager) PruneStale(ctx context.Context, repoRoot string, maxAgeHours float64) error {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		return m.pruneOrphanedBranches(ctx, repoRoot, nil)
	}
	shallow, err := m.repoIsShallow(ctx, repoRoot, 5*time.Second)
	if err != nil {
		return err
	}
	if shallow {
		if _, err := m.deepenShallowRepo(ctx, repoRoot, 600*time.Second); err != nil {
			return err
		}
	}
	settings, err := m.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.Classic() {
		if err := m.runSweep(ctx, repoRoot, maxAgeHours, settings); err != nil {
			return err
		}
		return m.warnFootprint(ctx, repoRoot)
	}
	now := epoch(time.Now())
	candidates, err := pruneCandidates(worktreesDir, maxAgeHours, now)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return m.pruneOrphanedBranches(ctx, repoRoot, nil)
	}
	verdicts, err := m.classifyCandidates(ctx, repoRoot, candidates)
	if err != nil {
		return err
	}
	preserved, kept, err := m.reapVerdicts(ctx, repoRoot, verdicts, now-7*24*3600)
	if err != nil {
		return err
	}
	if len(preserved) > 0 {
		sorted := slices.Clone(preserved)
		slices.Sort(sorted)
		slog.Warn(fmt.Sprintf("Preserving %d worktree(s) older than 7 days with unmerged work (run `icebath worktree prune` to review and reclaim): %s", len(preserved), strings.Join(sorted, ", ")))
	}
	if err := m.pruneOrphanedBranches(ctx, repoRoot, kept); err != nil {
		return err
	}
	return m.warnFootprint(ctx, repoRoot)
}

func (m *Manager) warnFootprint(ctx context.Context, repoRoot string) error {
	count, size, err := m.worktreesSummary(ctx, repoRoot, 10)
	if err != nil {
		if fatal(ctx, err) {
			return err
		}
		slog.Debug("worktree summary failed", "error", err)
		return nil
	}
	var sizeMB int64
	if size != nil {
		sizeMB = *size
	}
	if count >= 10 || sizeMB >= 5120 {
		sizeText := ""
		if sizeMB != 0 {
			sizeText = fmt.Sprintf(" (%.1fGB)", float64(sizeMB)/1024)
		}
		slog.Warn(fmt.Sprintf(".worktrees/ holds %d tree(s)%s — run `icebath worktree list` to audit and `icebath worktree prune` to reclaim safely.", count, sizeText))
	}
	return nil
}

func (m *Manager) pruneOrphanedBranches(ctx context.Context, repoRoot string, protect map[string]bool) error {
	listing, ok, err := m.gitOut(ctx, repoRoot, 10*time.Second, nil, "branch", "--format=%(refname:short)")
	if err != nil {
		if fatal(ctx, err) {
			return err
		}
		return nil
	}
	if !ok {
		return nil
	}
	var all []string
	for _, b := range strings.Split(listing, "\n") {
		if b = strings.TrimSpace(b); b != "" {
			all = append(all, b)
		}
	}
	wt, err := m.git(ctx, repoRoot, 10*time.Second, nil, "worktree", "list", "--porcelain")
	if err != nil {
		if fatal(ctx, err) {
			return err
		}
		return nil
	}
	active := map[string]bool{}
	for _, line := range strings.Split(wt.text(), "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			active[strings.TrimSpace(strings.TrimPrefix(line, "branch refs/heads/"))] = true
		}
	}
	current, err := m.git(ctx, repoRoot, 5*time.Second, nil, "branch", "--show-current")
	if err != nil {
		if fatal(ctx, err) {
			return err
		}
	} else if c := current.out(); c != "" {
		active[c] = true
	}
	active["main"] = true
	var orphaned []string
	for _, b := range all {
		if active[b] || protect[b] {
			continue
		}
		if strings.HasPrefix(b, "icebath/icebath-") || strings.HasPrefix(b, "pr-") {
			orphaned = append(orphaned, b)
		}
	}
	if len(orphaned) == 0 {
		return nil
	}
	env := m.noninteractiveEnv(ctx)
	for i := 0; i < len(orphaned); i += 50 {
		batch := orphaned[i:min(i+50, len(orphaned))]
		args := append([]string{"branch", "-D"}, batch...)
		if err := m.gitQuiet(ctx, repoRoot, 30*time.Second, env, "Failed to prune orphaned branches", args...); err != nil {
			return err
		}
	}
	slog.Debug("Pruned orphaned branches", "count", len(orphaned))
	return nil
}

func (m *Manager) treeSizeMB(ctx context.Context, path string, timeout time.Duration) (*int64, error) {
	r, err := m.run(ctx, "du", []string{"-sm", path}, call{timeout: timeout})
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		return nil, nil
	}
	fields := strings.Fields(r.text())
	if !r.ok() || len(fields) == 0 {
		return nil, nil
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return nil, nil
	}
	return &n, nil
}

func (m *Manager) worktreesSummary(ctx context.Context, repoRoot string, sizeBelowCount int) (int, *int64, error) {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		return 0, nil, nil
	}
	names, err := sortedEntries(worktreesDir)
	if err != nil {
		return 0, nil, nil
	}
	count := 0
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(worktreesDir, name)); err == nil && info.IsDir() {
			count++
		}
	}
	if sizeBelowCount > 0 && count >= sizeBelowCount {
		return count, nil, nil
	}
	size, err := m.treeSizeMB(ctx, worktreesDir, 20*time.Second)
	if err != nil {
		return 0, nil, err
	}
	return count, size, nil
}
