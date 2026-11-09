package tools

import (
	"context"
	"fmt"
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

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/lsp"
)

// Worktrees section of the active profile config with every field coerced
type SweepConfig struct {
	ArchiveDirty         bool
	ArchiveMaxFileMB     float64
	ArchiveRetentionDays float64
	MaxTotalGB           float64
}

var sweepDefaults = []struct {
	key string
	val any
}{{"archive_dirty", true}, {"archive_max_file_mb", int64(50)}, {"archive_retention_days", int64(30)}, {"max_total_gb", int64(0)}}

var protectedBranches = map[string]bool{"main": true, "master": true, "develop": true, "dev": true, "trunk": true}

var archiveReasons = map[string]string{"dirty": "uncommitted changes", "unpushed": "unpushed commits"}

// One planned sweep action for a tree with the facts it was decided on
type Decision struct {
	Name      string
	Path      string
	Action    string
	Reason    string
	AgeHours  float64
	Branch    *string
	Mtime     float64
	Force     bool
	Verdict   string
	LockState string
	SizeMB    *int
	TierHours float64
	PlannedAt float64
}

// Field map of the decision in declaration order, absent branch, lock state and size as null
func (d *Decision) AsDict() *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", d.Name)
	o.Set("path", d.Path)
	o.Set("action", d.Action)
	o.Set("reason", d.Reason)
	o.Set("age_hours", d.AgeHours)
	if d.Branch != nil {
		o.Set("branch", *d.Branch)
	} else {
		o.Set("branch", nil)
	}
	o.Set("mtime", d.Mtime)
	o.Set("force", d.Force)
	o.Set("verdict", d.Verdict)
	if d.LockState != "" {
		o.Set("lock_state", d.LockState)
	} else {
		o.Set("lock_state", nil)
	}
	if d.SizeMB != nil {
		o.Set("size_mb", int64(*d.SizeMB))
	} else {
		o.Set("size_mb", nil)
	}
	o.Set("tier_hours", d.TierHours)
	o.Set("planned_at", d.PlannedAt)
	return o
}

// Loads the readonly config tree of the active profile, swapped by tests
var sweepLoadConfig = func() (config.Tree, error) {
	h := home.Process()
	return config.LoadReadonly(context.Background(), h, config.NewSecrets(h).Lookup())
}

// Worktrees settings of the active profile, defaults on any read failure and per field on a bad value
func SweepSettings() SweepConfig {
	raw := map[string]any{}
	for _, d := range sweepDefaults {
		raw[d.key] = d.val
	}
	tree, err := sweepLoadConfig()
	if err != nil {
		toolsLogger(nil).Debug("worktrees config unreadable; using defaults")
	} else if section, ok := tree["worktrees"].(map[string]any); ok {
		for _, d := range sweepDefaults {
			if v, ok := section[d.key]; ok && v != nil {
				raw[d.key] = v
			}
		}
	}
	num := func(key string, def int64) float64 {
		f, err := pyint.Float(raw[key])
		if err != nil {
			return float64(def)
		}
		return max(0.0, f)
	}
	return SweepConfig{
		ArchiveDirty:         pyTruthy(raw["archive_dirty"]),
		ArchiveMaxFileMB:     num("archive_max_file_mb", 50),
		ArchiveRetentionDays: num("archive_retention_days", 30),
		MaxTotalGB:           num("max_total_gb", 0),
	}
}

// Whether the sweep must act exactly like the pre archive pruner
func ClassicMode(s SweepConfig) bool { return !s.ArchiveDirty && s.MaxTotalGB == 0 }

func sweepInside(target string, roots []string) string {
	for _, root := range roots {
		if target == root || strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return root
		}
	}
	return ""
}

