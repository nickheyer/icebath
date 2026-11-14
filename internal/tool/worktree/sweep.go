package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	protectedBranches = map[string]bool{"main": true, "master": true, "develop": true, "dev": true, "trunk": true}
	archiveReasons    = map[string]string{"dirty": "uncommitted changes", "unpushed": "unpushed commits"}
	kanbanTree        = regexp.MustCompile(`^t_[0-9a-f]+$`)
	destructive       = map[string]bool{"remove": true, "remove-keep-branch": true, "archive-remove": true}
)

type Settings struct {
	ArchiveDirty         bool
	ArchiveMaxFileMB     float64
	ArchiveRetentionDays float64
	MaxTotalGB           float64
}

func (s Settings) Classic() bool { return !s.ArchiveDirty && s.MaxTotalGB == 0 }

func (m *Manager) Settings(ctx context.Context) (Settings, error) {
	cfg, err := m.config.LoadReadonly(ctx)
	if err != nil {
		return Settings{}, err
	}
	w := cfg.Worktrees
	return Settings{
		ArchiveDirty:         w.ArchiveDirty,
		ArchiveMaxFileMB:     math.Max(0, w.ArchiveMaxFileMB),
		ArchiveRetentionDays: math.Max(0, w.ArchiveRetentionDays),
		MaxTotalGB:           math.Max(0, w.MaxTotalGB),
	}, nil
}

type nullString string

func (s nullString) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(s))
}

type Decision struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Action    string     `json:"action"`
	Reason    string     `json:"reason"`
	AgeHours  float64    `json:"age_hours"`
	Branch    nullString `json:"branch"`
	Mtime     float64    `json:"mtime"`
	Force     bool       `json:"force"`
	Verdict   string     `json:"verdict"`
	LockState nullString `json:"lock_state"`
	SizeMB    *int64     `json:"size_mb"`
	TierHours float64    `json:"tier_hours"`
	PlannedAt float64    `json:"planned_at"`
}

