package worktrees

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Prefixes of the trees icebath names itself, aged on the base tier; any other tree is named by the user and gets three times as long
var autoPrefixes = []string{"icebath-", "subagent-"}

// Idle hours before a tree may be reclaimed: the base tier for trees icebath named, three times it for the user's
func Tier(name string, hours float64) float64 {
	for _, prefix := range autoPrefixes {
		if strings.HasPrefix(name, prefix) {
			return hours
		}
	}
	return hours * 3
}

// Tree past its tier, when it was last changed and whether it is past three times its tier
type candidate struct {
	entry string
	mtime time.Time
	force bool
}

var kanbanPrefix = regexp.MustCompile(`^t_[0-9a-f]+`)

// Trees under .worktrees past their tier by the time of their folder alone, kanban trees left out
func candidates(dir string, hours float64, now time.Time) []candidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || kanbanPrefix.MatchString(entry.Name()) {
			continue
		}
		tier := time.Duration(Tier(entry.Name(), hours) * float64(time.Hour))
		if info.ModTime().After(now.Add(-tier)) {
			continue
		}
		out = append(out, candidate{path, info.ModTime(), !info.ModTime().After(now.Add(-3 * tier))})
	}
	return out
}

// A candidate with its verdict, dirty, unpushed, locked-live, reap or reap-keep-branch, and the state of its lock
type pruneVerdict struct {
	candidate
	verdict string
	lock    string
}

// Classifies the candidates, several at a time and changing nothing: whether they hold work, have unpushed commits, are locked by a
// process or session still there, or may be reaped with or without their branch
func (w *Worktrees) classify(cfg *config.Config, repo string, found []candidate) []pruneVerdict {
	cache := loadVerdicts(cfg)
	heads := sync.OnceValue(func() map[string]string { return remoteHeads(repo, 10*time.Second) })
	one := func(c candidate) pruneVerdict {
		if Dirty(c.entry, repo, 5*time.Second) {
			return pruneVerdict{c, "dirty", ""}
		}
		keepBranch := false
		if Unpushed(c.entry, 5*time.Second) {
			merged := allMerged(c.entry, 30*time.Second, 20, cache) || prMerged(c.entry, 15*time.Second, cache)
			if !merged && !pushedExact(c.entry, heads(), 10*time.Second) {
				return pruneVerdict{c, "unpushed", ""}
			}
			keepBranch = !merged
		}
		lock := w.lockState(repo, c.entry, 5*time.Second)
		switch {
		case lock == "live":
			return pruneVerdict{c, "locked-live", ""}
		case keepBranch:
			return pruneVerdict{c, "reap-keep-branch", lock}
		}
		return pruneVerdict{c, "reap", lock}
	}
	verdicts := make([]pruneVerdict, len(found))
	slots := make(chan struct{}, max(1, min(8, runtime.NumCPU(), len(found))))
	var wg sync.WaitGroup
	for i, c := range found {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			verdicts[i] = one(c)
		})
	}
	wg.Wait()
	cache.save()
	return verdicts
}

// Why a verdict keeps a tree that holds work
var keepReasons = map[string]string{"dirty": "uncommitted changes", "unpushed": "unpushed commits"}

// Unlocks, removes and deletes the branch of every reapable verdict in turn: the trees kept that are older than the cutoff, and the
// branches kept
func (w *Worktrees) reap(repo string, verdicts []pruneVerdict, staleBefore time.Time) ([]string, map[string]bool) {
	var stale []string
	kept := map[string]bool{}
	for _, v := range verdicts {
		name := filepath.Base(v.entry)
		if reason, ok := keepReasons[v.verdict]; ok {
			if !v.mtime.After(staleBefore) {
				stale = append(stale, fmt.Sprintf("%s (%s)", name, reason))
			}
			continue
		}
		if v.verdict == "locked-live" {
			slog.Debug("Skipping live-locked worktree: " + name)
			continue
		}
		if blocked := Blocked(v.entry); blocked != nil {
			stale = append(stale, fmt.Sprintf("%s (%s)", name, blocked.Reason))
			continue
		}
		if v.lock == "dead" {
			quiet(repo, nil, 10*time.Second, "Failed to unlock dead worktree "+name, "worktree", "unlock", v.entry)
		}
		branch, err := w.reapOne(repo, v)
		if err != nil {
			slog.Debug("Failed to prune worktree "+name, "error", err)
		}
		if branch != "" && v.verdict == "reap-keep-branch" {
			kept[branch] = true
		}
	}
	return stale, kept
}

