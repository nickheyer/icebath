package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Store status, prune and console input of the checkpoints command, the module attributes hermes tests patch
var (
	checkpointsStoreStatusFn = CheckpointStoreStatus
	checkpointsPruneFn       = PruneCheckpoints
	checkpointsInputFn       = func(prompt string) (string, error) { return pyrich.NewConsole(os.Stdout).Input(prompt) }
)

// Age of a unix timestamp as seconds, minutes, hours or days ago, a dash for a non numeric value
func checkpointsFmtAge(ts any) string {
	if ts == nil {
		return "\u2014"
	}
	stamp, err := pyint.Float(ts)
	if err != nil {
		return "\u2014"
	}
	age := float64(time.Now().UnixNano())/1e9 - stamp
	if age < 0 {
		return "now"
	}
	for _, step := range []struct {
		bound, div float64
		unit       string
	}{{60, 1, "s"}, {3600, 60, "m"}, {86400, 3600, "h"}} {
		if age < step.bound {
			return fmt.Sprintf("%d%s ago", int(age/step.div), step.unit)
		}
	}
	return fmt.Sprintf("%dd ago", int(age/86400))
}

// Unix timestamp of a project's last touch, zero for an absent or non numeric value
func checkpointsLastTouch(v any) float64 {
	if v == nil {
		return 0
	}
	f, err := pyint.Float(v)
	if err != nil {
		return 0
	}
	return f
}

// Integer namespace value with a default for an absent, nil or non integer value
func checkpointsNsInt(ns map[string]any, key string, def int) int {
	v, ok := ns[key]
	if !ok || v == nil {
		return def
	}
	n, isInt := asIntValue(v)
	if !isInt {
		return def
	}
	return n
}

// Prints the store sizes, every project by last touch and the legacy archives
func checkpointsCmdStatus(env *Env, ns map[string]any, w io.Writer) (int, error) {
	info, err := checkpointsStoreStatusFn(env, "")
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "Checkpoint base: %s\n", info.Base)
	fmt.Fprintf(w, "Total size:      %s\n", sizefmtBytes(info.TotalSizeBytes))
	fmt.Fprintf(w, "  store/         %s\n", sizefmtBytes(info.StoreSizeBytes))
	fmt.Fprintf(w, "  legacy-*       %s\n", sizefmtBytes(info.LegacySizeBytes))
	fmt.Fprintf(w, "Projects:        %d\n", info.ProjectCount)
	projects := append([]CheckpointProjectStatus{}, info.Projects...)
	sort.SliceStable(projects, func(i, j int) bool {
		return checkpointsLastTouch(projects[i].LastTouch) > checkpointsLastTouch(projects[j].LastTouch)
	})
	if len(projects) > 0 {
		limit := checkpointsNsInt(ns, "limit", 0)
		if limit == 0 {
			limit = 20
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s  %s  %s  STATE\n", pystr.Ljust("WORKDIR", 60), pystr.Rjust("COMMITS", 7), pystr.Rjust("LAST TOUCH", 12))
		for _, p := range projects[:min(limit, len(projects))] {
			wd := p.Workdir
			if wd == "" {
				wd = "(unknown)"
			}
			if n := pystr.Len(wd); n > 60 {
				wd = "\u2026" + pystr.Slice(wd, n-59, n)
			}
			state := "orphan"
			if p.Exists {
				state = "live"
			}
			fmt.Fprintf(w, "  %s  %s  %s  %s\n", pystr.Ljust(wd, 60), pystr.Rjust(fmt.Sprint(p.Commits), 7), pystr.Rjust(checkpointsFmtAge(p.LastTouch), 12), state)
		}
	}
	if len(info.LegacyArchives) > 0 {
		legacy := append([]CheckpointLegacyArchive{}, info.LegacyArchives...)
		sort.SliceStable(legacy, func(i, j int) bool { return legacy[i].Mtime > legacy[j].Mtime })
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Legacy archives (%d):\n", len(legacy))
		checkpointsPrintArchives(w, legacy)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Clear with: icebath checkpoints clear-legacy")
	}
	return 0, nil
}