type ArchiveOutcome struct {
	Ref     string
	Removed bool
	Why     string
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func insideRoot(target string, roots []string) string {
	for _, root := range roots {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

func (m *Manager) LivePIDs(paths []string, exclude []int) (map[string][]int, error) {
	real := map[string]string{}
	var order []string
	for _, p := range paths {
		r := resolvePath(p)
		if _, seen := real[r]; !seen {
			order = append(order, r)
		}
		real[r] = p
	}
	if len(real) == 0 {
		return map[string][]int{}, nil
	}
	roots := slices.Clone(order)
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	skip := map[int]bool{}
	for _, pid := range exclude {
		skip[pid] = true
	}
	hits, err := scanLivePIDs(roots, skip)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for root, pids := range hits {
		out[real[root]] = pids
	}
	return out, nil
}

func pidList(pids []int) string {
	if len(pids) > 5 {
		pids = pids[:5]
	}
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, ", ")
}

func (m *Manager) lastActivity(ctx context.Context, path string) (float64, error) {
	now := epoch(time.Now())
	env, trusted, err := m.repoEnv(ctx, path)
	if err != nil {
		return 0, err
	}
	if !trusted {
		return now, nil
	}
	env[envKey("GIT_OPTIONAL_LOCKS")] = "0"
	gd, err := m.git(ctx, path, 10*time.Second, env, "rev-parse", "--absolute-git-dir")
	if err != nil {
		if fatal(ctx, err) {
			return 0, err
		}
		return epoch(time.Now()), nil
	}
	status, err := m.git(ctx, path, 60*time.Second, env, "status", "--porcelain", "-z", "-uall", "--no-renames")
	if err != nil {
		if fatal(ctx, err) {
			return 0, err
		}
		return epoch(time.Now()), nil
	}
	if !status.ok() {
		return epoch(time.Now()), nil
	}
	var candidates []string
	if gitDir := strings.TrimSpace(string(gd.stdout)); gitDir != "" {
		candidates = append(candidates, filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"))
	}
	wt := filepath.Clean(path)
	var changed []string
	for _, entry := range strings.Split(string(status.stdout), "\x00") {
		if len(entry) > 3 {
			changed = append(changed, filepath.Join(wt, filepath.FromSlash(entry[3:])))
		}
	}
	candidates = append(candidates, changed...)
	for _, item := range changed {
		parent := filepath.Dir(item)
		for parent != wt && strings.HasPrefix(parent, wt+string(filepath.Separator)) {
			if _, err := os.Stat(parent); err == nil {
				break
			}
			parent = filepath.Dir(parent)
		}
		candidates = append(candidates, parent)
	}
	newest := 0.0
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		info, err := os.Lstat(c)
		if err != nil {
			continue
		}
		newest = math.Max(newest, epoch(info.ModTime()))
	}
	return newest, nil
}

func PruneTierHours(name string, maxAgeHours float64) float64 {
	if strings.HasPrefix(name, "icebath-") || strings.HasPrefix(name, "subagent-") {
		return maxAgeHours
	}
	return maxAgeHours * 3
}

func (m *Manager) planSurvivor(ctx context.Context, repoRoot string, c *candidate, verdict string, settings Settings, tier, now float64) (string, string, string, float64, error) {
	what := archiveReasons[verdict]
	lock, err := m.lockState(ctx, repoRoot, c.path, 5*time.Second)
	if err != nil {
		return "", "", "", 0, err
	}
	if lock == lockLive {
		return "skip-live-lock", "lock owner still running", lock, 0, nil
	}
	active, err := m.lastActivity(ctx, c.path)
	if err != nil {
		return "", "", "", 0, err
	}
	if active > now-tier*3600 {
		return "skip-young", fmt.Sprintf("%s; last activity %.1fh ago", what, (now-active)/3600), lock, active, nil
	}
	if !settings.ArchiveDirty {
		return "keep-" + verdict, what, lock, active, nil
	}
	unsafe, err := m.archivePreflight(ctx, c.path, settings.ArchiveMaxFileMB)
	if err != nil {
		return "", "", "", 0, err
	}
	if unsafe != nil {
		return unsafe.Action, unsafe.Reason, lock, active, nil
	}
	return "archive-remove", what, lock, active, nil
}

func (m *Manager) applyBudget(ctx context.Context, decisions []*Decision, maxTotalGB, maxFileMB float64) error {
	var eligible []*Decision
	for _, d := range decisions {
		if d.Action == "keep-dirty" || d.Action == "keep-unpushed" {
			eligible = append(eligible, d)
		}
	}
	var total int64
	for _, d := range eligible {
		size, err := m.treeSizeMB(ctx, d.Path, 120*time.Second)
		if err != nil {
			return err
		}
		var v int64
		if size != nil {
			v = *size
		}
		d.SizeMB = &v
		total += v
	}
	budget := maxTotalGB * 1024
	ordered := slices.Clone(eligible)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Mtime < ordered[j].Mtime })
	for _, d := range ordered {
		if float64(total) <= budget {
			break
		}
		unsafe, err := m.archivePreflight(ctx, d.Path, maxFileMB)
		if err != nil {
			return err
		}
		if unsafe != nil {
			d.Action, d.Reason = unsafe.Action, unsafe.Reason
			continue
		}
		d.Action = "archive-remove"
		d.Reason = fmt.Sprintf("%s; over worktrees.max_total_gb=%g", d.Reason, maxTotalGB)
		total -= *d.SizeMB
	}
	return nil
}