// Removes one reapable tree and, unless its verdict keeps it, its branch: the branch it had, empty when it was not removed
func (w *Worktrees) reapOne(repo string, v pruneVerdict) (string, error) {
	name := filepath.Base(v.entry)
	current, err := run(v.entry, nil, 5*time.Second, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(current.Stdout)
	w.Hooks().Release(v.entry)
	removed, err := run(repo, nil, 15*time.Second, "worktree", "remove", v.entry, "--force")
	if err != nil {
		return "", err
	}
	if removed.Code != 0 {
		slog.Debug("Failed to remove worktree "+name, "error", strings.TrimSpace(removed.Stderr))
		return "", nil
	}
	if branch != "" && v.verdict != "reap-keep-branch" {
		if _, err := run(repo, plain(), 10*time.Second, "branch", "-D", branch); err != nil {
			return branch, err
		}
	}
	slog.Debug("Pruned stale worktree", "tree", name, "force", v.force)
	return branch, nil
}

// Removes the trees of the repository idle past their tier and the branches no tree has, archiving the ones holding work first unless the
// settings act as the pruner before archives; then reclaims what features keep for checkouts that are gone
func (w *Worktrees) PruneStale(ctx context.Context, cfg *config.Config, repo string, hours float64) error {
	defer w.Hooks().Reclaim(ctx)
	dir := filepath.Join(repo, ".worktrees")
	if _, err := os.Stat(dir); err != nil {
		orphanBranches(repo, nil)
		return nil
	}
	if Shallow(repo, 5*time.Second) {
		deepen(repo, 10*time.Minute)
	}
	settings := w.Settings(cfg)
	if !settings.Classic() {
		if err := w.Sweep(cfg, repo, hours); err != nil {
			return err
		}
		footprint(repo)
		return nil
	}
	now := time.Now()
	found := candidates(dir, hours, now)
	if len(found) == 0 {
		orphanBranches(repo, nil)
		return nil
	}
	stale, kept := w.reap(repo, w.classify(cfg, repo, found), now.Add(-7*24*time.Hour))
	if len(stale) > 0 {
		sort.Strings(stale)
		slog.Warn(fmt.Sprintf("Preserving %d worktree(s) older than 7 days with unmerged work (run `icebath worktree prune` to review and "+
			"reclaim): %s", len(stale), strings.Join(stale, ", ")))
	}
	orphanBranches(repo, kept)
	footprint(repo)
	return nil
}

// Warns when .worktrees holds ten trees or more, or five gigabytes
func footprint(repo string) {
	count, mb := Summary(repo, 10)
	if count >= 10 || mb != nil && *mb >= 5120 {
		size := ""
		if mb != nil && *mb != 0 {
			size = fmt.Sprintf(" (%.1fGB)", float64(*mb)/1024)
		}
		slog.Warn(fmt.Sprintf(".worktrees/ holds %d tree(s)%s — run `icebath worktree list` to audit and `icebath worktree prune` to "+
			"reclaim safely.", count, size))
	}
}

// Deletes the local branches of trees icebath made, and of pull requests, that no tree has checked out, but the protected ones
func orphanBranches(repo string, protect map[string]bool) {
	listing, ok, err := output(repo, 10*time.Second, "branch", "--format=%(refname:short)")
	if err != nil || !ok {
		return
	}
	trees, err := run(repo, nil, 10*time.Second, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	active := map[string]bool{"main": true}
	for line := range strings.Lines(trees.Stdout) {
		if branch, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "branch refs/heads/"); ok {
			active[strings.TrimSpace(branch)] = true
		}
	}
	if current, err := run(repo, nil, 5*time.Second, "branch", "--show-current"); err == nil {
		if branch := strings.TrimSpace(current.Stdout); branch != "" {
			active[branch] = true
		}
	}
	var orphaned []string
	for _, branch := range strings.Split(listing, "\n") {
		branch = strings.TrimSpace(branch)
		if branch != "" && !active[branch] && !protect[branch] && (strings.HasPrefix(branch, "icebath/icebath-") || strings.HasPrefix(branch, "pr-")) {
			orphaned = append(orphaned, branch)
		}
	}
	if len(orphaned) == 0 {
		return
	}
	for chunk := range slices.Chunk(orphaned, 50) {
		quiet(repo, plain(), 30*time.Second, "Failed to prune orphaned branches", append([]string{"branch", "-D"}, chunk...)...)
	}
	slog.Debug("Pruned orphaned branches", "count", len(orphaned))
}

// One planned action of the sweep for a tree, with what it was decided on
type Decision struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Action   string  `json:"action"`
	Reason   string  `json:"reason"`
	AgeHours float64 `json:"age_hours"`
	Mtime    float64 `json:"mtime"`
	Force    bool    `json:"force"`
	Verdict  string  `json:"verdict"`
	Lock     string  `json:"lock_state,omitempty"`
	SizeMB   *int    `json:"size_mb"`
	Tier     float64 `json:"tier_hours"`
	Planned  float64 `json:"planned_at"`
	mtime    time.Time
	planned  time.Time
}

