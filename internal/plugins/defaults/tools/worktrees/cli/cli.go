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
	cmd.Flags().StringVar(&c.repo, "repo", "", "Repository folder, instead of the one you are in")
	cmd.Flags().BoolVar(&c.json, "json", false, "Print the result as JSON")
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
		cli.NewConsole(cmd).Failure(failure.Message())
		return exit(1)
	}
	return err
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := worktreesv1connect.NewWorktreeServiceClient(conn)
	var root common
	command := &cobra.Command{
		Use:     "worktree",
		GroupID: cli.GroupSessions,
		Short:   "Clean up the git worktrees that sessions leave behind",
		Long: "A session started with -w works in a git worktree of its own, under .worktrees/ in the repository. Use these commands to list " +
			"those worktrees, remove the ones that are safe to remove and bring back an archived one. With no subcommand, worktree lists " +
			"them.\n\n" +
			"While the setting Archive unsaved work is on, a worktree with changes that are not committed or pushed " +
			"is archived to refs/icebath/archive/<tree> before it is removed. Worktrees in use are never touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return list(cmd, service, &root) },
	}
	root.flags(command, "Keep worktrees younger than this many days, even when removable")
	var listing common
	listCommand := &cobra.Command{Use: "list", Aliases: []string{"ls", "audit"}, Short: "List every worktree and whether it is safe to remove",
		Long: "Lists each worktree with its age and size, whether it is safe to remove and why. This is what worktree does with no " +
			"subcommand.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return list(cmd, service, &listing) }}
	listing.flags(listCommand, "Keep worktrees younger than this many days, even when removable")
	command.AddCommand(listCommand, prune(service), restore(service))
	return []*cobra.Command{command}
}

// Size as the tables show it, in the largest unit that fits; Not measured for none
func size(mb *int32) string {
	if mb == nil {
		return "Not measured"
	}
	return strs.Size(float64(*mb) * 1024 * 1024)
}

// Length of time in words in the largest whole unit it has passed, such as 3 days
func span(length time.Duration) string {
	unit := 24 * time.Hour
	switch {
	case length < time.Minute:
		return "Under a minute"
	case length < time.Hour:
		unit = time.Minute
	case length < 24*time.Hour:
		unit = time.Hour
	}
	return strs.Span(length.Truncate(unit))
}

// Days as a length of time in words
func days(n float64) string { return span(time.Duration(n * float64(24*time.Hour))) }

// What a verdict or a decision of the worktrees says, in words
var said = map[string]string{
	"reap": "Remove", "reap-keep-branch": "Remove but keep branch", "reap-archive": "Archive and remove", "keep": "Keep", "delete": "Delete",
	"remove": "Remove", "remove-keep-branch": "Remove but keep branch", "archive-remove": "Archive and remove", "skip-young": "Skip as too new",
	"skip-live-lock": "Skip as locked", "skip-live-process": "Skip as in use", "skip-kanban": "Skip as a kanban tree",
}

// Words of a verdict or a decision, one not known said with its dashes as spaces
func words(code string) string {
	if text, ok := said[code]; ok {
		return text
	}
	text := strings.ReplaceAll(code, "-", " ")
	if text == "" {
		return "Unknown"
	}
	return strs.Capital(text)
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
	con := cli.NewConsole(cmd)
	if archives := audit.GetArchives(); len(archives) > 0 {
		con.Say(strs.Count(len(archives), "archived worktree") + ". Bring one back with icebath worktree restore <name>.")
		var rows [][]string
		for _, a := range archives {
			age := "Unknown"
			if a.GetCreated() != nil {
				age = span(time.Since(a.GetCreated().AsTime()))
			}
			why := cmp.Or(a.GetReason(), "Unknown")
			if n := len(a.GetSkipped()); n > 0 {
				why += ". " + strs.Count(n, "large untracked file") + " not saved"
			}
			rows = append(rows, []string{a.GetName(), age, cmp.Or(a.GetBranch(), "No branch"), why})
		}
		con.Table([]string{"Archive", "Age", "Branch", "Why it was archived"}, rows)
		con.Blank()
	}
	trees, external := audit.GetTrees(), audit.GetExternal()
	if len(trees) == 0 && len(external) == 0 {
		con.Say("This repository has no worktrees under .worktrees, so there is nothing to clean up. A session started with -w makes one.")
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
		sorted := slices.Clone(trees)
		slices.SortStableFunc(sorted, func(a, b *worktreesv1.TreeRecord) int { return int(b.GetSizeMb()) - int(a.GetSizeMb()) })
		var rows [][]string
		for _, r := range sorted {
			rows = append(rows, []string{strs.Head(r.GetName(), 32), days(r.GetAgeDays()), size(r.SizeMb), words(r.GetVerdict()), r.GetReason()})
		}
		con.Table([]string{"Worktree", "Age", "Size", "Verdict", "Why"}, rows)
		con.Blank()
		con.Say(strs.Count(len(trees), "worktree") + " take " + size(&total) + ". Running icebath worktree prune frees " + size(&reapable) + " now.")
	}
	if len(external) > 0 {
		con.Blank()
		con.Say(strs.Count(len(external), "worktree") + " outside .worktrees, which prune never touches:")
		stale := false
		var rows [][]string
		for _, e := range external {
			state := "Present"
			if e.GetMissing() {
				state = "Missing"
			} else if e.GetLocked() {
				state = "Locked"
			}
			rows = append(rows, []string{e.GetPath(), cmp.Or(e.GetBranch(), "No branch"), state})
			stale = stale || e.GetMissing() && !e.GetLocked()
		}
		con.Table([]string{"Folder", "Branch", "State"}, rows)
		if stale {
			con.Say("  Running icebath worktree prune forgets the missing ones. It deletes no files.")
		}
	}
	deletable := 0
	for _, b := range audit.GetBranches() {
		if b.GetVerdict() == "delete" {
			deletable++
		}
	}
	if deletable > 0 {
		con.Say(strs.Count(deletable, "local branch") + " already merged upstream would be deleted too.")
	}
	return nil
}

