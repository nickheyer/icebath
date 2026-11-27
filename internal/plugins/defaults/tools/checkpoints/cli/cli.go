package cli

import (
	"fmt"
	"io"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/cli"
	checkpointsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1/checkpointsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Failure of a command with the exit status it ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := checkpointsv1connect.NewCheckpointServiceClient(conn)
	limit := 20
	root := &cobra.Command{
		Use:   "checkpoints",
		Short: "Inspect / prune / clear the filesystem checkpoint store",
		Long: "Manage the filesystem checkpoint store — the shadow git repo icebath uses to snapshot working directories before " +
			"write_file/patch/terminal calls. Lets you see how much space checkpoints occupy, force a prune, or wipe the base.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return status(cmd, service, limit) },
	}
	root.Flags().IntVar(&limit, "limit", 20, "Max projects to list (default 20)")
	for _, name := range []string{"status", "list"} {
		limit := 20
		short := "Show total size, project count, and per-project breakdown"
		if name == "list" {
			short = "Alias for 'status'"
		}
		command := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return status(cmd, service, limit) }}
		command.Flags().IntVar(&limit, "limit", 20, "Max projects to list (default 20)")
		root.AddCommand(command)
	}
	root.AddCommand(prune(service), clearAll(service), clearLegacy(service))
	return []*cobra.Command{root}
}

// Age of a time as seconds, minutes, hours or days ago, a dash for none
func ago(when time.Time) string {
	if when.IsZero() {
		return "—"
	}
	age := time.Since(when)
	switch {
	case age < 0:
		return "now"
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(age.Hours()/24))
}

// Prints the sizes of the store and the archives, every project by when it was last snapshotted, and the archives
func status(cmd *cobra.Command, service checkpointsv1connect.CheckpointServiceClient, limit int) error {
	out := cmd.OutOrStdout()
	info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Checkpoint base: %s\n", info.GetBase())
	fmt.Fprintf(out, "Total size:      %s\n", checkpoints.Size(info.GetTotalBytes()))
	fmt.Fprintf(out, "  store/         %s\n", checkpoints.Size(info.GetStoreBytes()))
	fmt.Fprintf(out, "  legacy-*       %s\n", checkpoints.Size(info.GetLegacyBytes()))
	fmt.Fprintf(out, "Projects:        %d\n", len(info.GetProjects()))
	projects := slices.Clone(info.GetProjects())
	slices.SortStableFunc(projects, func(a, b *checkpointsv1.Project) int { return b.GetTouched().AsTime().Compare(a.GetTouched().AsTime()) })
	if len(projects) > 0 {
		if limit <= 0 {
			limit = 20
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  %-60s  %7s  %12s  STATE\n", "WORKDIR", "COMMITS", "LAST TOUCH")
		for _, project := range projects[:min(limit, len(projects))] {
			workdir := project.GetWorkdir()
			if workdir == "" {
				workdir = "(unknown)"
			}
			if runes := []rune(workdir); len(runes) > 60 {
				workdir = "…" + string(runes[len(runes)-59:])
			}
			state := "orphan"
			if project.GetExists() {
				state = "live"
			}
			touched := time.Time{}
			if project.GetTouched() != nil {
				touched = project.GetTouched().AsTime()
			}
			fmt.Fprintf(out, "  %-60s  %7d  %12s  %s\n", workdir, project.GetCommits(), ago(touched), state)
		}
	}
	if archives := slices.Clone(info.GetArchives()); len(archives) > 0 {
		slices.SortStableFunc(archives, func(a, b *checkpointsv1.Archive) int {
			return b.GetModified().AsTime().Compare(a.GetModified().AsTime())
		})
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Legacy archives (%d):\n", len(archives))
		printArchives(out, archives)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Clear with: icebath checkpoints clear-legacy")
	}
	if notice := info.GetFootprint(); notice != "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, notice)
	}
	return nil
}

func printArchives(out io.Writer, archives []*checkpointsv1.Archive) {
	for _, archive := range archives {
		fmt.Fprintf(out, "  %-40s  %10s\n", archive.GetName(), checkpoints.Size(archive.GetBytes()))
	}
}