// Hours rounded to a tenth
func tenths(hours float64) float64 { return float64(int(hours*10+0.5)) / 10 }

// What the sweep makes of a dirty or unpushed tree past its tier: its action and reason, its lock state and when it last changed
func (w *Worktrees) survivor(repo, entry, verdict string, settings Settings, tier time.Duration, now time.Time) (string, string, string, time.Time) {
	what := keepReasons[verdict]
	lock := w.lockState(repo, entry, 5*time.Second)
	if lock == "live" {
		return "skip-live-lock", "lock owner still running", lock, time.Time{}
	}
	active := lastActivity(entry)
	if active.After(now.Add(-tier)) {
		return "skip-young", fmt.Sprintf("%s; last activity %.1fh ago", what, now.Sub(active).Hours()), lock, active
	}
	if !settings.ArchiveDirty {
		return "keep-" + verdict, what, lock, active
	}
	if unsafe := Preflight(entry, settings.ArchiveMaxFileMB); unsafe != nil {
		return unsafe.Action, unsafe.Reason, lock, active
	}
	return "archive-remove", what, lock, active
}

// Archives and removes the oldest idle trees kept for their work until all of them together fit the size
func budget(decisions []*Decision, maxGB, maxFileMB float64) {
	var eligible []*Decision
	total := 0
	for _, d := range decisions {
		if d.Action != "keep-dirty" && d.Action != "keep-unpushed" {
			continue
		}
		size := 0
		if mb := SizeMB(d.Path, 2*time.Minute); mb != nil {
			size = *mb
		}
		d.SizeMB = &size
		total += size
		eligible = append(eligible, d)
	}
	slices.SortStableFunc(eligible, func(a, b *Decision) int { return a.mtime.Compare(b.mtime) })
	for _, d := range eligible {
		if float64(total) <= maxGB*1024 {
			break
		}
		if unsafe := Preflight(d.Path, maxFileMB); unsafe != nil {
			d.Action, d.Reason = unsafe.Action, unsafe.Reason
			continue
		}
		d.Action, d.Reason = "archive-remove", fmt.Sprintf("%s; over Worktree disk limit=%sGB", d.Reason, strconv.FormatFloat(maxGB, 'g', -1, 64))
		total -= *d.SizeMB
	}
}

// Actions of the sweep that change a tree
var destructive = []string{"remove", "remove-keep-branch", "archive-remove"}

