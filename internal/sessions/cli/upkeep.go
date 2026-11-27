package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/cli"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Rows a confirmation previews before the rest are counted
const previewRows = 15

// Runs a maintenance task on the store, printing its progress, failing with its last line when it fails
func (r *run) maintain(request *sessionsv1.MaintainRequest) error {
	_, err := r.task(request)
	return err
}

// Runs a maintenance task, printing and returning the lines it reports
func (r *run) task(request *sessionsv1.MaintainRequest) ([]string, error) {
	stream, err := r.service.Maintain(r.ctx(), request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var lines []string
	for {
		response, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
		if response.GetFailed() {
			return lines, errors.New(response.GetLine())
		}
		if response.GetLine() != "" {
			r.print(response.GetLine())
			lines = append(lines, response.GetLine())
		}
		if response.GetDone() {
			return lines, nil
		}
	}
}

// Time the duration ago
func timestampAgo(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(-d))
}

// Count of things with the verb after it agreeing: 1 session matches, 2 sessions match
func agreeing(n int, noun, one, many string) string {
	if n == 1 {
		return strs.Count(n, noun) + " " + one
	}
	return strs.Count(n, noun) + " " + many
}

// Row of a session in a prune or archive preview
func previewRow(s *sessionsv1.Session) []string {
	model := s.GetModel()
	if at := strings.LastIndex(model, "/"); at >= 0 {
		model = model[at+1:]
	}
	return []string{s.GetId(), shown(s.GetUpdated()), cmp.Or(s.GetSource(), "Unknown"), cmp.Or(model, "Unknown"), strs.Number(float64(s.GetMessages())),
		cmp.Or(s.GetTitle(), "Untitled")}
}

// How the time filters read a time
const times = "A time is a number of days, a length such as 30m, 5h, 2d or 1w, or a date such as 2026-07-05 14:30."

func addPrune(parent *cobra.Command, at func(*cobra.Command) *run) {
	var f filters
	var includeArchived, includePinned, neverActive, force bool
	command := &cobra.Command{Use: "prune", Short: "Delete old sessions",
		Long: "Deletes the ended sessions that match the filters, after showing them and asking. With no filter, prune deletes sessions " +
			"older than 90 days. With any filter, sessions of every age can match. Archived and pinned sessions stay unless you include " +
			"them. " + times + "\n\n" +
			"--never-active deletes something else: sessions from chat apps that were opened and never used, with no messages, tokens, " +
			"tool calls or title, older than --older-than or 30 days. An ordinary prune never reaches these, because it only picks ended " +
			"sessions.",
		Example: "  icebath sessions prune --dry-run\n  icebath sessions prune --older-than 30 --source telegram\n" +
			"  icebath sessions prune --title scratch --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if neverActive {
				return r.pruneNeverActive(&f)
			}
			filter, err := f.build("90")
			if err != nil {
				return err
			}
			filter.IncludeArchived, filter.IncludePinned = includeArchived, includePinned
			preview, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
			if err != nil {
				return errors.New(sentence(tui.Reason(err)))
			}
			return r.confirmBulk(&f, filter, preview.GetSessions(), int(preview.GetPinned()), int(preview.GetOpen()), true, func() error {
				done, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, Force: force})
				if err != nil {
					return errors.New(sentence(tui.Reason(err)))
				}
				r.printf("✓ Deleted %s.", strs.Count(int(done.GetDeleted()), "session"))
				if done.GetBusy() > 0 {
					r.printf("Skipped %s in the middle of a reply or of being summarized.", strs.Count(int(done.GetBusy()), "session"))
				}
				return nil
			})
		}}
	f.bind(command, "Only sessions older than this time, such as 30 for 30 days, 5h or a date")
	command.Flags().BoolVar(&includeArchived, "include-archived", false, "Also delete archived sessions")
	command.Flags().BoolVar(&includePinned, "include-pinned", false, "Also delete pinned sessions")
	command.Flags().BoolVar(&neverActive, "never-active", false, "Delete chat app sessions that were opened and never used, instead of ended ones")
	command.Flags().BoolVar(&force, "force", false, "Run while another icebath holds the session store, which can stall every agent")
	parent.AddCommand(command)
}