// Pids by root of every process whose cwd or open fd lies inside a root, read from proc
func linuxLivePids(roots []string, exclude map[int]bool) (map[string][]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	hits := map[string][]int{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !pyIsDigits(e.Name()) || exclude[pid] {
			continue
		}
		pidDir := filepath.Join("/proc", e.Name())
		links := []string{filepath.Join(pidDir, "cwd")}
		if fds, err := os.ReadDir(filepath.Join(pidDir, "fd")); err == nil {
			for _, fd := range fds {
				links = append(links, filepath.Join(pidDir, "fd", fd.Name()))
			}
		}
		for _, link := range links {
			target, err := os.Readlink(link)
			if err != nil {
				continue
			}
			if root := sweepInside(strings.TrimSuffix(target, " (deleted)"), roots); root != "" {
				hits[root] = append(hits[root], pid)
				break
			}
		}
	}
	return hits, nil
}

func pyIsDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Pids by root of every process whose cwd lies inside a root, through the process table
func psutilLivePids(roots []string, exclude map[int]bool) map[string][]int {
	hits := map[string][]int{}
	pids, err := proc.PIDs()
	if err != nil {
		return hits
	}
	for _, pid := range pids {
		if exclude[pid] {
			continue
		}
		cwd, err := proc.Cwd(pid)
		if err != nil {
			continue
		}
		real, err := pypath.Realpath(cwd, pypath.Lax)
		if err != nil {
			continue
		}
		if root := sweepInside(real, roots); root != "" {
			hits[root] = append(hits[root], pid)
		}
	}
	return hits
}

// Pids of live processes working in each path keyed by the path as given, excluded pids ignored
func LiveProcessPids(paths []string, exclude []int) map[string][]int {
	real := map[string]string{}
	for _, p := range paths {
		r, err := pypath.Realpath(p, pypath.Lax)
		if err != nil {
			r = p
		}
		real[r] = p
	}
	if len(real) == 0 {
		return map[string][]int{}
	}
	roots := make([]string, 0, len(real))
	for r := range real {
		roots = append(roots, r)
	}
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	skip := map[int]bool{}
	for _, p := range exclude {
		skip[p] = true
	}
	var hits map[string][]int
	if runtime.GOOS == "linux" {
		var err error
		if hits, err = linuxLivePids(roots, skip); err != nil {
			hits = map[string][]int{}
		}
	} else {
		hits = psutilLivePids(roots, skip)
	}
	out := map[string][]int{}
	for root, pids := range hits {
		out[real[root]] = pids
	}
	return out
}

