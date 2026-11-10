package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Worktree command the subcommand dispatch calls, the worktree_cmd attribute hermes tests patch
var worktreeCmdFn = cmdWorktree

// Size column text, a question mark for an unknown size and gigabytes from 1024 MB up
func worktreeFmtSize(sizeMB *int) string {
	if sizeMB == nil {
		return "?"
	}
	if *sizeMB >= 1024 {
		return fmt.Sprintf("%.1fG", float64(*sizeMB)/1024)
	}
	return fmt.Sprintf("%dM", *sizeMB)
}

// String namespace value, empty for an absent or non string entry
func worktreeNsString(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Truthiness of a namespace value, false for an absent entry
func worktreeNsBool(ns map[string]any, key string) bool {
	v, ok := ns[key]
	return ok && pyTruthy(v)
}

// Optional float namespace value, nil for an absent, nil or non numeric entry
func worktreeNsFloat(ns map[string]any, key string) *float64 {
	v, ok := ns[key]
	if !ok || v == nil {
		return nil
	}
	f, err := pyint.Float(v)
	if err != nil {
		return nil
	}
	return &f
}

// First runes of a text, the whole text when it is shorter
func worktreeHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Ordered dict of a tree record in its dataclass field order
func treeRecordDict(r TreeRecord) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", r.Name)
	o.Set("path", r.Path)
	o.Set("branch", r.Branch)
	o.Set("age_days", r.AgeDays)
	if r.SizeMB != nil {
		o.Set("size_mb", *r.SizeMB)
	} else {
		o.Set("size_mb", nil)
	}
	o.Set("verdict", r.Verdict)
	o.Set("reason", r.Reason)
	untracked := make([]any, 0, len(r.Untracked))
	for _, u := range r.Untracked {
		untracked = append(untracked, u)
	}
	o.Set("untracked", untracked)
	return o
}

// Ordered dict of an external tree record in its dataclass field order
func externalTreeRecordDict(e ExternalTreeRecord) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("path", e.Path)
	o.Set("branch", e.Branch)
	o.Set("locked", e.Locked)
	o.Set("missing", e.Missing)
	return o
}

// Ordered dict of a branch record in its dataclass field order
func branchRecordDict(b BranchRecord) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", b.Name)
	o.Set("verdict", b.Verdict)
	o.Set("reason", b.Reason)
	return o
}

// Ordered dict of an archive in its dataclass field order
func archiveDict(a Archive) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("ref", a.Ref)
	o.Set("name", a.Name)
	o.Set("sha", a.Sha)
	o.Set("created", a.Created)
	meta := a.Meta
	if meta == nil {
		meta = pyjson.NewObject()
	}
	o.Set("meta", meta)
	o.Set("staged", a.Staged)
	return o
}

// Meta text under a key, the fallback for an absent or empty value
func worktreeMetaText(meta *pyjson.Object, key, fallback string) string {
	if meta == nil {
		return fallback
	}
	v, _ := meta.Get(key)
	if !pyTruthy(v) {
		return fallback
	}
	return pyrepr.Str(v)
}

// Writes the two space indented JSON of an ordered object followed by a newline
func worktreePrintJSON(w io.Writer, o *pyjson.Object) error {
	text, err := pyjson.Dumps(o, pyjson.Indent(2))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, text)
	return err
}