// What the sweep would do with every tree under .worktrees, changing nothing: olderThanDays, when given, only holds back removals
func (w *Worktrees) Plan(cfg *config.Config, repo string, hours float64, olderThanDays *float64) []*Decision {
	settings, now := w.Settings(cfg), time.Now()
	dir := filepath.Join(repo, ".worktrees")
	if !fsx.IsDir(dir) {
		return nil
	}
	found := candidates(dir, hours, now)
	doomed := map[string]bool{}
	for _, c := range found {
		doomed[filepath.Base(c.entry)] = true
	}
	var decisions []*Decision
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || doomed[entry.Name()] {
			continue
		}
		d := &Decision{Name: entry.Name(), Path: path, Action: "skip-young", Reason: "younger than its age tier", AgeHours: tenths(now.Sub(info.ModTime()).Hours())}
		if kanbanTree.MatchString(entry.Name()) {
			d.Action, d.Reason = "skip-kanban", "owned by kanban gc"
		}
		decisions = append(decisions, d)
	}
	var verdicts []pruneVerdict
	if len(found) > 0 {
		verdicts = w.classify(cfg, repo, found)
	}
	live := map[string][]int{}
	if !settings.Classic() {
		var paths []string
		for _, v := range verdicts {
			paths = append(paths, v.entry)
		}
		live = Live(paths)
	}
	for _, v := range verdicts {
		name := filepath.Base(v.entry)
		tier := Tier(name, hours)
		d := &Decision{Name: name, Path: v.entry, AgeHours: tenths(now.Sub(v.mtime).Hours()), Mtime: float64(v.mtime.Unix()), Force: v.force,
			Verdict: v.verdict, Lock: v.lock, Tier: tier, Planned: float64(now.Unix()), mtime: v.mtime, planned: now}
		_, archivable := keepReasons[v.verdict]
		switch {
		case len(live[v.entry]) > 0:
			d.Action, d.Reason = "skip-live-process", "in use by pid "+pids(live[v.entry])
		case v.verdict == "locked-live":
			d.Action, d.Reason = "skip-live-lock", "lock owner still running"
		case archivable && settings.Classic():
			d.Action, d.Reason = "keep-"+v.verdict, keepReasons[v.verdict]
		case archivable:
			var active time.Time
			d.Action, d.Reason, d.Lock, active = w.survivor(repo, v.entry, v.verdict, settings, time.Duration(tier*float64(time.Hour)), now)
			if !active.IsZero() {
				d.AgeHours = min(d.AgeHours, tenths(now.Sub(active).Hours()))
			}
		default:
			switch blocked := Blocked(v.entry); {
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
		budget(decisions, settings.MaxTotalGB, settings.ArchiveMaxFileMB)
	}
	if olderThanDays != nil {
		for _, d := range decisions {
			if slices.Contains(destructive, d.Action) && d.AgeHours < *olderThanDays*24 {
				d.Action, d.Reason = "skip-young", fmt.Sprintf("idle %.1fh (--older-than %s)", d.AgeHours, strconv.FormatFloat(*olderThanDays, 'g', -1, 64))
			}
		}
	}
	return decisions
}

// Whether the branch is on a remote; when that can't be told it counts as published, so doubt keeps it
func published(repo, branch string, heads map[string]string, known bool) bool {
	remotes, ok, err := output(repo, 10*time.Second, "remote")
	switch {
	case err != nil:
		return true
	case ok && remotes == "":
		return false
	case !known:
		return true
	}
	if _, in := heads[branch]; in {
		return true
	}
	refs, ok, err := output(repo, 10*time.Second, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch)
	return err != nil || !ok || refs != ""
}

// Why the staged state no longer matches the archive's, empty when it still does
func indexMoved(tree, captured string) string {
	if state, err := StagedState(tree); err == nil && state == captured {
		return ""
	}
	return "staged changes since the snapshot"
}

// What archiving then removing one tree came to
type ArchiveRemoval struct {
	Ref string
	// Branch kept with the tree gone, empty for none
	KeptBranch string
	Removed    bool
	// Why the tree was kept after its archive
	WhyKept string
}

// Archives the tree, then removes it unless it changed meanwhile, is in use, or holds what the archive lacks; its branch goes too unless
// it is kept, protected or published. heads gives origin's branches, read when nil
func (w *Worktrees) ArchiveThenRemove(repo, tree, reason string, maxMB float64, unlock bool, heads func() (map[string]string, bool), keepBranch bool, exclude ...int) (*ArchiveRemoval, error) {
	started := time.Now()
	archive, err := Snapshot(repo, tree, reason, maxMB)
	if err != nil {
		return nil, err
	}
	blocked := Blocker(tree, archive.Meta.Skipped, true)
	busy := inUseSince(tree, started, exclude...)
	if busy == "" {
		busy = indexMoved(tree, archive.Staged)
	}
	if busy == "" && blocked != nil {
		busy = blocked.Reason
	}
	if busy != "" {
		slog.Warn("Archived "+tree+" to "+archive.Ref+" but keeping it", "reason", busy)
		return &ArchiveRemoval{Ref: archive.Ref, WhyKept: busy}, nil
	}
	w.Hooks().Release(tree)
	if unlock {
		quiet(repo, nil, 10*time.Second, "unlock "+tree, "worktree", "unlock", tree)
	}
	removed, err := run(repo, nil, time.Minute, "worktree", "remove", "--force", tree)
	if err != nil {
		return nil, err
	}
	if removed.Code != 0 {
		stderr := strings.TrimSpace(removed.Stderr)
		slog.Warn("Archived "+tree+" to "+archive.Ref+" but could not remove it", "error", stderr)
		return &ArchiveRemoval{Ref: archive.Ref, WhyKept: cmp.Or(stderr, "git worktree remove failed")}, nil
	}
	branch := archive.Meta.Branch
	if branch == "" || keepBranch || protected[branch] {
		return &ArchiveRemoval{Ref: archive.Ref, KeptBranch: branch, Removed: true}, nil
	}
	if heads == nil {
		heads = func() (map[string]string, bool) {
			found := remoteHeads(repo, 10*time.Second)
			return found, found != nil
		}
	}
	if found, known := heads(); published(repo, branch, found, known) {
		return &ArchiveRemoval{Ref: archive.Ref, KeptBranch: branch, Removed: true}, nil
	}
	if _, err := run(repo, plain(), 10*time.Second, "branch", "-D", branch); err != nil {
		return nil, err
	}
	return &ArchiveRemoval{Ref: archive.Ref, Removed: true}, nil
}

// Checks one planned tree again right before it is touched: why to keep it now, and the state of its lock
func (w *Worktrees) recheck(repo string, d *Decision) (string, string) {
	if !fsx.IsDir(d.Path) {
		return "gone", ""
	}
	lock := w.lockState(repo, d.Path, 5*time.Second)
	if lock == "live" {
		return "lock owner still running", lock
	}
	since := d.planned
	if since.IsZero() {
		since = time.Now()
	}
	if d.Action == "archive-remove" && d.Tier != 0 {
		if earlier := time.Now().Add(-time.Duration(d.Tier * float64(time.Hour))); earlier.Before(since) {
			since = earlier
		}
	}
	if busy := inUseSince(d.Path, since); busy != "" {
		return busy, lock
	}
	if d.Action != "archive-remove" && Dirty(d.Path, repo, 10*time.Second) {
		return "new uncommitted changes", lock
	}
	return "", lock
}

// What the sweep did with one tree it archived
type ArchivedTree struct {
	Ref     string `json:"ref"`
	Removed bool   `json:"removed"`
	WhyKept string `json:"why_kept,omitempty"`
}

// Carries out a plan, checking each tree again right before it is archived or removed: the trees kept for their work, the branches kept,
// and what was archived
func (w *Worktrees) Execute(cfg *config.Config, repo string, plan []*Decision) ([]string, map[string]bool, []ArchivedTree) {
	settings := w.Settings(cfg)
	heads := sync.OnceValues(func() (map[string]string, bool) {
		found := remoteHeads(repo, 10*time.Second)
		return found, found != nil
	})
	kept := map[string]bool{}
	var archived []ArchivedTree
	var preserved []string
	for _, d := range plan {
		if strings.HasPrefix(d.Action, "keep-") {
			preserved = append(preserved, fmt.Sprintf("%s (%s)", d.Name, d.Reason))
		}
	}
	for _, d := range plan {
		if !slices.Contains(destructive, d.Action) {
			continue
		}
		why, lock := w.recheck(repo, d)
		if why != "" {
			slog.Info("Sweep: keeping "+d.Name, "reason", why)
			continue
		}
		if d.Action != "archive-remove" {
			_, branches := w.reap(repo, []pruneVerdict{{candidate{d.Path, d.mtime, d.Force}, d.Verdict, lock}}, time.Time{})
			for branch := range branches {
				kept[branch] = true
			}
			continue
		}
		removal, err := w.ArchiveThenRemove(repo, d.Path, "sweep: "+d.Reason, settings.ArchiveMaxFileMB, lock == "dead", heads, false)
		if err != nil {
			slog.Warn("Keeping worktree "+d.Name+": archive failed", "error", err)
			preserved = append(preserved, d.Name+" (archive failed)")
			continue
		}
		archived = append(archived, ArchivedTree{removal.Ref, removal.Removed, removal.WhyKept})
		if !removal.Removed {
			preserved = append(preserved, fmt.Sprintf("%s (archived to %s, kept: %s)", d.Name, removal.Ref, removal.WhyKept))
		}
		if removal.KeptBranch != "" {
			kept[removal.KeptBranch] = true
		}
	}
	return preserved, kept, archived
}

// Plans and carries out the sweep of one repository, deletes the archives past their retention and the branches no tree has
func (w *Worktrees) Sweep(cfg *config.Config, repo string, hours float64) error {
	preserved, kept, archived := w.Execute(cfg, repo, w.Plan(cfg, repo, hours, nil))
	removed := 0
	for _, a := range archived {
		if a.Removed {
			removed++
		}
	}
	if removed > 0 {
		slog.Warn(fmt.Sprintf("Archived and removed %d idle worktree(s) with unmerged work; restore any with `icebath worktree restore <name>` "+
			"(`icebath worktree list` shows archives).", removed))
	}
	if len(preserved) > 0 {
		sort.Strings(preserved)
		slog.Warn(fmt.Sprintf("Preserving %d stale worktree(s) (run `icebath worktree prune --dry-run` for reasons): %s", len(preserved),
			strings.Join(preserved[:min(20, len(preserved))], ", ")))
	}
	expired, err := Expire(repo, w.Settings(cfg).ArchiveRetentionDays, false)
	if err != nil {
		return err
	}
	if len(expired) > 0 {
		slog.Info(fmt.Sprintf("Deleted %d worktree archive(s) older than %g days", len(expired), w.Settings(cfg).ArchiveRetentionDays))
	}
	orphanBranches(repo, kept)
	return nil
}