func (m *Manager) PlanSweep(ctx context.Context, repoRoot string, maxAgeHours float64, settings Settings, olderThanDays *float64) ([]*Decision, error) {
	now := epoch(time.Now())
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if info, err := os.Stat(worktreesDir); err != nil || !info.IsDir() {
		return nil, nil
	}
	candidates, err := pruneCandidates(worktreesDir, maxAgeHours, now)
	if err != nil {
		return nil, err
	}
	doomed := map[string]bool{}
	for _, c := range candidates {
		doomed[c.name] = true
	}
	names, err := sortedEntries(worktreesDir)
	if err != nil {
		return nil, err
	}
	var decisions []*Decision
	for _, name := range names {
		path := filepath.Join(worktreesDir, name)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || doomed[name] {
			continue
		}
		age := (now - epoch(info.ModTime())) / 3600
		d := &Decision{Name: name, Path: path, Action: "skip-young", Reason: "younger than its age tier", AgeHours: round1(age)}
		if kanbanTree.MatchString(name) {
			d.Action, d.Reason = "skip-kanban", "owned by kanban gc"
		}
		decisions = append(decisions, d)
	}
	var verdicts []*classified
	if len(candidates) > 0 {
		if verdicts, err = m.classifyCandidates(ctx, repoRoot, candidates); err != nil {
			return nil, err
		}
	}
	live := map[string][]int{}
	if !settings.Classic() {
		paths := make([]string, 0, len(verdicts))
		for _, v := range verdicts {
			paths = append(paths, v.path)
		}
		if live, err = m.LivePIDs(paths, nil); err != nil {
			return nil, err
		}
	}
	for _, v := range verdicts {
		tier := PruneTierHours(v.name, maxAgeHours)
		d := &Decision{Name: v.name, Path: v.path, AgeHours: round1((now - v.mtime) / 3600), Mtime: v.mtime, Force: v.force,
			Verdict: v.verdict, LockState: nullString(v.lock), TierHours: tier, PlannedAt: now}
		_, archivable := archiveReasons[v.verdict]
		switch {
		case len(live[v.path]) > 0:
			d.Action, d.Reason = "skip-live-process", "in use by pid "+pidList(live[v.path])
		case v.verdict == "locked-live":
			d.Action, d.Reason = "skip-live-lock", "lock owner still running"
		case archivable && settings.Classic():
			d.Action, d.Reason = "keep-"+v.verdict, archiveReasons[v.verdict]
		case archivable:
			action, reason, lock, active, err := m.planSurvivor(ctx, repoRoot, &v.candidate, v.verdict, settings, tier, now)
			if err != nil {
				return nil, err
			}
			d.Action, d.Reason, d.LockState = action, reason, nullString(lock)
			if active != 0 {
				d.AgeHours = math.Min(d.AgeHours, round1((now-active)/3600))
			}
		default:
			blocked, err := m.RemovalBlocked(ctx, v.path)
			if err != nil {
				return nil, err
			}
			switch {
			case blocked != nil:
				d.Action, d.Reason = blocked.Action, blocked.Reason
			case v.verdict == "reap-keep-branch":
				d.Action, d.Reason = "remove-keep-branch", "pushed to origin; branch kept"
			default:
				d.Action, d.Reason = "remove", "clean and merged/pushed"
			}
		}
		decisions = append(decisions, d)
	}
	if settings.MaxTotalGB != 0 {
		if err := m.applyBudget(ctx, decisions, settings.MaxTotalGB, settings.ArchiveMaxFileMB); err != nil {
			return nil, err
		}
	}
	if olderThanDays != nil {
		for _, d := range decisions {
			if destructive[d.Action] && d.AgeHours < *olderThanDays*24 {
				d.Action, d.Reason = "skip-young", fmt.Sprintf("idle %.1fh (--older-than %g)", d.AgeHours, *olderThanDays)
			}
		}
	}
	return decisions, nil
}

func (m *Manager) branchPublished(ctx context.Context, repoRoot, branch string, heads map[string]string, headsKnown bool) (bool, error) {
	remotes, ok, err := m.gitOut(ctx, repoRoot, 10*time.Second, nil, "remote")
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return true, nil
	}
	if ok && remotes == "" {
		return false, nil
	}
	if !headsKnown {
		return true, nil
	}
	if _, found := heads[branch]; found {
		return true, nil
	}
	refs, ok, err := m.gitOut(ctx, repoRoot, 10*time.Second, nil, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return true, nil
	}
	return !ok || strings.TrimSpace(refs) != "", nil
}

func (m *Manager) inUseSince(ctx context.Context, path string, since float64, exclude []int) (string, error) {
	live, err := m.LivePIDs([]string{path}, exclude)
	if err != nil {
		return "", err
	}
	if pids := live[path]; len(pids) > 0 {
		return "in use by pid " + pidList(pids), nil
	}
	active, err := m.lastActivity(ctx, path)
	if err != nil {
		return "", err
	}
	if active >= since {
		return fmt.Sprintf("changed %.0fs ago", math.Max(0, epoch(time.Now())-active)), nil
	}
	return "", nil
}

func (m *Manager) indexMoved(ctx context.Context, path, captured string) (string, error) {
	staged, err := m.stagedState(ctx, path)
	if err == nil && staged == captured {
		return "", nil
	}
	if fatal(ctx, err) {
		return "", err
	}
	return "staged changes since the snapshot", nil
}

type ArchiveRemoveOptions struct {
	Reason         string
	MaxUntrackedMB float64
	Unlock         bool
	KeepBranch     bool
	ExcludePIDs    []int
	remoteHeads    func() (map[string]string, bool, error)
}

type ArchiveRemoveResult struct {
	Ref        string
	KeptBranch string
	Removed    bool
	WhyKept    string
}