// The force flag, or a yes to the question; Aborted said otherwise
func confirmed(cmd *cobra.Command, force bool, question string) bool {
	if force || cli.NewConsole(cmd).Agree(question+" [y/N]: ", false) {
		return true
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
	return false
}

func prune(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var retention, size int
	var keepOrphans, force bool
	command := &cobra.Command{
		Use:   "prune",
		Short: "Delete orphan/stale checkpoints and GC the store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			request := &checkpointsv1.PruneRequest{RetentionDays: int32(retention), MaxSizeMb: int32(size), Orphans: !keepOrphans}
			if request.Orphans && !force {
				info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
				if err != nil {
					return err
				}
				var orphans []*checkpointsv1.Project
				for _, project := range info.GetProjects() {
					if !project.GetExists() {
						orphans = append(orphans, project)
					}
				}
				if len(orphans) > 0 {
					fmt.Fprintf(out, "This will permanently delete %d orphan checkpoint project(s) whose workdir is not currently reachable:\n\n", len(orphans))
					for _, project := range orphans {
						workdir := project.GetWorkdir()
						if workdir == "" {
							workdir = "(unknown)"
						}
						fmt.Fprintf(out, "  %s  (%d commit(s))\n", workdir, project.GetCommits())
					}
					fmt.Fprintln(out)
					fmt.Fprintln(out, "A workdir can be unreachable because the project was deleted,")
					fmt.Fprintln(out, "or because an external volume / network share / VPN is down.")
					fmt.Fprintln(out, "Pass --keep-orphans to prune stale entries only.")
					if !cli.NewConsole(cmd).Agree("Delete these orphan projects? [y/N]: ", false) {
						fmt.Fprintln(out, "Aborted.")
						return exit(1)
					}
				}
				request.AllowListed = true
				for _, project := range orphans {
					request.Allow = append(request.Allow, project.GetHash())
				}
			}
			fmt.Fprintln(out, "Pruning checkpoint store…")
			fmt.Fprintf(out, "  retention_days:    %d\n", retention)
			fmt.Fprintf(out, "  delete_orphans:    %t\n", request.Orphans)
			fmt.Fprintf(out, "  max_total_size_mb: %d\n", size)
			fmt.Fprintln(out)
			result, err := service.Prune(cmd.Context(), request)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Scanned:         %d\n", result.GetScanned())
			fmt.Fprintf(out, "Deleted orphan:  %d\n", result.GetOrphans())
			fmt.Fprintf(out, "Deleted stale:   %d\n", result.GetStale())
			fmt.Fprintf(out, "Errors:          %d\n", result.GetErrors())
			fmt.Fprintf(out, "Bytes reclaimed: %s\n", checkpoints.Size(result.GetFreed()))
			return nil
		},
	}
	command.Flags().IntVar(&retention, "retention-days", 7, "Drop projects whose last_touch is older than N days (default 7)")
	command.Flags().IntVar(&size, "max-size-mb", 500, "After orphan/stale prune, drop oldest commits per project until total size <= this (default 500)")
	command.Flags().BoolVar(&keepOrphans, "keep-orphans", false, "Skip deleting projects whose workdir no longer exists")
	command.Flags().BoolVarP(&force, "force", "f", false, "Skip the orphan-deletion confirmation prompt")
	return command
}

func clearAll(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "clear",
		Short: "Delete the entire checkpoint base (all /rollback history)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
			if err != nil {
				return err
			}
			if !info.GetExists() {
				fmt.Fprintln(out, "Nothing to clear — checkpoint base does not exist.")
				return nil
			}
			fmt.Fprintf(out, "This will delete the ENTIRE checkpoint base at %s\n", info.GetBase())
			fmt.Fprintf(out, "  size:        %s\n", checkpoints.Size(info.GetTotalBytes()))
			fmt.Fprintf(out, "  projects:    %d\n", len(info.GetProjects()))
			fmt.Fprintf(out, "  legacy dirs: %d\n", len(info.GetArchives()))
			fmt.Fprintln(out)
			fmt.Fprintln(out, "All /rollback history for every working directory will be lost.")
			if !confirmed(cmd, force, "Proceed?") {
				return exit(1)
			}
			result, err := service.Clear(cmd.Context(), &checkpointsv1.ClearRequest{})
			if err != nil {
				fmt.Fprintln(out, tui.Reason(err))
				return exit(2)
			}
			fmt.Fprintf(out, "Cleared. Reclaimed %s.\n", checkpoints.Size(result.GetFreed()))
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation prompt")
	return command
}

func clearLegacy(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "clear-legacy",
		Short: "Delete only the legacy-<ts>/ archives of what the checkpoint folder held before its store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
			if err != nil {
				return err
			}
			archives := info.GetArchives()
			if len(archives) == 0 {
				fmt.Fprintln(out, "No legacy archives to clear.")
				return nil
			}
			var total int64
			for _, archive := range archives {
				total += archive.GetBytes()
			}
			fmt.Fprintf(out, "Found %d legacy archive(s), total %s:\n", len(archives), checkpoints.Size(total))
			printArchives(out, archives)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Legacy archives hold what the checkpoint folder held before its shared")
			fmt.Fprintln(out, "store was made, moved aside at that time. Delete when you're confident")
			fmt.Fprintln(out, "you don't need anything in them.")
			if !confirmed(cmd, force, "Delete all legacy archives?") {
				return exit(1)
			}
			result, err := service.ClearLegacy(cmd.Context(), &checkpointsv1.ClearLegacyRequest{})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Deleted %d archive(s), reclaimed %s.\n", result.GetDeleted(), checkpoints.Size(result.GetFreed()))
			if result.GetErrors() > 0 {
				fmt.Fprintf(out, "Could not delete %d archive(s) (see logs).\n", result.GetErrors())
				return exit(2)
			}
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation prompt")
	return command
}
