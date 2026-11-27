package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	worktreesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/worktrees/v1/worktreesv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Failure of a command with the exit status it ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Flags every worktree command takes
type common struct {
	repo  string
	json  bool
	older float64
}

func (c *common) flags(cmd *cobra.Command, older string) {
	cmd.Flags().StringVar(&c.repo, "repo", "", "Repo root (default: current repo)")
	cmd.Flags().BoolVar(&c.json, "json", false, "Machine-readable output")
	cmd.Flags().Float64Var(&c.older, "older-than", 0, older)
}

// Folder whose repository the command works on
func (c *common) directory() string {
	if c.repo != "" {
		return c.repo
	}
	dir, _ := os.Getwd()
	return dir
}

// Days young trees are held back by, none unless the flag was given
func (c *common) olderThan(cmd *cobra.Command) *float64 {
	if !cmd.Flags().Changed("older-than") {
		return nil
	}
	return &c.older
}

// Prints a refusal of the service and ends with status 1
func refused(cmd *cobra.Command, err error) error {
	if failure, ok := err.(*connect.Error); ok && (failure.Code() == connect.CodeFailedPrecondition || failure.Code() == connect.CodeInvalidArgument) {
		fmt.Fprintln(cmd.OutOrStdout(), failure.Message())
		return exit(1)
	}
	return err
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := worktreesv1connect.NewWorktreeServiceClient(conn)
	var root common
	command := &cobra.Command{
		Use:   "worktree",
		Short: "Audit and reclaim accumulated git worktrees and merged branches",
		Long: "Audit, reclaim and restore the .worktrees/ trees icebath sessions accumulate. Idle trees with uncommitted changes or unpushed " +
			"commits are archived to refs/icebath/archive/<tree> before removal (Archive dirty trees in /settings under Tools › Worktrees); " +
			"`restore` brings one back. In-use trees are never touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return list(cmd, service, &root) },
	}
	root.flags(command, "Treat reapable trees younger than DAYS as keep")
	var listing common
	listCommand := &cobra.Command{Use: "list", Aliases: []string{"ls", "audit"}, Short: "Classify every tree: age, size, verdict, reason (default action)",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return list(cmd, service, &listing) }}
	listing.flags(listCommand, "Treat reapable trees younger than DAYS as keep")
	command.AddCommand(listCommand, prune(service), restore(service))
	return []*cobra.Command{command}
}

// Size as the tables show it, gigabytes from 1024 MB up, a question mark when not measured
func size(mb *int32) string {
	switch {
	case mb == nil:
		return "?"
	case *mb >= 1024:
		return fmt.Sprintf("%.1fG", float64(*mb)/1024)
	}
	return fmt.Sprintf("%dM", *mb)
}

