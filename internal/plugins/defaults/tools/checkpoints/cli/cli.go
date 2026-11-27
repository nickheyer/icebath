package cli

import (
	"cmp"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	checkpointsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1/checkpointsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
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
		Use:     "checkpoints",
		GroupID: cli.GroupSafety,
		Short:   "See and free the space that checkpoints take",
		Long: "Before the agent changes files, icebath saves a checkpoint of the folder, which /rollback restores. The checkpoints of every " +
			"project are kept in one store. Use these commands to see how much space they take and to delete old ones. With no subcommand, " +
			"checkpoints shows the status.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return status(cmd, service, limit) },
	}
	root.Flags().IntVar(&limit, "limit", 20, "Largest number of projects to list")
	for _, name := range []string{"status", "list"} {
		limit := 20
		long := "Shows the size of the checkpoint store, then each project with how many checkpoints it has and when it last changed. " +
			"Projects whose folder is gone are marked orphan."
		if name == "list" {
			long = "Another name for status. " + long
		}
		command := &cobra.Command{Use: name, Short: "Show how much space checkpoints take, by project", Long: long, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return status(cmd, service, limit) }}
		command.Flags().IntVar(&limit, "limit", 20, "Largest number of projects to list")
		root.AddCommand(command)
	}
	root.AddCommand(prune(service), clearAll(service), clearLegacy(service))
	return []*cobra.Command{root}
}

// Age of a time in words, in the largest whole unit it has passed: "5 minutes ago"; Unknown for none
func ago(when time.Time) string {
	if when.IsZero() {
		return "Unknown"
	}
	age := time.Since(when)
	unit := 24 * time.Hour
	switch {
	case age < time.Second:
		return "Just now"
	case age < time.Minute:
		unit = time.Second
	case age < time.Hour:
		unit = time.Minute
	case age < 24*time.Hour:
		unit = time.Hour
	}
	return strs.Span(age.Truncate(unit)) + " ago"
}

// Size of bytes in words
func size(n int64) string { return strs.Size(float64(n)) }

// Folder of a project as a person reads it
func folder(project *checkpointsv1.Project) string {
	return cmp.Or(project.GetWorkdir(), "Unknown folder")
}

// Prints the sizes of the store and the archives, every project by when it was last snapshotted, and the archives
func status(cmd *cobra.Command, service checkpointsv1connect.CheckpointServiceClient, limit int) error {
	c := cli.NewConsole(cmd)
	info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
	if err != nil {
		return err
	}
	c.Pairs("Checkpoints", [][2]string{
		{"Folder", info.GetBase()},
		{"Space taken", size(info.GetTotalBytes())},
		{"Shared store", size(info.GetStoreBytes())},
		{"Old archives", size(info.GetLegacyBytes())},
		{"Projects", strs.Number(float64(len(info.GetProjects())))},
	})
	projects := slices.Clone(info.GetProjects())
	slices.SortStableFunc(projects, func(a, b *checkpointsv1.Project) int { return b.GetTouched().AsTime().Compare(a.GetTouched().AsTime()) })
	if len(projects) == 0 {
		c.Blank()
		c.Say("No project has checkpoints yet. One is saved each time the agent is about to change files in a project.")
	} else {
		if limit <= 0 {
			limit = 20
		}
		var rows [][]string
		for _, project := range projects[:min(limit, len(projects))] {
			workdir := folder(project)
			if runes := []rune(workdir); len(runes) > 60 {
				workdir = "…" + string(runes[len(runes)-59:])
			}
			state := "Orphan"
			if project.GetExists() {
				state = "Present"
			}
			touched := time.Time{}
			if project.GetTouched() != nil {
				touched = project.GetTouched().AsTime()
			}
			rows = append(rows, []string{workdir, strs.Number(float64(project.GetCommits())), ago(touched), state})
		}
		c.Blank()
		c.Table([]string{"Folder", "Checkpoints", "Last saved", "Folder state"}, rows)
		if hidden := len(projects) - limit; hidden > 0 {
			c.Say("  " + strs.Count(hidden, "more project") + " not shown. Pass --limit to show more.")
		}
	}
	if archives := slices.Clone(info.GetArchives()); len(archives) > 0 {
		slices.SortStableFunc(archives, func(a, b *checkpointsv1.Archive) int {
			return b.GetModified().AsTime().Compare(a.GetModified().AsTime())
		})
		c.Blank()
		c.Say(strs.Count(len(archives), "archive") + " left from the old checkpoint folder:")
		printArchives(c, archives)
		c.Blank()
		c.Say("Delete them with icebath checkpoints clear-legacy.")
	}
	if notice := info.GetFootprint(); notice != "" {
		c.Blank()
		c.Say(notice)
	}
	return nil
}

func printArchives(c *cli.Console, archives []*checkpointsv1.Archive) {
	var rows [][]string
	for _, archive := range archives {
		rows = append(rows, []string{archive.GetName(), size(archive.GetBytes())})
	}
	c.Table([]string{"Archive", "Size"}, rows)
}

// The force flag, or a yes to the question; what was not done said otherwise
func confirmed(cmd *cobra.Command, force bool, question string) bool {
	c := cli.NewConsole(cmd)
	if force || c.Confirm(question, false) {
		return true
	}
	c.Say("Nothing was deleted.")
	return false
}