// Shows what a bulk prune or archive matches, then asks and runs it
func (r *run) confirmBulk(f *filters, filter *sessionsv1.Filter, matches []*sessionsv1.Session, pinned, open int, prune bool, apply func() error) error {
	verb := "archived"
	if prune {
		verb = "deleted"
	}
	if pinned > 0 && !filter.GetIncludePinned() {
		tail := "Unpin them first with icebath sessions unpin <session_ids...> to include them."
		if prune {
			tail = "Pass --include-pinned to delete them anyway, or unpin them first with icebath sessions unpin <session_ids...>."
		}
		r.printf("%s too, and they stay because they are pinned. %s", agreeing(pinned, "pinned session", "matches", "match"), tail)
	}
	if open > 0 {
		if prune {
			r.printf("%s too, and they stay because prune only deletes ended sessions. Delete one yourself with icebath sessions delete "+
				"<session_id>.", agreeing(open, "open session", "matches", "match"))
		} else {
			r.printf("%s too, and they stay because they are still open. Add --include-open to archive them too.",
				agreeing(open, "open session", "matches", "match"))
		}
	}
	if len(matches) == 0 {
		r.printf("No session matches %s.", describe(filter))
		return nil
	}
	spanText := fmt.Sprintf("last used between %s and %s", shown(matches[0].GetUpdated()), shown(matches[len(matches)-1].GetUpdated()))
	if f.dryRun || !f.yes {
		r.printf("Filters: %s.", describe(filter))
		r.printf("%s, %s:", agreeing(len(matches), "session", "matches", "match"), spanText)
		limit := previewRows
		if f.dryRun {
			limit = len(matches)
		}
		var table [][]string
		for _, s := range matches[:min(limit, len(matches))] {
			table = append(table, previewRow(s))
		}
		cli.NewConsole(r.cmd).Table([]string{"ID", "Last used", "Source", "Model", "Messages", "Title"}, table)
		if len(matches) > limit {
			r.printf("  And %s more.", strs.Number(float64(len(matches)-limit)))
		}
	}
	if f.dryRun {
		r.printf("Nothing was %s, because you passed --dry-run.", verb)
		return nil
	}
	if !f.yes {
		question := fmt.Sprintf("Archive these %s? They stay kept, and you can bring them back. [y/N] ", strs.Count(len(matches), "session"))
		if prune {
			question = fmt.Sprintf("Delete these %s? Their history can't be brought back. [y/N] ", strs.Count(len(matches), "session"))
		}
		ok, err := r.confirm(question, "Nothing was "+verb+".", "--yes")
		if err != nil || !ok {
			return err
		}
	}
	return apply()
}

func (r *run) pruneNeverActive(f *filters) error {
	age := 30.0
	if f.olderThan != "" {
		value := strings.ToLower(strings.TrimSpace(f.olderThan))
		switch match := span.FindStringSubmatch(value); {
		case bareDays.MatchString(value):
			fmt.Sscanf(value, "%g", &age)
		case match != nil:
			var n float64
			fmt.Sscanf(match[1], "%g", &n)
			age = n * units[match[2][0]] / 86400
		default:
			return fmt.Errorf("--older-than takes a number of days or a length such as 2d or 1w, and “%s” is neither.", f.olderThan)
		}
	}
	filter := &sessionsv1.Filter{NeverActive: true, ActiveBefore: timestampAgo(time.Duration(age * 86400 * float64(time.Second)))}
	preview, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
	if err != nil {
		return errors.New(sentence(tui.Reason(err)))
	}
	matches := preview.GetSessions()
	older := strs.Span(time.Duration(age * 24 * float64(time.Hour)).Round(time.Hour))
	if len(matches) == 0 {
		r.printf("No chat app session older than %s was opened and never used.", older)
		return nil
	}
	r.printf("%s older than %s, opened and never used, with no messages, tokens, tool calls or title:",
		agreeing(len(matches), "chat app session", "is", "are"), older)
	limit := previewRows
	if f.dryRun {
		limit = len(matches)
	}
	var table [][]string
	for _, s := range matches[:min(limit, len(matches))] {
		table = append(table, []string{s.GetId(), shown(s.GetCreated()), cmp.Or(s.GetSource(), "Unknown"), cmp.Or(s.GetPeer().GetSessionKey(), "None")})
	}
	cli.NewConsole(r.cmd).Table([]string{"ID", "Opened", "Source", "Chat key"}, table)
	if len(matches) > limit {
		r.printf("  And %s more.", strs.Number(float64(len(matches)-limit)))
	}
	if f.dryRun {
		r.print("Nothing was deleted, because you passed --dry-run.")
		return nil
	}
	if !f.yes {
		ok, err := r.confirm(fmt.Sprintf("Delete these %s? They hold nothing, and they can't be brought back. [y/N] ", strs.Count(len(matches), "session")),
			"Nothing was deleted.", "--yes")
		if err != nil || !ok {
			return err
		}
	}
	done, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter})
	if err != nil {
		return errors.New(sentence(tui.Reason(err)))
	}
	r.printf("✓ Deleted %s and %s.", strs.Count(int(done.GetDeleted()), "unused session"), strs.Count(int(done.GetRoutes()), "old route"))
	if done.GetBusy() > 0 {
		r.printf("Skipped %s in the middle of a reply or of being summarized.", strs.Count(int(done.GetBusy()), "session"))
	}
	return nil
}