// Prints the audit of trees, external trees, branches and archives as a table or JSON
func worktreeCmdList(ctx context.Context, repoRoot string, ns map[string]any, w io.Writer) (int, error) {
	olderThan := worktreeNsFloat(ns, "older_than")
	records, err := AuditWorktrees(ctx, repoRoot, true, olderThan)
	if err != nil {
		return 0, err
	}
	external, err := AuditExternalTrees(repoRoot)
	if err != nil {
		return 0, err
	}
	branchRecords, err := AuditBranches(repoRoot)
	if err != nil {
		return 0, err
	}
	archives, err := ListArchives(repoRoot)
	if err != nil {
		return 0, err
	}
	if worktreeNsBool(ns, "json") {
		o := pyjson.NewObject()
		o.Set("repo", repoRoot)
		trees := make([]any, 0, len(records))
		for _, r := range records {
			trees = append(trees, treeRecordDict(r))
		}
		o.Set("trees", trees)
		externalTrees := make([]any, 0, len(external))
		for _, e := range external {
			externalTrees = append(externalTrees, externalTreeRecordDict(e))
		}
		o.Set("external_trees", externalTrees)
		branches := make([]any, 0, len(branchRecords))
		for _, b := range branchRecords {
			branches = append(branches, branchRecordDict(b))
		}
		o.Set("branches", branches)
		archiveDicts := make([]any, 0, len(archives))
		for _, a := range archives {
			archiveDicts = append(archiveDicts, archiveDict(a))
		}
		o.Set("archives", archiveDicts)
		return 0, worktreePrintJSON(w, o)
	}
	worktreePrintArchives(w, archives)
	if len(records) == 0 && len(external) == 0 {
		fmt.Fprintln(w, "No worktrees under .worktrees/ — nothing to reclaim.")
		return 0, nil
	}
	if len(records) > 0 {
		totalMB, reapableMB := 0, 0
		for _, r := range records {
			if r.SizeMB == nil {
				continue
			}
			totalMB += *r.SizeMB
			if strings.HasPrefix(r.Verdict, "reap") {
				reapableMB += *r.SizeMB
			}
		}
		fmt.Fprintf(w, "%-32s %6s %6s %-13s REASON\n", "TREE", "AGE", "SIZE", "VERDICT")
		for _, r := range sortTreeRecordsBySize(records) {
			fmt.Fprintf(w, "%-32s %5.1fd %6s %-13s %s\n", worktreeHead(r.Name, 32), r.AgeDays, worktreeFmtSize(r.SizeMB), r.Verdict, r.Reason)
		}
		fmt.Fprintf(w, "\n%d tree(s), %s total — %s reclaimable now via `icebath worktree prune`.\n", len(records), worktreeFmtSize(&totalMB), worktreeFmtSize(&reapableMB))
	}
	if len(external) > 0 {
		fmt.Fprintf(w, "\n%d externally-registered worktree(s) (never touched by prune):\n", len(external))
		stale := false
		for _, e := range external {
			state := "ok"
			if e.Missing {
				state = "MISSING"
			} else if e.Locked {
				state = "locked"
			}
			branch := e.Branch
			if branch == "" {
				branch = "?"
			}
			fmt.Fprintf(w, "  %s  [%s]  %s\n", e.Path, branch, state)
			if e.Missing && !e.Locked {
				stale = true
			}
		}
		if stale {
			fmt.Fprintln(w, "  Stale registrations (MISSING) are cleaned by `icebath worktree prune` (metadata only).")
		}
	}
	deletable := 0
	for _, b := range branchRecords {
		if b.Verdict == "delete" {
			deletable++
		}
	}
	if deletable > 0 {
		fmt.Fprintf(w, "%d local branch(es) fully merged/patch-equivalent upstream would also be deleted.\n", deletable)
	}
	return 0, nil
}

// Prints every archived worktree with its age, branch, reason and skipped file count
func worktreePrintArchives(w io.Writer, archives []Archive) {
	if len(archives) == 0 {
		return
	}
	fmt.Fprintf(w, "%d archived worktree(s) (restore with `icebath worktree restore <name>`):\n", len(archives))
	for _, a := range archives {
		age := 0.0
		if a.Created != 0 {
			age = (nowSeconds() - a.Created) / 86400
		}
		skipped := 0
		if a.Meta != nil {
			if v, _ := a.Meta.Get("skipped_untracked"); pyTruthy(v) {
				if list, ok := v.([]any); ok {
					skipped = len(list)
				}
			}
		}
		extra := ""
		if skipped > 0 {
			extra = fmt.Sprintf(", %d large untracked file(s) not captured", skipped)
		}
		fmt.Fprintf(w, "  %-40s %5.1fd  %s  (%s%s)\n", a.Name, age, worktreeMetaText(a.Meta, "branch", "detached"), worktreeMetaText(a.Meta, "reason", "?"), extra)
	}
	fmt.Fprintln(w)
}

// Table lines of a sweep plan ordered by action and name, closed by the per action totals
func worktreeSweepLines(plan []*Decision) []string {
	lines := []string{fmt.Sprintf("%-40s %7s  %-20s REASON", "TREE", "IDLE", "DECISION")}
	ordered := append([]*Decision{}, plan...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Action != ordered[j].Action {
			return ordered[i].Action < ordered[j].Action
		}
		return ordered[i].Name < ordered[j].Name
	})
	for _, d := range ordered {
		lines = append(lines, fmt.Sprintf("%-40s %6.1fh  %-20s %s", worktreeHead(d.Name, 40), d.AgeHours, d.Action, d.Reason))
	}
	counts := map[string]int{}
	for _, d := range plan {
		counts[d.Action]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	totals := strings.Join(parts, ", ")
	if totals == "" {
		totals = "nothing to do"
	}
	return append(lines, "Sweep totals: "+totals)
}