// Prints a sweep's plan by action and name as a table, closed by the count of each action
func sweep(con *cli.Console, plan []*worktreesv1.Decision) {
	ordered := slices.Clone(plan)
	slices.SortStableFunc(ordered, func(a, b *worktreesv1.Decision) int {
		if a.GetAction() != b.GetAction() {
			return strings.Compare(a.GetAction(), b.GetAction())
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	counts := map[string]int{}
	var rows [][]string
	for _, d := range ordered {
		rows = append(rows, []string{strs.Head(d.GetName(), 40), span(time.Duration(d.GetAgeHours() * float64(time.Hour))), words(d.GetAction()), d.GetReason()})
		counts[d.GetAction()]++
	}
	if len(rows) > 0 {
		con.Table([]string{"Worktree", "Unused for", "Decision", "Why"}, rows)
	}
	var parts []string
	for _, action := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, words(action)+" "+strs.Number(float64(counts[action])))
	}
	if len(parts) == 0 {
		con.Say("The automatic cleanup has nothing to do.")
		return
	}
	con.Say("The automatic cleanup in all: " + strings.Join(parts, " · "))
}

func prune(service worktreesv1connect.WorktreeServiceClient) *cobra.Command {
	var c common
	var dry, treesOnly, branchesOnly bool
	command := &cobra.Command{
		Use:   "prune",
		Short: "Remove safe worktrees and merged local branches",
		Long: "Removes the worktrees that are safe to remove and deletes the local branches that are fully merged. --dry-run shows the " +
			"plan without changing anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, con := cmd.OutOrStdout(), cli.NewConsole(cmd)
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
				sweep(con, response.GetSweep())
				con.Blank()
				if preserved := response.GetPreserved(); len(preserved) > 0 {
					con.Say("Kept " + strs.Count(len(preserved), "worktree") + " that hold work:")
					for _, r := range preserved {
						con.Say("  " + r.GetName() + ": " + r.GetReason())
					}
				}
			}
			actions := response.GetActions()
			if len(actions) == 0 {
				con.Say("Nothing to clean up. Every worktree and branch holds work or is in use.")
				return nil
			}
			if dry {
				con.Say("What this command would clean up, besides the automatic cleanup above:")
			} else {
				con.Say("What this command cleaned up, besides the automatic cleanup above:")
			}
			for _, line := range actions {
				con.Say("  " + line)
			}
			if dry {
				con.Say(strs.Count(len(actions), "step") + " planned. Nothing was changed, because of --dry-run.")
			} else {
				con.Success(strs.Count(len(actions), "step") + " done.")
			}
			return nil
		},
	}
	c.flags(command, "Only remove worktrees unused for at least this many days")
	command.Flags().BoolVar(&dry, "dry-run", false, "Show the plan without changing anything")
	command.Flags().BoolVar(&treesOnly, "trees-only", false, "Remove only worktrees and leave local branches alone")
	command.Flags().BoolVar(&branchesOnly, "branches-only", false, "Delete only merged local branches and leave worktrees alone")
	return command
}

func restore(service worktreesv1connect.WorktreeServiceClient) *cobra.Command {
	var repo, path string
	command := &cobra.Command{
		Use:     "restore <name>",
		Short:   "Bring back an archived worktree",
		Long:    "Recreates an archived worktree with its tracked, staged and untracked files, at its old path or at --path.",
		Example: "  icebath worktree restore fix-login --path ../fix-login",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			con := cli.NewConsole(cmd)
			if len(args) == 0 {
				con.Say("Usage: icebath worktree restore <name>")
				con.Say("Name the archived worktree to bring back. Run icebath worktree to see them.")
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
				con.Failure(restored.GetError())
				return exit(1)
			}
			con.Success("Restored the archive " + restored.GetRef() + " to " + restored.GetPath() + ".")
			switch {
			case restored.GetDetached() && restored.GetBranch() != "":
				con.Say("  It is on no branch, at commit " + strs.Head(restored.GetHead(), 12) + ", because the branch " + restored.GetBranch() +
					" moved or is checked out elsewhere. Make a branch with git switch -c <name>.")
			case restored.GetDetached():
				con.Say("  It is on no branch, at commit " + strs.Head(restored.GetHead(), 12) + ". Make a branch with git switch -c <name>.")
			default:
				con.Say("  Branch: " + restored.GetBranch())
			}
			for _, s := range restored.GetSkipped() {
				con.Say("  Not saved, as it was too large: " + s.GetPath() + ", " + strs.Size(float64(s.GetBytes())))
			}
			return nil
		},
	}
	command.Flags().StringVar(&path, "path", "", "Folder to restore the worktree to, instead of its old path")
	command.Flags().StringVar(&repo, "repo", "", "Repository folder, instead of the one you are in")
	return command
}