func addArchive(parent *cobra.Command, at func(*cobra.Command) *run) {
	var f filters
	var includeOpen bool
	command := &cobra.Command{Use: "archive", Short: "Archive the sessions that match filters",
		Long: "Archives the ended sessions that match the filters, after showing them and asking. An archived session is hidden from " +
			"lists but kept, and you can bring it back. Nothing is deleted. " + times,
		Example: "  icebath sessions archive --older-than 14 --dry-run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			filter, err := f.build("")
			if err != nil {
				return err
			}
			if !f.any() || f.set("older-than") && len(filterFlagsSet(&f)) == 1 {
				return errors.New("archive needs a filter besides --older-than, so it never archives every ended session. Add one such as " +
					"--newer-than 5h, --source cli or --title codex.")
			}
			filter.IncludeOpen = includeOpen
			preview, err := r.service.Archive(r.ctx(), &sessionsv1.ArchiveRequest{Filter: filter, DryRun: true})
			if err != nil {
				return errors.New(sentence(tui.Reason(err)))
			}
			return r.confirmBulk(&f, filter, preview.GetSessions(), int(preview.GetPinned()), int(preview.GetOpen()), false, func() error {
				done, err := r.service.Archive(r.ctx(), &sessionsv1.ArchiveRequest{Filter: filter})
				if err != nil {
					return errors.New(sentence(tui.Reason(err)))
				}
				r.printf("✓ Archived %s. They are hidden from lists and kept whole, and you can bring them back.",
					strs.Count(int(done.GetArchived()), "session"))
				return nil
			})
		}}
	f.bind(command, "Only sessions older than this time, such as 30 for 30 days, 5h or a date")
	command.Flags().BoolVar(&includeOpen, "include-open", false, "Also archive sessions that never ended")
	parent.AddCommand(command)
}

func filterFlagsSet(f *filters) []string {
	var out []string
	for _, name := range filterFlags {
		if f.set(name) {
			out = append(out, name)
		}
	}
	return out
}

func addOptimize(parent *cobra.Command, at func(*cobra.Command) *run) {
	var force bool
	command := &cobra.Command{Use: "optimize", Short: "Free disk space in the session store",
		Long: "Tidies the search index and compacts the session database to free disk space. No session or message changes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_OPTIMIZE, Force: force})
		}}
	command.Flags().BoolVar(&force, "force", false, "Run while another icebath holds the session store, which can stall every agent")
	parent.AddCommand(command)
}

func addCleanMarkers(parent *cobra.Command, at func(*cobra.Command) *run) {
	var dryRun, noBackup bool
	command := &cobra.Command{Use: "clean-markers", Short: "Clear tool markers saved as reply text",
		Long: "Some local models save a bare marker such as [memory] as the text of a reply instead of real text. clean-markers clears that " +
			"text once, in place, after backing up the database. Nothing else in those replies changes, the tool calls included.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_CLEAN_MARKERS, DryRun: dryRun, Backup: !noBackup})
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Only count the replies that would change")
	command.Flags().BoolVar(&noBackup, "no-backup", false, "Skip the backup of the database taken before the change")
	parent.AddCommand(command)
}

func addOptimizeStorage(parent *cobra.Command, at func(*cobra.Command) *run) {
	var noVacuum, yes, force bool
	command := &cobra.Command{Use: "optimize-storage", Short: "Rebuild the search index in less space",
		Long: "Builds the search index of the session store again from the messages, in a smaller layout, then compacts the database. " +
			"optimize-storage also mends an index that is missing, damaged or half built. Only the search index changes, never a session or " +
			"message. optimize-storage " +
			"refuses while another icebath holds the store, and you can stop it and run it again safely.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			request := &sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_OPTIMIZE_STORAGE, NoVacuum: noVacuum, Force: force, DryRun: true}
			plan, err := r.task(request)
			if err != nil {
				return err
			}
			if len(plan) > 0 && strings.Contains(plan[len(plan)-1], "nothing to do") {
				return nil
			}
			if !yes {
				ok, err := r.confirm("Rebuild the search index now? [y/N] ", "Nothing changed.", "--yes")
				if err != nil || !ok {
					return err
				}
			}
			request.DryRun = false
			return r.maintain(request)
		}}
	command.Flags().BoolVar(&noVacuum, "no-vacuum", false, "Skip compacting the database at the end, so the freed space stays in the file")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking about disk space")
	command.Flags().BoolVar(&force, "force", false, "Run even while another icebath holds the session store")
	parent.AddCommand(command)
}