// One line per archive with its name and size
func checkpointsPrintArchives(w io.Writer, archives []CheckpointLegacyArchive) {
	for _, arch := range archives {
		fmt.Fprintf(w, "  %s  %s\n", pystr.Ljust(arch.Name, 40), pystr.Rjust(sizefmtBytes(arch.SizeBytes), 10))
	}
}

// Prunes orphan and stale projects after confirming exactly the orphans previewed
func checkpointsCmdPrune(env *Env, ns map[string]any, w io.Writer) (int, error) {
	deleteOrphans := !pyTruthy(ns["keep_orphans"])
	force := pyTruthy(ns["force"])
	retentionDays := checkpointsNsInt(ns, "retention_days", 7)
	maxSizeMB := checkpointsNsInt(ns, "max_size_mb", 500)
	var orphanAllowlist map[string]bool
	if deleteOrphans && !force {
		info, err := checkpointsStoreStatusFn(env, "")
		if err != nil {
			return 1, err
		}
		var orphans []CheckpointProjectStatus
		for _, p := range info.Projects {
			if !p.Exists {
				orphans = append(orphans, p)
			}
		}
		var preV2Orphans []CheckpointPreV2Status
		for _, p := range info.PreV2Projects {
			if !p.Exists {
				preV2Orphans = append(preV2Orphans, p)
			}
		}
		if len(orphans) > 0 || len(preV2Orphans) > 0 {
			fmt.Fprintf(w, "This will permanently delete %d orphan checkpoint project(s) whose workdir is not currently reachable:\n", len(orphans)+len(preV2Orphans))
			fmt.Fprintln(w)
			for _, p := range orphans {
				wd := p.Workdir
				if wd == "" {
					wd = "(unknown)"
				}
				fmt.Fprintf(w, "  %s  (%d commit(s))\n", wd, p.Commits)
			}
			for _, p := range preV2Orphans {
				wd := "(unknown)"
				if p.Workdir != nil && *p.Workdir != "" {
					wd = *p.Workdir
				}
				fmt.Fprintf(w, "  %s  (pre-v2 shadow repo)\n", wd)
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "A workdir can be unreachable because the project was deleted,")
			fmt.Fprintln(w, "or because an external volume / network share / VPN is down.")
			fmt.Fprintln(w, "Pass --keep-orphans to prune stale entries only.")
			if !checkpointsConfirm(w, "Delete these orphan projects?") {
				fmt.Fprintln(w, "Aborted.")
				return 1, nil
			}
		}
		orphanAllowlist = map[string]bool{}
		for _, p := range orphans {
			orphanAllowlist[p.Hash] = true
		}
		for _, p := range preV2Orphans {
			orphanAllowlist[p.Path] = true
		}
	}
	fmt.Fprintln(w, "Pruning checkpoint store\u2026")
	fmt.Fprintf(w, "  retention_days:    %d\n", retentionDays)
	fmt.Fprintf(w, "  delete_orphans:    %s\n", pyBoolText(deleteOrphans))
	fmt.Fprintf(w, "  max_total_size_mb: %d\n", maxSizeMB)
	fmt.Fprintln(w)
	result, err := checkpointsPruneFn(env, CheckpointPruneOptions{RetentionDays: retentionDays, DeleteOrphans: deleteOrphans,
		MaxTotalSizeMB: maxSizeMB, OrphanAllowlist: orphanAllowlist})
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "Scanned:         %d\n", result.Scanned)
	fmt.Fprintf(w, "Deleted orphan:  %d\n", result.DeletedOrphan)
	fmt.Fprintf(w, "Deleted stale:   %d\n", result.DeletedStale)
	fmt.Fprintf(w, "Errors:          %d\n", result.Errors)
	fmt.Fprintf(w, "Bytes reclaimed: %s\n", sizefmtBytes(result.BytesFreed))
	return 0, nil
}