// Newest mtime among HEAD, its reflog, every changed or untracked file and the surviving parent of each change
func lastActivity(path string) float64 {
	env := noninteractiveRepoGitEnv(path, nil)
	if env == nil {
		return nowSeconds()
	}
	env["GIT_OPTIONAL_LOCKS"] = "0"
	run := func(timeout time.Duration, args ...string) (*pysubprocess.Completed, error) {
		return pysubprocess.RunWith(context.Background(), append([]string{"git"}, args...), pysubprocess.Options{Dir: path, Env: mapToEnviron(env), Timeout: timeout})
	}
	gd, err := run(10*time.Second, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nowSeconds()
	}
	status, err := run(60*time.Second, "status", "--porcelain", "-z", "-uall", "--no-renames")
	if err != nil || status.Returncode != 0 {
		return nowSeconds()
	}
	gitDir := pystr.Strip(string(gd.Stdout))
	var candidates []string
	if gitDir != "" {
		candidates = append(candidates, filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"))
	}
	var changed []string
	for _, entry := range strings.Split(string(status.Stdout), "\x00") {
		if len(entry) > 3 {
			changed = append(changed, filepath.Join(path, entry[3:]))
		}
	}
	candidates = append(candidates, changed...)
	wt := filepath.Clean(path)
	for _, item := range changed {
		parent := filepath.Dir(item)
		for parent != wt && strings.HasPrefix(parent, wt+string(os.PathSeparator)) {
			if _, err := os.Lstat(parent); err == nil {
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
		if st, err := os.Lstat(c); err == nil {
			newest = max(newest, float64(st.ModTime().UnixNano())/1e9)
		}
	}
	return newest
}

// Action, reason, lock state and last activity of a dirty or unpushed tree past its tier
func planSurvivor(repoRoot, entry, verdict string, s SweepConfig, tierHours, now float64) (string, string, string, float64) {
	what := archiveReasons[verdict]
	lockState := worktreeLockIsLive(repoRoot, entry, 5*time.Second)
	if lockState == "live" {
		return "skip-live-lock", "lock owner still running", lockState, 0
	}
	active := lastActivity(entry)
	if active > now-tierHours*3600 {
		return "skip-young", fmt.Sprintf("%s; last activity %.1fh ago", what, (now-active)/3600), lockState, active
	}
	if !s.ArchiveDirty {
		return "keep-" + verdict, what, lockState, active
	}
	if unsafe := ArchivePreflight(entry, s.ArchiveMaxFileMB); unsafe != nil {
		return unsafe.Action, unsafe.Reason, lockState, active
	}
	return "archive-remove", what, lockState, active
}

// Sizes trees in megabytes, swapped by tests
var sweepTreeSizeMB = TreeSizeMB

// Evicts the oldest idle kept trees, archive first, until they fit the total size budget
func applyBudget(decisions []*Decision, maxTotalGB, maxFileMB float64) {
	var eligible []*Decision
	for _, d := range decisions {
		if d.Action == "keep-dirty" || d.Action == "keep-unpushed" {
			eligible = append(eligible, d)
		}
	}
	total := 0
	for _, d := range eligible {
		size := 0
		if mb := sweepTreeSizeMB(d.Path, 120*time.Second); mb != nil {
			size = *mb
		}
		d.SizeMB = &size
		total += size
	}
	budgetMB := maxTotalGB * 1024
	ordered := slices.Clone(eligible)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Mtime < ordered[j].Mtime })
	for _, d := range ordered {
		if float64(total) <= budgetMB {
			break
		}
		if unsafe := ArchivePreflight(d.Path, maxFileMB); unsafe != nil {
			d.Action, d.Reason = unsafe.Action, unsafe.Reason
			continue
		}
		d.Action, d.Reason = "archive-remove", fmt.Sprintf("%s; over worktrees.max_total_gb=%s", d.Reason, pyFormatG(maxTotalGB))
		total -= *d.SizeMB
	}
}

var destructiveActions = []string{"remove", "remove-keep-branch", "archive-remove"}

var kanbanTreeFullRe = regexp.MustCompile(`^t_[0-9a-f]+\n?$`)

// Read only decision for every tree under the worktrees dir, olderThanDays when positive only restricts removal
func PlanSweep(repoRoot string, maxAgeHours int, s *SweepConfig, now float64, olderThanDays *float64) []*Decision {
	settings := SweepSettings
	cfg := SweepConfig{}
	if s != nil {
		cfg = *s
	} else {
		cfg = settings()
	}
	if now == 0 {
		now = nowSeconds()
	}
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if st, err := os.Stat(worktreesDir); err != nil || !st.IsDir() {
		return nil
	}
	candidates := pruneCandidates(worktreesDir, maxAgeHours, now)
	doomed := map[string]bool{}
	for _, c := range candidates {
		doomed[filepath.Base(c.entry)] = true
	}
	var decisions []*Decision
	ents, _ := os.ReadDir(worktreesDir)
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		entry := filepath.Join(worktreesDir, name)
		st, err := os.Stat(entry)
		if err != nil || !st.IsDir() || doomed[name] {
			continue
		}
		age := (now - float64(st.ModTime().UnixNano())/1e9) / 3600
		d := &Decision{Name: name, Path: entry, Action: "skip-young", Reason: "younger than its age tier", AgeHours: pyint.Round(age, 1)}
		if kanbanTreeFullRe.MatchString(name) {
			d.Action, d.Reason = "skip-kanban", "owned by kanban gc"
		}
		decisions = append(decisions, d)
	}
	var verdicts []pruneVerdict
	if len(candidates) > 0 {
		verdicts = classifyPruneCandidates(repoRoot, candidates)
	}
	live := map[string][]int{}
	if !ClassicMode(cfg) {
		var paths []string
		for _, v := range verdicts {
			paths = append(paths, v.entry)
		}
		live = LiveProcessPids(paths, nil)
	}
	for _, v := range verdicts {
		name := filepath.Base(v.entry)
		tier := PruneTierHours(name, float64(maxAgeHours))
		d := &Decision{Name: name, Path: v.entry, AgeHours: pyint.Round((now-v.mtime)/3600, 1), Mtime: v.mtime, Force: v.force,
			Verdict: v.verdict, LockState: v.lockState, TierHours: tier, PlannedAt: now}
		_, archivable := archiveReasons[v.verdict]
		switch {
		case len(live[v.entry]) > 0:
			d.Action, d.Reason = "skip-live-process", "in use by pid "+joinPids(live[v.entry])
		case v.verdict == "locked-live":
			d.Action, d.Reason = "skip-live-lock", "lock owner still running"
		case archivable && ClassicMode(cfg):
			d.Action, d.Reason = "keep-"+v.verdict, archiveReasons[v.verdict]
		case archivable:
			var active float64
			d.Action, d.Reason, d.LockState, active = planSurvivor(repoRoot, v.entry, v.verdict, cfg, tier, now)
			if active != 0 {
				d.AgeHours = min(d.AgeHours, pyint.Round((now-active)/3600, 1))
			}
		default:
			if blocked := RemovalBlocked(v.entry); blocked != nil {
				d.Action, d.Reason = blocked.Action, blocked.Reason
			} else if v.verdict == "reap-keep-branch" {
				d.Action, d.Reason = "remove-keep-branch", "pushed to origin; branch kept"
			} else {
				d.Action, d.Reason = "remove", "clean and merged/pushed"
			}
		}
		decisions = append(decisions, d)
	}
	if cfg.MaxTotalGB != 0 {
		applyBudget(decisions, cfg.MaxTotalGB, cfg.ArchiveMaxFileMB)
	}
	if olderThanDays != nil {
		for _, d := range decisions {
			if slices.Contains(destructiveActions, d.Action) && d.AgeHours < *olderThanDays*24 {
				d.Action, d.Reason = "skip-young", fmt.Sprintf("idle %.1fh (--older-than %s)", d.AgeHours, pyFormatG(*olderThanDays))
			}
		}
	}
	return decisions
}