func prune(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var retention, megabytes int
	var keepOrphans, force bool
	command := &cobra.Command{
		Use:   "prune",
		Short: "Delete old checkpoints to free space",
		Long: "Deletes the checkpoints of projects whose folder is gone and of projects that have not changed for --retention-days. Then " +
			"prune deletes the oldest checkpoints of each project until the store is no bigger than --max-size-mb. A folder may only seem " +
			"gone because a drive or network share is not connected, so prune asks before it deletes those projects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			request := &checkpointsv1.PruneRequest{RetentionDays: int32(retention), MaxSizeMb: int32(megabytes), Orphans: !keepOrphans}
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
					c.Say(strs.Count(len(orphans), "project") + " with checkpoints have a folder that cannot be found now:")
					c.Blank()
					var rows [][]string
					for _, project := range orphans {
						rows = append(rows, []string{folder(project), strs.Number(float64(project.GetCommits()))})
					}
					c.Table([]string{"Folder", "Checkpoints"}, rows)
					c.Blank()
					c.Say("A folder cannot be found when its project was deleted, or when the drive, network share or VPN it is on is not connected.")
					c.Say("Pass --keep-orphans to delete only checkpoints that are too old.")
					if !c.Confirm("Delete the checkpoints of these projects? They cannot be brought back.", false) {
						c.Say("Nothing was deleted.")
						return exit(1)
					}
				}
				request.AllowListed = true
				for _, project := range orphans {
					request.Allow = append(request.Allow, project.GetHash())
				}
			}
			age := "No age limit"
			if retention > 0 {
				age = strs.Span(time.Duration(retention) * 24 * time.Hour)
			}
			most := "No limit"
			if megabytes > 0 {
				most = strs.Size(float64(megabytes) * 1024 * 1024)
			}
			orphans := "Keep their checkpoints"
			if request.Orphans {
				orphans = "Delete their checkpoints"
			}
			c.Pairs("Deleting old checkpoints", [][2]string{
				{"Delete projects unchanged for", age},
				{"Projects whose folder is gone", orphans},
				{"Most space kept", most},
			})
			result, err := service.Prune(cmd.Context(), request)
			if err != nil {
				return err
			}
			c.Pairs("Done", [][2]string{
				{"Projects checked", strs.Number(float64(result.GetScanned()))},
				{"Deleted, folder gone", strs.Number(float64(result.GetOrphans()))},
				{"Deleted, too old", strs.Number(float64(result.GetStale()))},
				{"Could not delete", strs.Number(float64(result.GetErrors()))},
				{"Space freed", size(result.GetFreed())},
			})
			if result.GetErrors() > 0 {
				c.Warning("Some checkpoints could not be deleted. The log of icebath says why.")
			}
			return nil
		},
	}
	command.Flags().IntVar(&retention, "retention-days", 7, "Days a project may go unchanged before its checkpoints are deleted")
	command.Flags().IntVar(&megabytes, "max-size-mb", 500, "Most space in MB the store may take once pruned")
	command.Flags().BoolVar(&keepOrphans, "keep-orphans", false, "Keep the checkpoints of projects whose folder is gone")
	command.Flags().BoolVarP(&force, "force", "f", false, "Go ahead without asking first")
	return command
}

func clearAll(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "clear",
		Short: "Delete every checkpoint",
		Long: "Deletes the whole checkpoint store, so /rollback cannot restore anything saved before. clear asks first unless you pass " +
			"--force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
			if err != nil {
				return err
			}
			if !info.GetExists() {
				c.Say("There are no checkpoints to delete.")
				return nil
			}
			c.Pairs("Every checkpoint", [][2]string{
				{"Folder", info.GetBase()},
				{"Space taken", size(info.GetTotalBytes())},
				{"Projects", strs.Number(float64(len(info.GetProjects())))},
				{"Old archives", strs.Number(float64(len(info.GetArchives())))},
			})
			if !confirmed(cmd, force, "Delete every checkpoint of every folder? /rollback can then restore nothing saved before.") {
				return exit(1)
			}
			result, err := service.Clear(cmd.Context(), &checkpointsv1.ClearRequest{})
			if err != nil {
				c.Failure(tui.Reason(err))
				return exit(2)
			}
			c.Success("Deleted every checkpoint and freed " + size(result.GetFreed()) + ".")
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "Go ahead without asking first")
	return command
}

func clearLegacy(service checkpointsv1connect.CheckpointServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "clear-legacy",
		Short: "Delete the archives left from the old checkpoint folder",
		Long: "Deletes only the legacy archives. They hold what the checkpoint folder held before icebath kept all checkpoints in one store. " +
			"clear-legacy asks first unless you pass --force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			info, err := service.Status(cmd.Context(), &checkpointsv1.StatusRequest{})
			if err != nil {
				return err
			}
			archives := info.GetArchives()
			if len(archives) == 0 {
				c.Say("There are no archives left from the old checkpoint folder.")
				return nil
			}
			var total int64
			for _, archive := range archives {
				total += archive.GetBytes()
			}
			c.Say(strs.Count(len(archives), "archive") + " left from the old checkpoint folder take " + size(total) + ":")
			printArchives(c, archives)
			c.Blank()
			c.Say("They hold what the checkpoint folder held before all checkpoints moved into one store.")
			c.Say("Delete them once you are sure you need nothing in them.")
			if !confirmed(cmd, force, "Delete these archives? They cannot be brought back.") {
				return exit(1)
			}
			result, err := service.ClearLegacy(cmd.Context(), &checkpointsv1.ClearLegacyRequest{})
			if err != nil {
				return err
			}
			c.Success("Deleted " + strs.Count(int(result.GetDeleted()), "archive") + " and freed " + size(result.GetFreed()) + ".")
			if result.GetErrors() > 0 {
				c.Failure("Could not delete " + strs.Count(int(result.GetErrors()), "archive") + ". The log of icebath says why.")
				return exit(2)
			}
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "Go ahead without asking first")
	return command
}