// Python repr of a bool
func pyBoolText(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

// Asks a yes or no question on the console, false on end of input or interrupt
func checkpointsConfirm(w io.Writer, prompt string) bool {
	answer, err := checkpointsInputFn(prompt + " [y/N]: ")
	if err != nil {
		fmt.Fprintln(w)
		return false
	}
	answer = pystr.Lower(pystr.Strip(answer))
	return answer == "y" || answer == "yes"
}

// The force flag or an interactive yes, printing Aborted otherwise
func checkpointsConfirmed(ns map[string]any, w io.Writer, prompt string) bool {
	if pyTruthy(ns["force"]) || checkpointsConfirm(w, prompt) {
		return true
	}
	fmt.Fprintln(w, "Aborted.")
	return false
}

// Deletes the whole checkpoint base after confirmation
func checkpointsCmdClear(env *Env, ns map[string]any, w io.Writer) (int, error) {
	info, err := checkpointsStoreStatusFn(env, "")
	if err != nil {
		return 1, err
	}
	if info.TotalSizeBytes == 0 && !pathExistsFollow(CheckpointBase(env)) {
		fmt.Fprintln(w, "Nothing to clear \u2014 checkpoint base does not exist.")
		return 0, nil
	}
	fmt.Fprintf(w, "This will delete the ENTIRE checkpoint base at %s\n", info.Base)
	fmt.Fprintf(w, "  size:        %s\n", sizefmtBytes(info.TotalSizeBytes))
	fmt.Fprintf(w, "  projects:    %d\n", info.ProjectCount)
	fmt.Fprintf(w, "  legacy dirs: %d\n", len(info.LegacyArchives))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "All /rollback history for every working directory will be lost.")
	if !checkpointsConfirmed(ns, w, "Proceed?") {
		return 1, nil
	}
	result := ClearAllCheckpoints(env, "")
	if result.Deleted {
		fmt.Fprintf(w, "Cleared. Reclaimed %s.\n", sizefmtBytes(result.BytesFreed))
		return 0, nil
	}
	fmt.Fprintln(w, "Could not clear checkpoint base (see logs).")
	return 2, nil
}

// Deletes the legacy archives after confirmation and reports failures
func checkpointsCmdClearLegacy(env *Env, ns map[string]any, w io.Writer) (int, error) {
	info, err := checkpointsStoreStatusFn(env, "")
	if err != nil {
		return 1, err
	}
	legacy := info.LegacyArchives
	if len(legacy) == 0 {
		fmt.Fprintln(w, "No legacy archives to clear.")
		return 0, nil
	}
	var total int64
	for _, a := range legacy {
		total += a.SizeBytes
	}
	fmt.Fprintf(w, "Found %d legacy archive(s), total %s:\n", len(legacy), sizefmtBytes(total))
	checkpointsPrintArchives(w, legacy)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Legacy archives hold pre-v2 per-project shadow repos, moved aside")
	fmt.Fprintln(w, "during the single-store migration. Delete when you're confident")
	fmt.Fprintln(w, "you don't need the old /rollback history.")
	if !checkpointsConfirmed(ns, w, "Delete all legacy archives?") {
		return 1, nil
	}
	result := ClearLegacyCheckpoints(env, "")
	fmt.Fprintf(w, "Deleted %d archive(s), reclaimed %s.\n", result.Deleted, sizefmtBytes(result.BytesFreed))
	if result.Errors > 0 {
		fmt.Fprintf(w, "Could not delete %d archive(s) (see logs).\n", result.Errors)
		return 2, nil
	}
	return 0, nil
}

// CheckpointsCommand runs the icebath checkpoints subcommand named by checkpoints_command, status when bare
func CheckpointsCommand(ctx context.Context, env *Env, ns map[string]any, w io.Writer) (int, error) {
	switch strings.TrimSpace(pyStrOrEmpty(ns["checkpoints_command"])) {
	case "", "status", "list":
		return checkpointsCmdStatus(env, ns, w)
	case "prune":
		return checkpointsCmdPrune(env, ns, w)
	case "clear":
		return checkpointsCmdClear(env, ns, w)
	case "clear-legacy":
		return checkpointsCmdClearLegacy(env, ns, w)
	}
	return 2, fmt.Errorf("unknown checkpoints command %q", ns["checkpoints_command"])
}