func printJSON(out io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

// One tree of an audit as the JSON output has it
type treeJSON struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Branch    string   `json:"branch"`
	AgeDays   float64  `json:"age_days"`
	SizeMB    *int32   `json:"size_mb"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	Untracked []string `json:"untracked"`
}

func treesJSON(records []*worktreesv1.TreeRecord) []treeJSON {
	out := []treeJSON{}
	for _, r := range records {
		out = append(out, treeJSON{r.GetName(), r.GetPath(), r.GetBranch(), r.GetAgeDays(), r.SizeMb, r.GetVerdict(), r.GetReason(),
			append([]string{}, r.GetUntracked()...)})
	}
	return out
}

// Prints the audit of the trees under .worktrees, the trees registered elsewhere, the branches and the archives, as a table or JSON
func list(cmd *cobra.Command, service worktreesv1connect.WorktreeServiceClient, c *common) error {
	out := cmd.OutOrStdout()
	audit, err := service.Audit(cmd.Context(), &worktreesv1.AuditRequest{Directory: c.directory(), OlderThan: c.olderThan(cmd)})
	if err != nil {
		return refused(cmd, err)
	}
	if c.json {
		type external struct {
			Path    string `json:"path"`
			Branch  string `json:"branch"`
			Locked  bool   `json:"locked"`
			Missing bool   `json:"missing"`
		}
		type branch struct {
			Name    string `json:"name"`
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		}
		type archive struct {
			Ref     string         `json:"ref"`
			Name    string         `json:"name"`
			Sha     string         `json:"sha"`
			Created float64        `json:"created"`
			Meta    map[string]any `json:"meta"`
		}
		document := struct {
			Repo     string     `json:"repo"`
			Trees    []treeJSON `json:"trees"`
			External []external `json:"external_trees"`
			Branches []branch   `json:"branches"`
			Archives []archive  `json:"archives"`
		}{Repo: audit.GetRepo(), Trees: treesJSON(audit.GetTrees()), External: []external{}, Branches: []branch{}, Archives: []archive{}}
		for _, e := range audit.GetExternal() {
			document.External = append(document.External, external{e.GetPath(), e.GetBranch(), e.GetLocked(), e.GetMissing()})
		}
		for _, b := range audit.GetBranches() {
			document.Branches = append(document.Branches, branch{b.GetName(), b.GetVerdict(), b.GetReason()})
		}
		for _, a := range audit.GetArchives() {
			skipped := []map[string]any{}
			for _, s := range a.GetSkipped() {
				skipped = append(skipped, map[string]any{"path": s.GetPath(), "bytes": s.GetBytes()})
			}
			created := 0.0
			if a.GetCreated() != nil {
				created = float64(a.GetCreated().AsTime().Unix())
			}
			document.Archives = append(document.Archives, archive{a.GetRef(), a.GetName(), a.GetSha(), created,
				map[string]any{"branch": a.GetBranch(), "reason": a.GetReason(), "path": a.GetPath(), "skipped_untracked": skipped}})
		}
		return printJSON(out, document)
	}
	if archives := audit.GetArchives(); len(archives) > 0 {
		fmt.Fprintf(out, "%d archived worktree(s) (restore with `icebath worktree restore <name>`):\n", len(archives))
		for _, a := range archives {
			age := 0.0
			if a.GetCreated() != nil {
				age = time.Since(a.GetCreated().AsTime()).Hours() / 24
			}
			extra := ""
			if n := len(a.GetSkipped()); n > 0 {
				extra = fmt.Sprintf(", %d large untracked file(s) not captured", n)
			}
			fmt.Fprintf(out, "  %-40s %5.1fd  %s  (%s%s)\n", a.GetName(), age, cmp.Or(a.GetBranch(), "detached"), cmp.Or(a.GetReason(), "?"), extra)
		}
		fmt.Fprintln(out)
	}
	trees, external := audit.GetTrees(), audit.GetExternal()
	if len(trees) == 0 && len(external) == 0 {
		fmt.Fprintln(out, "No worktrees under .worktrees/ — nothing to reclaim.")
		return nil
	}
	if len(trees) > 0 {
		var total, reapable int32
		for _, r := range trees {
			if r.SizeMb != nil {
				total += *r.SizeMb
				if strings.HasPrefix(r.GetVerdict(), "reap") {
					reapable += *r.SizeMb
				}
			}
		}
		fmt.Fprintf(out, "%-32s %6s %6s %-13s REASON\n", "TREE", "AGE", "SIZE", "VERDICT")
		sorted := slices.Clone(trees)
		slices.SortStableFunc(sorted, func(a, b *worktreesv1.TreeRecord) int { return int(b.GetSizeMb()) - int(a.GetSizeMb()) })
		for _, r := range sorted {
			fmt.Fprintf(out, "%-32s %5.1fd %6s %-13s %s\n", strs.Head(r.GetName(), 32), r.GetAgeDays(), size(r.SizeMb), r.GetVerdict(), r.GetReason())
		}
		fmt.Fprintf(out, "\n%d tree(s), %s total — %s reclaimable now via `icebath worktree prune`.\n", len(trees), size(&total), size(&reapable))
	}
	if len(external) > 0 {
		fmt.Fprintf(out, "\n%d externally-registered worktree(s) (never touched by prune):\n", len(external))
		stale := false
		for _, e := range external {
			state := "ok"
			if e.GetMissing() {
				state = "MISSING"
			} else if e.GetLocked() {
				state = "locked"
			}
			fmt.Fprintf(out, "  %s  [%s]  %s\n", e.GetPath(), cmp.Or(e.GetBranch(), "?"), state)
			stale = stale || e.GetMissing() && !e.GetLocked()
		}
		if stale {
			fmt.Fprintln(out, "  Stale registrations (MISSING) are cleaned by `icebath worktree prune` (metadata only).")
		}
	}
	deletable := 0
	for _, b := range audit.GetBranches() {
		if b.GetVerdict() == "delete" {
			deletable++
		}
	}
	if deletable > 0 {
		fmt.Fprintf(out, "%d local branch(es) fully merged/patch-equivalent upstream would also be deleted.\n", deletable)
	}
	return nil
}

// Table of a sweep's plan by action and name, closed by the count of each action
func sweepLines(plan []*worktreesv1.Decision) []string {
	lines := []string{fmt.Sprintf("%-40s %7s  %-20s REASON", "TREE", "IDLE", "DECISION")}
	ordered := slices.Clone(plan)
	slices.SortStableFunc(ordered, func(a, b *worktreesv1.Decision) int {
		if a.GetAction() != b.GetAction() {
			return strings.Compare(a.GetAction(), b.GetAction())
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	counts := map[string]int{}
	for _, d := range ordered {
		lines = append(lines, fmt.Sprintf("%-40s %6.1fh  %-20s %s", strs.Head(d.GetName(), 40), d.GetAgeHours(), d.GetAction(), d.GetReason()))
		counts[d.GetAction()]++
	}
	var parts []string
	for _, action := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", action, counts[action]))
	}
	return append(lines, "Sweep totals: "+cmp.Or(strings.Join(parts, ", "), "nothing to do"))
}

func prune(service worktreesv1connect.WorktreeServiceClient) *cobra.Command {
	var c common
	var dry, treesOnly, branchesOnly bool
	command := &cobra.Command{
		Use:   "prune",
		Short: "Remove safe trees and delete fully-merged local branches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			response, err := service.Prune(cmd.Context(), &worktreesv1.PruneRequest{Directory: c.directory(), DryRun: dry, OlderThan: c.olderThan(cmd),
				TreesOnly: treesOnly, BranchesOnly: branchesOnly, Sweep: true})
			if err != nil {
				return refused(cmd, err)
			}
			if c.json {
				type decision struct {
					Name      string  `json:"name"`
					Path      string  `json:"path"`
					Action    string  `json:"action"`
					Reason    string  `json:"reason"`
					AgeHours  float64 `json:"age_hours"`
					Mtime     float64 `json:"mtime"`
					Force     bool    `json:"force"`
					Verdict   string  `json:"verdict"`
					LockState *string `json:"lock_state"`
					SizeMB    *int32  `json:"size_mb"`
					TierHours float64 `json:"tier_hours"`
					PlannedAt float64 `json:"planned_at"`
				}
				sweep := []decision{}
				for _, d := range response.GetSweep() {
					var lock *string
					if d.GetLockState() != "" {
						state := d.GetLockState()
						lock = &state
					}
					sweep = append(sweep, decision{d.GetName(), d.GetPath(), d.GetAction(), d.GetReason(), d.GetAgeHours(), d.GetMtime(), d.GetForce(),
						d.GetVerdict(), lock, d.SizeMb, d.GetTierHours(), d.GetPlannedAt()})
				}
				return printJSON(out, struct {
					Repo      string     `json:"repo"`
					DryRun    bool       `json:"dry_run"`
					Actions   []string   `json:"actions"`
					Preserved []treeJSON `json:"preserved"`
					Sweep     []decision `json:"sweep"`
				}{response.GetRepo(), dry, append([]string{}, response.GetActions()...), treesJSON(response.GetPreserved()), sweep})
			}
			if !branchesOnly {
				fmt.Fprintln(out, strings.Join(sweepLines(response.GetSweep()), "\n")+"\n")
				if preserved := response.GetPreserved(); len(preserved) > 0 {
					fmt.Fprintf(out, "Preserved %d tree(s) with real work:\n", len(preserved))
					for _, r := range preserved {
						fmt.Fprintf(out, "  %s: %s\n", r.GetName(), r.GetReason())
					}
				}
			}
			actions := response.GetActions()
			if len(actions) == 0 {
				fmt.Fprintln(out, "Nothing to reclaim — all trees/branches carry real work or are in use.")
				return nil
			}
			fmt.Fprintln(out, "Attended reclaim (`icebath worktree prune` only; the automatic sweep is the table above):")
			for _, line := range actions {
				fmt.Fprintf(out, "  %s\n", line)
			}
			word := "done"
			if dry {
				word = "planned"
			}
			fmt.Fprintf(out, "%d action(s) %s.\n", len(actions), word)
			return nil
		},
	}
	c.flags(command, "Only reap trees idle for at least DAYS days (safety gates still apply)")
	command.Flags().BoolVar(&dry, "dry-run", false, "Show the plan without changing anything")
	command.Flags().BoolVar(&treesOnly, "trees-only", false, "Only remove worktrees; leave local branches alone")
	command.Flags().BoolVar(&branchesOnly, "branches-only", false, "Only delete merged local branches; leave worktrees alone")
	return command
}

func restore(service worktreesv1connect.WorktreeServiceClient) *cobra.Command {
	var repo, path string
	command := &cobra.Command{
		Use:   "restore <name>",
		Short: "Recreate an archived worktree (tracked, staged and untracked files)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				fmt.Fprintln(out, "Usage: icebath worktree restore <name> [--path DIR]")
				return exit(1)
			}
			dir := repo
			if dir == "" {
				dir, _ = os.Getwd()
			}
			restored, err := service.Restore(cmd.Context(), &worktreesv1.RestoreRequest{Directory: dir, Name: args[0], Path: path})
			if err != nil {
				return refused(cmd, err)
			}
			if restored.GetError() != "" {
				fmt.Fprintf(out, "✗ %s\n", restored.GetError())
				return exit(1)
			}
			fmt.Fprintf(out, "✓ Restored %s → %s\n", restored.GetRef(), restored.GetPath())
			if restored.GetDetached() {
				fmt.Fprintf(out, "  Detached at %s (branch %s moved or is checked out elsewhere); create a branch with `git switch -c <name>`.\n",
					strs.Head(restored.GetHead(), 12), cmp.Or(restored.GetBranch(), "-"))
			} else {
				fmt.Fprintf(out, "  Branch: %s\n", restored.GetBranch())
			}
			for _, s := range restored.GetSkipped() {
				fmt.Fprintf(out, "  Not captured (over the size cap): %s (%d MB)\n", s.GetPath(), s.GetBytes()/(1024*1024))
			}
			return nil
		},
	}
	command.Flags().StringVar(&path, "path", "", "Restore here instead of the original path")
	command.Flags().StringVar(&repo, "repo", "", "Repo root (default: current repo)")
	return command
}