func joinPids(pids []int) string {
	parts := make([]string, 0, 5)
	for _, p := range pids[:min(5, len(pids))] {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ", ")
}

// Whether the branch exists on a remote, unknown counts as published so doubt keeps it
func branchPublished(repoRoot, branch string, remoteHeads map[string]string, headsKnown bool) bool {
	remotes, ok, err := worktreeGitOut([]string{"remote"}, repoRoot, 10*time.Second)
	if err != nil {
		return true
	}
	if ok && remotes == "" {
		return false
	}
	if !headsKnown {
		return true
	}
	if _, in := remoteHeads[branch]; in {
		return true
	}
	refs, ok, err := worktreeGitOut([]string{"for-each-ref", "--format=%(refname)", "refs/remotes/*/" + branch}, repoRoot, 10*time.Second)
	if err != nil {
		return true
	}
	return !ok || pystr.Strip(refs) != ""
}

// Why the path must not be removed now, a live process in it or a change at or after since, empty while idle
func inUseSince(path string, since float64, exclude []int) string {
	if pids := LiveProcessPids([]string{path}, exclude)[path]; len(pids) > 0 {
		return "in use by pid " + joinPids(pids)
	}
	if active := lastActivity(path); active >= since {
		return fmt.Sprintf("changed %.0fs ago", max(0.0, nowSeconds()-active))
	}
	return ""
}

// Why the staged state no longer matches the archive, empty when it still does
func indexMoved(path, captured string) string {
	if state, err := StagedState(path); err == nil && state == captured {
		return ""
	}
	return "staged changes since the snapshot"
}

// Stops this process's language servers for the tree before it is removed, never blocking removal
func releaseLspClients(wtPath string) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(nil).Debug(fmt.Sprintf("LSP release for worktree %s failed: %v", wtPath, r))
		}
	}()
	lsp.ReleaseWorkspace(wtPath)
}