// Plan of the automatic sweep for a repo, applied unless dry run, with the action lines it produced
func worktreeSweep(ctx context.Context, repoRoot string, dryRun bool, olderThan *float64) ([]*Decision, []string, error) {
	settings := SweepSettings()
	plan := PlanSweep(ctx, repoRoot, 24, &settings, 0, olderThan)
	if dryRun || ClassicMode(settings) {
		return plan, []string{}, nil
	}
	_, _, archived := ExecutePlan(repoRoot, plan, settings)
	expired, err := PruneExpiredArchives(repoRoot, settings.ArchiveRetentionDays, 0, false)
	if err != nil {
		return nil, nil, err
	}
	saved := make([]string, 0, len(archived)+len(expired))
	for _, a := range archived {
		if a.Removed {
			saved = append(saved, i18n.T(ctx, "cli.worktree.sweep_archived_removed", "", map[string]any{"ref": a.Ref}))
			continue
		}
		why := a.WhyKept
		if why == "" {
			why = "?"
		}
		saved = append(saved, i18n.T(ctx, "cli.worktree.sweep_archived_kept", "", map[string]any{"ref": a.Ref, "reason": why}))
	}
	for _, r := range expired {
		saved = append(saved, "deleted expired archive "+r)
	}
	return plan, saved, nil
}

// Audit records minus the trees the sweep already decided and minus any tree a live process works in
func worktreeAttendedRecords(records []TreeRecord, plan []*Decision) []TreeRecord {
	if records == nil {
		records = []TreeRecord{}
	}
	if ClassicMode(SweepSettings()) {
		return records
	}
	owned := map[string]bool{}
	for _, d := range plan {
		if d.Action != "skip-young" {
			owned[d.Path] = true
		}
	}
	free := []TreeRecord{}
	paths := []string{}
	for _, r := range records {
		if !owned[r.Path] {
			free = append(free, r)
			paths = append(paths, r.Path)
		}
	}
	live := LiveProcessPids(paths, nil)
	out := []TreeRecord{}
	for _, r := range free {
		if len(live[r.Path]) == 0 {
			out = append(out, r)
		}
	}
	return out
}

// Runs the sweep then the attended reclaim of trees and branches, printing the result as text or JSON
func worktreeCmdPrune(ctx context.Context, repoRoot string, ns map[string]any, w io.Writer) (int, error) {
	dryRun := worktreeNsBool(ns, "dry_run")
	asJSON := worktreeNsBool(ns, "json")
	olderThan := worktreeNsFloat(ns, "older_than")
	actions := []string{}
	kept := []TreeRecord{}
	var plan []*Decision
	if !worktreeNsBool(ns, "branches_only") {
		var err error
		plan, actions, err = worktreeSweep(ctx, repoRoot, dryRun, olderThan)
		if err != nil {
			return 0, err
		}
		if !asJSON {
			fmt.Fprintln(w, strings.Join(worktreeSweepLines(plan), "\n")+"\n")
		}
		missing, err := PruneMissingRegistrations(repoRoot, dryRun)
		if err != nil {
			return 0, err
		}
		actions = append(actions, missing...)
		audited, err := AuditWorktrees(ctx, repoRoot, false, olderThan)
		if err != nil {
			return 0, err
		}
		treeRecords := worktreeAttendedRecords(audited, plan)
		reclaimed, err := ReclaimWorktrees(ctx, repoRoot, dryRun, treeRecords)
		if err != nil {
			return 0, err
		}
		actions = append(actions, reclaimed...)
		for _, r := range treeRecords {
			if r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use") {
				kept = append(kept, r)
			}
		}
		if len(kept) > 0 && !asJSON {
			fmt.Fprintf(w, "Preserved %d tree(s) with real work:\n", len(kept))
			for _, r := range kept {
				fmt.Fprintf(w, "  %s: %s\n", r.Name, r.Reason)
			}
		}
	}
	if !worktreeNsBool(ns, "trees_only") {
		branches, err := ReclaimBranches(repoRoot, dryRun, nil)
		if err != nil {
			return 0, err
		}
		actions = append(actions, branches...)
	}
	if asJSON {
		o := pyjson.NewObject()
		o.Set("repo", repoRoot)
		o.Set("dry_run", dryRun)
		actionList := make([]any, 0, len(actions))
		for _, a := range actions {
			actionList = append(actionList, a)
		}
		o.Set("actions", actionList)
		preserved := make([]any, 0, len(kept))
		for _, r := range kept {
			preserved = append(preserved, treeRecordDict(r))
		}
		o.Set("preserved", preserved)
		sweep := make([]any, 0, len(plan))
		for _, d := range plan {
			sweep = append(sweep, d.AsDict())
		}
		o.Set("sweep", sweep)
		return 0, worktreePrintJSON(w, o)
	}
	if len(actions) > 0 {
		fmt.Fprintln(w, "Attended reclaim (`icebath worktree prune` only; the automatic sweep is the table above):")
		for _, line := range actions {
			fmt.Fprintf(w, "  %s\n", line)
		}
		word := "done"
		if dryRun {
			word = "planned"
		}
		fmt.Fprintf(w, "%d action(s) %s.\n", len(actions), word)
	} else {
		fmt.Fprintln(w, "Nothing to reclaim — all trees/branches carry real work or are in use.")
	}
	return 0, nil
}