func (m *Manager) ArchiveThenRemove(ctx context.Context, repoRoot, path string, o ArchiveRemoveOptions) (*ArchiveRemoveResult, error) {
	started := epoch(time.Now())
	arc, err := m.snapshot(ctx, repoRoot, path, o.Reason, o.MaxUntrackedMB)
	if err != nil {
		return nil, err
	}
	blocked, err := m.removalBlocker(ctx, path, arc.Skipped(), true)
	if err != nil {
		return nil, err
	}
	busy, err := m.inUseSince(ctx, path, started, o.ExcludePIDs)
	if err != nil {
		return nil, err
	}
	if busy == "" {
		if busy, err = m.indexMoved(ctx, path, arc.Staged); err != nil {
			return nil, err
		}
	}
	if busy == "" && blocked != nil {
		busy = blocked.Reason
	}
	if busy != "" {
		slog.Warn("Archived worktree but keeping it", "path", path, "ref", arc.Ref, "reason", busy)
		return &ArchiveRemoveResult{Ref: arc.Ref, WhyKept: busy}, nil
	}
	if err := m.releaseLSP(ctx, path); err != nil {
		return nil, err
	}
	if o.Unlock {
		if err := m.gitQuiet(ctx, repoRoot, 10*time.Second, nil, "unlock "+path, "worktree", "unlock", path); err != nil {
			return nil, err
		}
	}
	removed, err := m.git(ctx, repoRoot, 60*time.Second, nil, "worktree", "remove", "--force", path)
	if err != nil {
		return nil, err
	}
	if !removed.ok() {
		why := removed.errText()
		slog.Warn("Archived worktree but could not remove it", "path", path, "ref", arc.Ref, "stderr", why)
		if why == "" {
			why = "git worktree remove failed"
		}
		return &ArchiveRemoveResult{Ref: arc.Ref, WhyKept: why}, nil
	}
	branch := arc.Branch()
	if branch == "" || o.KeepBranch || protectedBranches[branch] {
		return &ArchiveRemoveResult{Ref: arc.Ref, KeptBranch: branch, Removed: true}, nil
	}
	var heads map[string]string
	var known bool
	if o.remoteHeads == nil {
		heads, known, err = m.fetchRemoteHeads(ctx, repoRoot, 10*time.Second)
	} else {
		heads, known, err = o.remoteHeads()
	}
	if err != nil {
		return nil, err
	}
	published, err := m.branchPublished(ctx, repoRoot, branch, heads, known)
	if err != nil {
		return nil, err
	}
	if published {
		return &ArchiveRemoveResult{Ref: arc.Ref, KeptBranch: branch, Removed: true}, nil
	}
	if _, err := m.git(ctx, repoRoot, 10*time.Second, m.noninteractiveEnv(ctx), "branch", "-D", branch); err != nil {
		return nil, err
	}
	return &ArchiveRemoveResult{Ref: arc.Ref, Removed: true}, nil
}

func (m *Manager) recheck(ctx context.Context, repoRoot string, d *Decision) (string, string, error) {
	if info, err := os.Stat(d.Path); err != nil || !info.IsDir() {
		return "gone", "", nil
	}
	lock, err := m.lockState(ctx, repoRoot, d.Path, 5*time.Second)
	if err != nil {
		return "", "", err
	}
	if lock == lockLive {
		return "lock owner still running", lock, nil
	}
	since := d.PlannedAt
	if since == 0 {
		since = epoch(time.Now())
	}
	if d.Action == "archive-remove" && d.TierHours != 0 {
		since = math.Min(since, epoch(time.Now())-d.TierHours*3600)
	}
	busy, err := m.inUseSince(ctx, d.Path, since, nil)
	if err != nil {
		return "", "", err
	}
	if busy != "" {
		return busy, lock, nil
	}
	if d.Action != "archive-remove" {
		dirty, err := m.isDirty(ctx, d.Path, repoRoot, 10*time.Second)
		if err != nil {
			return "", "", err
		}
		if dirty {
			return "new uncommitted changes", lock, nil
		}
	}
	return "", lock, nil
}

type headsMemo struct {
	done  bool
	heads map[string]string
	known bool
}