// ReleaseLspClients stops the language servers rooted at a worktree before its removal
func ReleaseLspClients(wtPath string) { releaseLspClients(wtPath) }

// Outcome of archiving then removing one tree
type ArchiveRemoval struct {
	Ref        string
	KeptBranch *string
	Removed    bool
	WhyKept    string
}

// Snapshot step of every archive, the worktree_archive attribute hermes tests patch
var snapshotWorktreeFn = SnapshotWorktree

// Snapshots the tree then removes it unless it changed, is in use or holds what the archive lacks
func ArchiveThenRemove(repoRoot, path, reason string, maxUntrackedMB float64, unlock bool, remoteHeads func() (map[string]string, bool), keepBranch bool, excludePids []int) (*ArchiveRemoval, error) {
	started := nowSeconds()
	arc, err := snapshotWorktreeFn(repoRoot, path, reason, maxUntrackedMB)
	if err != nil {
		return nil, err
	}
	skippedV, _ := arc.Meta.Get("skipped_untracked")
	var skipped []SkippedUntracked
	if list, ok := skippedV.([]any); ok {
		for _, item := range list {
			if o, ok := item.(*pyjson.Object); ok {
				p, _ := o.Get("path")
				b, _ := o.Get("bytes")
				ps, _ := p.(string)
				bn, _ := b.(int64)
				skipped = append(skipped, SkippedUntracked{ps, bn})
			}
		}
	}
	blocked := RemovalBlocker(path, skipped, true)
	busy := inUseSince(path, started, excludePids)
	if busy == "" {
		busy = indexMoved(path, arc.Staged)
	}
	if busy == "" && blocked != nil {
		busy = blocked.Reason
	}
	if busy != "" {
		toolsLogger(nil).Warn(fmt.Sprintf("Archived %s to %s but keeping it: %s", path, arc.Ref, busy))
		return &ArchiveRemoval{arc.Ref, nil, false, busy}, nil
	}
	releaseLspClients(path)
	if unlock {
		worktreeGitQuiet([]string{"worktree", "unlock", path}, repoRoot, 10*time.Second, "unlock "+path, nil)
	}
	removed, err := worktreeGit([]string{"worktree", "remove", "--force", path}, repoRoot, 60*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if removed.Returncode != 0 {
		stderr := pystr.Strip(removed.Stderr)
		toolsLogger(nil).Warn(fmt.Sprintf("Archived %s to %s but could not remove it: %s", path, arc.Ref, stderr))
		if stderr == "" {
			stderr = "git worktree remove failed"
		}
		return &ArchiveRemoval{arc.Ref, nil, false, stderr}, nil
	}
	branchV, _ := arc.Meta.Get("branch")
	branch, _ := branchV.(string)
	if branch == "" || keepBranch || protectedBranches[branch] {
		var kept *string
		if branch != "" {
			kept = &branch
		}
		return &ArchiveRemoval{arc.Ref, kept, true, ""}, nil
	}
	var heads map[string]string
	known := true
	if remoteHeads == nil {
		heads = fetchRemoteBranchHeads(repoRoot, 10*time.Second)
		known = heads != nil
	} else {
		heads, known = remoteHeads()
	}
	if branchPublished(repoRoot, branch, heads, known) {
		return &ArchiveRemoval{arc.Ref, &branch, true, ""}, nil
	}
	if _, err := worktreeGit([]string{"branch", "-D", branch}, repoRoot, 10*time.Second, NoninteractiveGitEnv(nil)); err != nil {
		return nil, err
	}
	return &ArchiveRemoval{arc.Ref, nil, true, ""}, nil
}

// Fresh gate for one planned tree right before it is touched, the reason to keep it and its lock state
func sweepRecheck(repoRoot string, d *Decision) (string, string) {
	if st, err := os.Stat(d.Path); err != nil || !st.IsDir() {
		return "gone", ""
	}
	lockState := worktreeLockIsLive(repoRoot, d.Path, 5*time.Second)
	if lockState == "live" {
		return "lock owner still running", lockState
	}
	since := d.PlannedAt
	if since == 0 {
		since = nowSeconds()
	}
	if d.Action == "archive-remove" && d.TierHours != 0 {
		since = min(since, nowSeconds()-d.TierHours*3600)
	}
	if busy := inUseSince(d.Path, since, nil); busy != "" {
		return busy, lockState
	}
	if d.Action != "archive-remove" && worktreeIsDirty(d.Path, repoRoot, 10*time.Second) {
		return "new uncommitted changes", lockState
	}
	return "", lockState
}

// Archive ref of one archived tree with whether it was removed and why it was kept
type ArchivedTree struct {
	Ref     string
	Removed bool
	WhyKept string
}

// Applies a frozen plan re gating each tree right before it is archived or removed
func ExecutePlan(repoRoot string, plan []*Decision, s SweepConfig) ([]string, map[string]bool, []ArchivedTree) {
	remoteHeads := sync.OnceValues(func() (map[string]string, bool) {
		h := fetchRemoteBranchHeads(repoRoot, 10*time.Second)
		return h, h != nil
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
		if !slices.Contains(destructiveActions, d.Action) {
			continue
		}
		whyKeep, lockState := sweepRecheck(repoRoot, d)
		if whyKeep != "" {
			toolsLogger(nil).Info(fmt.Sprintf("Sweep: keeping %s (%s)", d.Name, whyKeep))
			continue
		}
		if d.Action != "archive-remove" {
			_, k := reapPruneVerdicts(repoRoot, []pruneVerdict{{pruneCandidate{d.Path, d.Mtime, d.Force}, d.Verdict, lockState}}, 0)
			for b := range k {
				kept[b] = true
			}
			continue
		}
		res, err := ArchiveThenRemove(repoRoot, d.Path, "sweep: "+d.Reason, s.ArchiveMaxFileMB, lockState == "dead", remoteHeads, false, nil)
		if err != nil {
			toolsLogger(nil).Warn(fmt.Sprintf("Keeping worktree %s: archive failed (%s)", d.Name, err))
			preserved = append(preserved, d.Name+" (archive failed)")
			continue
		}
		archived = append(archived, ArchivedTree{res.Ref, res.Removed, res.WhyKept})
		if !res.Removed {
			preserved = append(preserved, fmt.Sprintf("%s (archived to %s, kept: %s)", d.Name, res.Ref, res.WhyKept))
		}
		if res.KeptBranch != nil {
			kept[*res.KeptBranch] = true
		}
	}
	return preserved, kept, archived
}

// Plans, executes and applies archive retention for one repository in archive mode
func RunSweep(repoRoot string, maxAgeHours int, s SweepConfig) error {
	plan := PlanSweep(repoRoot, maxAgeHours, &s, 0, nil)
	preserved, kept, archived := ExecutePlan(repoRoot, plan, s)
	removed := 0
	for _, a := range archived {
		if a.Removed {
			removed++
		}
	}
	if removed > 0 {
		toolsLogger(nil).Warn(fmt.Sprintf("Archived and removed %d idle worktree(s) with unmerged work; restore any with "+
			"`icebath worktree restore <name>` (`icebath worktree list` shows archives).", removed))
	}
	if len(preserved) > 0 {
		sorted := slices.Clone(preserved)
		sort.Strings(sorted)
		toolsLogger(nil).Warn(fmt.Sprintf("Preserving %d stale worktree(s) (run `icebath worktree prune --dry-run` for reasons): %s",
			len(preserved), strings.Join(sorted[:min(20, len(sorted))], ", ")))
	}
	expired, err := PruneExpiredArchives(repoRoot, s.ArchiveRetentionDays, 0, false)
	if err != nil {
		return err
	}
	if len(expired) > 0 {
		toolsLogger(nil).Info(fmt.Sprintf("Deleted %d worktree archive(s) older than %s days", len(expired), pyFormatG(s.ArchiveRetentionDays)))
	}
	pruneOrphanedBranches(repoRoot, kept)
	return nil
}