// Recreates an archived worktree and prints where it landed, 1 on a missing name or an archive error
func worktreeCmdRestore(ctx context.Context, repoRoot string, ns map[string]any, w io.Writer) (int, error) {
	name := worktreeNsString(ns, "name")
	if name == "" {
		fmt.Fprintln(w, "Usage: icebath worktree restore <name> [--path DIR]")
		return 1, nil
	}
	result, err := RestoreArchive(ctx, repoRoot, name, worktreeNsString(ns, "path"))
	if err != nil {
		var archiveErr *ArchiveError
		if errors.As(err, &archiveErr) {
			fmt.Fprintf(w, "✗ %s\n", archiveErr.Error())
			return 1, nil
		}
		return 0, err
	}
	fmt.Fprintf(w, "✓ Restored %s → %s\n", result.Ref, result.Path)
	if result.Detached {
		branch := "-"
		if pyTruthy(result.Branch) {
			branch = pyrepr.Str(result.Branch)
		}
		fmt.Fprintf(w, "  Detached at %s (branch %s moved or is checked out elsewhere); create a branch with `git switch -c <name>`.\n", worktreeHead(result.Head, 12), branch)
	} else {
		fmt.Fprintf(w, "  Branch: %s\n", pyrepr.Str(result.Branch))
	}
	if items, ok := result.SkippedUntracked.([]any); ok {
		for _, item := range items {
			o, ok := item.(*pyjson.Object)
			if !ok {
				continue
			}
			p, _ := o.Get("path")
			b, _ := o.Get("bytes")
			size, _ := pyint.Float(b)
			fmt.Fprintf(w, "  Not captured (over the size cap): %s (%d MB)\n", pyrepr.Str(p), int64(math.Floor(size/(1024*1024))))
		}
	}
	return 0, nil
}

// Dispatches list, prune or restore for the given or current repo, 1 on a bad repo, age or action
func cmdWorktree(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	repoRoot := worktreeNsString(ns, "repo")
	if repoRoot == "" {
		repoRoot = gitRepoRoot()
	}
	if repoRoot == "" {
		fmt.Fprintln(w, "Not inside a git repository (or pass --repo <path>).")
		return 1, nil
	}
	if olderThan := worktreeNsFloat(ns, "older_than"); olderThan != nil && *olderThan < 0 {
		fmt.Fprintln(w, "--older-than must be a non-negative number of days.")
		return 1, nil
	}
	action := worktreeNsString(ns, "worktree_action")
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return worktreeCmdList(ctx, repoRoot, ns, w)
	case "prune":
		return worktreeCmdPrune(ctx, repoRoot, ns, w)
	case "restore":
		return worktreeCmdRestore(ctx, repoRoot, ns, w)
	}
	fmt.Fprintf(w, "Unknown worktree action: %s\n", action)
	return 1, nil
}

// Normalizes the ls and audit aliases to list, then runs the worktree command
func WorktreeCommand(ctx context.Context, env *Env, ns map[string]any, w io.Writer) (int, error) {
	if action, _ := ns["worktree_action"].(string); action == "ls" || action == "audit" {
		ns["worktree_action"] = "list"
	}
	return worktreeCmdFn(ctx, ns, w)
}