func (m *Manager) ExecutePlan(ctx context.Context, repoRoot string, plan []*Decision, settings Settings) ([]string, map[string]bool, []ArchiveOutcome, error) {
	memo := &headsMemo{}
	remoteHeads := func() (map[string]string, bool, error) {
		if !memo.done {
			heads, known, err := m.fetchRemoteHeads(ctx, repoRoot, 10*time.Second)
			if err != nil {
				return nil, false, err
			}
			memo.done, memo.heads, memo.known = true, heads, known
		}
		return memo.heads, memo.known, nil
	}
	kept := map[string]bool{}
	var archived []ArchiveOutcome
	var preserved []string
	for _, d := range plan {
		if strings.HasPrefix(d.Action, "keep-") {
			preserved = append(preserved, fmt.Sprintf("%s (%s)", d.Name, d.Reason))
		}
	}
	for _, d := range plan {
		if !destructive[d.Action] {
			continue
		}
		whyKeep, lock, err := m.recheck(ctx, repoRoot, d)
		if err != nil {
			return nil, nil, nil, err
		}
		if whyKeep != "" {
			slog.Info("Sweep: keeping worktree", "name", d.Name, "reason", whyKeep)
			continue
		}
		if d.Action != "archive-remove" {
			item := &classified{candidate: candidate{path: d.Path, name: d.Name, mtime: d.Mtime, force: d.Force}, verdict: d.Verdict, lock: lock}
			_, keptBranches, err := m.reapVerdicts(ctx, repoRoot, []*classified{item}, 0)
			if err != nil {
				return nil, nil, nil, err
			}
			for b := range keptBranches {
				kept[b] = true
			}
			continue
		}
		res, err := m.ArchiveThenRemove(ctx, repoRoot, d.Path, ArchiveRemoveOptions{
			Reason: "sweep: " + d.Reason, MaxUntrackedMB: settings.ArchiveMaxFileMB, Unlock: lock == lockDead, remoteHeads: remoteHeads,
		})
		if err != nil {
			if fatal(ctx, err) {
				return nil, nil, nil, err
			}
			slog.Warn("Keeping worktree: archive failed", "name", d.Name, "error", err)
			preserved = append(preserved, d.Name+" (archive failed)")
			continue
		}
		archived = append(archived, ArchiveOutcome{Ref: res.Ref, Removed: res.Removed, Why: res.WhyKept})
		if !res.Removed {
			preserved = append(preserved, fmt.Sprintf("%s (archived to %s, kept: %s)", d.Name, res.Ref, res.WhyKept))
		}
		if res.KeptBranch != "" {
			kept[res.KeptBranch] = true
		}
	}
	return preserved, kept, archived, nil
}

func (m *Manager) runSweep(ctx context.Context, repoRoot string, maxAgeHours float64, settings Settings) error {
	plan, err := m.PlanSweep(ctx, repoRoot, maxAgeHours, settings, nil)
	if err != nil {
		return err
	}
	preserved, kept, archived, err := m.ExecutePlan(ctx, repoRoot, plan, settings)
	if err != nil {
		return err
	}
	removed := 0
	for _, a := range archived {
		if a.Removed {
			removed++
		}
	}
	if removed > 0 {
		slog.Warn(fmt.Sprintf("Archived and removed %d idle worktree(s) with unmerged work; restore any with `icebath worktree restore <name>` (`icebath worktree list` shows archives).", removed))
	}
	if len(preserved) > 0 {
		sorted := slices.Clone(preserved)
		slices.Sort(sorted)
		if len(sorted) > 20 {
			sorted = sorted[:20]
		}
		slog.Warn(fmt.Sprintf("Preserving %d stale worktree(s) (run `icebath worktree prune --dry-run` for reasons): %s", len(preserved), strings.Join(sorted, ", ")))
	}
	var errs []error
	expired, err := m.PruneExpiredArchives(ctx, repoRoot, settings.ArchiveRetentionDays, false)
	if err != nil {
		if fatal(ctx, err) {
			return err
		}
		errs = append(errs, err)
	}
	if len(expired) > 0 {
		slog.Info(fmt.Sprintf("Deleted %d worktree archive(s) older than %g days", len(expired), settings.ArchiveRetentionDays))
	}
	if err := m.pruneOrphanedBranches(ctx, repoRoot, kept); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) StillInUse(ctx context.Context, path string) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, err := m.LivePIDs([]string{path}, []int{os.Getpid()})
		if err != nil {
			return false, err
		}
		if len(live[path]) == 0 {
			return false, nil
		}
		if !time.Now().Before(deadline) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