func addRepair(parent *cobra.Command, at func(*cobra.Command) *run) {
	var checkOnly, noBackup bool
	command := &cobra.Command{Use: "repair", Short: "Repair a damaged session store so sessions show again",
		Long: "Mends a session database whose layout is damaged, such as one that says table messages_fts already exists, which makes " +
			"session lists come back empty. repair backs the database up first, keeps every session and message and rebuilds the search " +
			"index when needed.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_REPAIR, CheckOnly: checkOnly, Backup: !noBackup})
		}}
	command.Flags().BoolVar(&checkOnly, "check-only", false, "Only report whether the database opens cleanly, without changing it")
	command.Flags().BoolVar(&noBackup, "no-backup", false, "Skip the backup of the database taken before the repair")
	parent.AddCommand(command)
}

func addJournalMode(parent *cobra.Command, at func(*cobra.Command) *run) {
	var path string
	var force bool
	command := &cobra.Command{Use: "set-journal-mode <delete|wal>", Short: "Switch the journal mode of the session store",
		Long: "Switches the session database between the wal and delete journal modes. icebath never leaves wal on its own at startup, " +
			"because another process may still have changes in the journal. So a journal mode of delete in the settings cannot change a " +
			"store that is already wal. Stop every other icebath first. set-journal-mode refuses while any process holds the file, then " +
			"switches the mode and checks the result.",
		Example: "  icebath sessions set-journal-mode delete",
		Args:    cobra.ExactArgs(1), ValidArgs: []string{"delete", "wal"}, RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "delete" && args[0] != "wal" {
				return fmt.Errorf("set-journal-mode takes delete or wal, not “%s”.", args[0])
			}
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_JOURNAL_MODE, Mode: args[0], Path: path, Force: force})
		}}
	command.Flags().StringVar(&path, "db", "", "Another icebath SQLite file to switch, instead of the session database")
	command.Flags().BoolVar(&force, "force", false, "Go on when the check for other processes fails, after you stopped them yourself")
	parent.AddCommand(command)
}

func addRecover(parent *cobra.Command, at func(*cobra.Command) *run) {
	var source, output, workDir, report string
	var inspectOnly, allowPartial bool
	var chunk int
	command := &cobra.Command{Use: "recover", Short: "Rescue the sessions of a damaged store into a new one",
		Long: "Rescues what can be read from a damaged session database into a new database, without changing the damaged one. recover copies " +
			"the database and its journal files before it opens anything. recover rebuilds the sessions and messages in the new database and " +
			"makes the search index again. icebath never swaps the new database in for you.",
		Example: "  icebath sessions recover --source sessions.db --output recovered.db\n  icebath sessions recover --source sessions.db --inspect-only",
		Args:    cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if !inspectOnly && output == "" {
				return errors.New("recover needs --output, unless you pass --inspect-only.")
			}
			if _, err := os.Stat(source); err != nil {
				return fmt.Errorf("There is no file at %s to recover from.", source)
			}
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_RECOVER, Path: source, Output: output, CheckOnly: inspectOnly,
				WorkDir: workDir, ChunkSize: int32(chunk), AllowPartial: allowPartial, Report: report})
		}}
	command.Flags().StringVar(&source, "source", "", "Damaged session database or backup to read")
	command.MarkFlagRequired("source")
	command.Flags().StringVar(&output, "output", "", "Path of the new database to write, needed unless --inspect-only")
	command.Flags().BoolVar(&inspectOnly, "inspect-only", false, "Only report which tables can be read, without writing a new database")
	command.Flags().StringVar(&workDir, "work-dir", "", "Folder for the temporary copy of the source, instead of beside the output")
	command.Flags().IntVar(&chunk, "chunk-size", 1000, "Rows saved in each batch")
	command.Flags().BoolVar(&allowPartial, "allow-partial", false, "Rescue what it can past damaged rows, noting every range it skips")
	command.Flags().StringVar(&report, "report", "", "Path of the JSON report, instead of the output's name with .recovery.json")
	parent.AddCommand(command)
}
