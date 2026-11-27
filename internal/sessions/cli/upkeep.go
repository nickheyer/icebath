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

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Line of a session in a prune or archive preview
func previewLine(s *sessionsv1.Session) string {
	model := s.GetModel()
	if at := strings.LastIndex(model, "/"); at >= 0 {
		model = model[at+1:]
	}
	if model == "" {
		model = "-"
	}
	return fmt.Sprintf("  %s  %s %s %s %4d msgs  %s", s.GetId(), cli.Pad(shown(s.GetUpdated()), 17), cli.Pad(s.GetSource(), 10), cli.Pad(strs.Head(model, 24), 24),
		s.GetMessages(), strs.Head(s.GetTitle(), 36))
}

func addPrune(parent *cobra.Command, at func(*cobra.Command) *run) {
	var f filters
	var includeArchived, includePinned, neverActive, force bool
	command := &cobra.Command{Use: "prune", Short: "Delete old sessions (filterable by time window, source, title, ...)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if neverActive {
				return r.pruneNeverActive(&f)
			}
			filter, err := f.build("90")
			if err != nil {
				return errors.New("Error: " + err.Error())
			}
			filter.IncludeArchived, filter.IncludePinned = includeArchived, includePinned
			preview, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
			if err != nil {
				return errors.New("Error: " + tui.Reason(err))
			}
			return r.confirmBulk(&f, filter, preview.GetSessions(), int(preview.GetPinned()), int(preview.GetOpen()), true, func() error {
				done, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, Force: force})
				if err != nil {
					return errors.New("Error: " + tui.Reason(err))
				}
				r.printf("Pruned %d session(s).", done.GetDeleted())
				if done.GetBusy() > 0 {
					r.printf("Skipped %d session(s) with a live turn or compression lock.", done.GetBusy())
				}
				return nil
			})
		}}
	f.bind(command, "Delete sessions older than AGE — days if bare number, or a duration like '5h'/'2d'/'1w', or an ISO timestamp (bare prune with no "+
		"filters defaults to 90 days; any filter matches all ages)")
	command.Flags().BoolVar(&includeArchived, "include-archived", false, "Also delete archived sessions (excluded by default)")
	command.Flags().BoolVar(&includePinned, "include-pinned", false, "Also delete pinned sessions (excluded by default — pin is a keep flag)")
	command.Flags().BoolVar(&neverActive, "never-active", false, "Instead of ended sessions, delete keyed gateway rows that were opened and never used (no messages, "+
		"tokens, tool calls or title) and are older than AGE (default 30 days). Ordinary prune can never reach these — it only ever selects ended sessions")
	command.Flags().BoolVar(&force, "force", false, "Run even while another icebath process holds the session database — rewriting the store under a "+
		"live writer can leave every agent refusing turns until all writers are stopped")
	parent.AddCommand(command)
}

// Shows what a bulk prune or archive matches, then asks and runs it
func (r *run) confirmBulk(f *filters, filter *sessionsv1.Filter, matches []*sessionsv1.Session, pinned, open int, prune bool, apply func() error) error {
	verb, noun := "archived", "Archive"
	if prune {
		verb, noun = "deleted", "Delete"
	}
	if pinned > 0 && !filter.GetIncludePinned() {
		tail := "Unpin first with `icebath sessions unpin <id>` to include them."
		if prune {
			tail = "Pass --include-pinned to delete them anyway, or unpin first with `icebath sessions unpin <id>`."
		}
		r.printf("Note: %s also match these filters but will NOT be %s (pin is a keep flag). %s", plural(pinned, "pinned session"), verb, tail)
	}
	if open > 0 {
		if prune {
			r.printf("Note: %s also match these filters but will be skipped because prune only deletes ended sessions. Use `icebath sessions delete <id>` "+
				"to remove one explicitly.", plural(open, "open session"))
		} else {
			r.printf("Note: %d matching session%s still open — skipped because archive defaults to ended sessions. Use --include-open to archive them too.",
				open, map[bool]string{true: "", false: "s"}[open == 1])
		}
	}
	if len(matches) == 0 {
		r.printf("No sessions match (%s).", describe(filter))
		return nil
	}
	spanText := fmt.Sprintf("oldest activity %s, newest activity %s", shown(matches[0].GetUpdated()), shown(matches[len(matches)-1].GetUpdated()))
	if f.dryRun || !f.yes {
		r.printf("%d session(s) match (%s; %s):", len(matches), describe(filter), spanText)
		limit := previewRows
		if f.dryRun {
			limit = len(matches)
		}
		for _, s := range matches[:min(limit, len(matches))] {
			r.print(previewLine(s))
		}
		if len(matches) > limit {
			r.printf("  … %d more", len(matches)-limit)
		}
	}
	if f.dryRun {
		r.printf("Dry run — nothing %s.", verb)
		return nil
	}
	if !f.yes {
		ok, err := r.confirm(fmt.Sprintf("%s these %d session(s) (%s)? [y/N] ", noun, len(matches), spanText), "Cancelled.", "--yes")
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
			return fmt.Errorf("Error: --older-than '%s' is not a duration. Use a bare number of days or a form like '2d' / '1w'.", f.olderThan)
		}
	}
	filter := &sessionsv1.Filter{NeverActive: true, ActiveBefore: timestampAgo(time.Duration(age * 86400 * float64(time.Second)))}
	preview, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
	if err != nil {
		return errors.New("Error: " + tui.Reason(err))
	}
	matches := preview.GetSessions()
	if len(matches) == 0 {
		r.printf("No never-active keyed sessions older than %g day(s).", age)
		return nil
	}
	r.printf("%d never-active keyed session(s) older than %g day(s) — no messages, tokens, tool calls or title:", len(matches), age)
	limit := previewRows
	if f.dryRun {
		limit = len(matches)
	}
	for _, s := range matches[:min(limit, len(matches))] {
		r.printf("  %s  %s %s %s", s.GetId(), cli.Pad(shown(s.GetCreated()), 17), cli.Pad(cmp.Or(s.GetSource(), "-"), 10), cmp.Or(s.GetPeer().GetSessionKey(), "-"))
	}
	if len(matches) > limit {
		r.printf("  … %d more", len(matches)-limit)
	}
	if f.dryRun {
		r.print("Dry run — nothing deleted.")
		return nil
	}
	if !f.yes {
		ok, err := r.confirm(fmt.Sprintf("Delete %d session(s)? [y/N] ", len(matches)), "Aborted.", "--yes")
		if err != nil || !ok {
			return err
		}
	}
	done, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter})
	if err != nil {
		return errors.New("Error: " + tui.Reason(err))
	}
	r.printf("Deleted %d never-active session(s) and %d stale routing entr(ies).", done.GetDeleted(), done.GetRoutes())
	if done.GetBusy() > 0 {
		r.printf("Skipped %d session(s) with a live turn or compression lock.", done.GetBusy())
	}
	return nil
}

func addArchive(parent *cobra.Command, at func(*cobra.Command) *run) {
	var f filters
	var includeOpen bool
	command := &cobra.Command{Use: "archive", Short: "Bulk-archive (soft-hide) sessions matching filters — no deletion", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			filter, err := f.build("")
			if err != nil {
				return errors.New("Error: " + err.Error())
			}
			if !f.any() || f.set("older-than") && len(filterFlagsSet(&f)) == 1 {
				return errors.New("Refusing to archive every ended session: pass at least one filter (e.g. --newer-than 5h, --source cli, --title codex).")
			}
			filter.IncludeOpen = includeOpen
			preview, err := r.service.Archive(r.ctx(), &sessionsv1.ArchiveRequest{Filter: filter, DryRun: true})
			if err != nil {
				return errors.New("Error: " + tui.Reason(err))
			}
			return r.confirmBulk(&f, filter, preview.GetSessions(), int(preview.GetPinned()), int(preview.GetOpen()), false, func() error {
				done, err := r.service.Archive(r.ctx(), &sessionsv1.ArchiveRequest{Filter: filter})
				if err != nil {
					return errors.New("Error: " + tui.Reason(err))
				}
				r.printf("Archived %d session(s). They're hidden from listings but fully recoverable (nothing was deleted).", done.GetArchived())
				return nil
			})
		}}
	f.bind(command, "Only archive sessions older than AGE (duration like '5h'/'2d', bare number of days, or ISO timestamp)")
	command.Flags().BoolVar(&includeOpen, "include-open", false, "Also archive never-ended sessions")
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
	command := &cobra.Command{Use: "optimize", Short: "Reclaim disk space: merge FTS5 segments + VACUUM (no data change)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_OPTIMIZE, Force: force})
		}}
	command.Flags().BoolVar(&force, "force", false, "Run even while another icebath process holds the session database — rewriting the store under a live "+
		"writer can leave every agent refusing turns until all writers are stopped")
	parent.AddCommand(command)
}

func addCleanMarkers(parent *cobra.Command, at func(*cobra.Command) *run) {
	var dryRun, noBackup bool
	command := &cobra.Command{Use: "clean-markers", Short: "Permanently clear stale tool-call marker content left as reply text",
		Long: "A local tool-call template can persist a bare bracketed marker (e.g. \"[memory]\") as an assistant turn's content instead of real text. This " +
			"rewrites the affected rows once, in place. Only the content column is touched; tool_calls and every other column on the row are left untouched.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_CLEAN_MARKERS, DryRun: dryRun, Backup: !noBackup})
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Report the affected row count without writing")
	command.Flags().BoolVar(&noBackup, "no-backup", false, "Skip the timestamped database backup taken before writing (not recommended)")
	parent.AddCommand(command)
}

func addOptimizeStorage(parent *cobra.Command, at func(*cobra.Command) *run) {
	var noVacuum, yes, force bool
	command := &cobra.Command{Use: "optimize-storage", Short: "Rebuild the search index in its compact layout (reclaims disk on large DBs)",
		Long: "Rebuild the full-text search index from the messages when it is missing, detached after damage, or left half done, then VACUUM. Refuses " +
			"while another icebath process holds the database. Safe to interrupt and re-run. No conversation data is changed; only the search index is rebuilt.",
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
				ok, err := r.confirm("Proceed? [y/N] ", "Cancelled.", "--yes")
				if err != nil || !ok {
					return err
				}
			}
			request.DryRun = false
			return r.maintain(request)
		}}
	command.Flags().BoolVar(&noVacuum, "no-vacuum", false, "Skip the final VACUUM (index is rebuilt but freed pages aren't returned to the OS until a later VACUUM)")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the disk-space confirmation prompt")
	command.Flags().BoolVar(&force, "force", false, "Run even while another icebath process holds the session database")
	parent.AddCommand(command)
}

func addRepair(parent *cobra.Command, at func(*cobra.Command) *run) {
	var checkOnly, noBackup bool
	command := &cobra.Command{Use: "repair", Short: "Repair a malformed session database schema so hidden sessions reappear",
		Long: "Recover a session database whose schema is malformed (e.g. 'table messages_fts already exists'), which makes session listings come back empty. " +
			"A backup is made first; sessions and messages are preserved and the FTS search index is rebuilt if needed.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_REPAIR, CheckOnly: checkOnly, Backup: !noBackup})
		}}
	command.Flags().BoolVar(&checkOnly, "check-only", false, "Only report whether the database opens cleanly; do not modify it")
	command.Flags().BoolVar(&noBackup, "no-backup", false, "Skip the timestamped backup copy (not recommended)")
	parent.AddCommand(command)
}

func addJournalMode(parent *cobra.Command, at func(*cobra.Command) *run) {
	var path string
	var force bool
	command := &cobra.Command{Use: "set-journal-mode <delete|wal>", Short: "Convert the session database between journal_mode=WAL and DELETE offline (every holder stopped)",
		Long: "Switch the on-disk journal mode of the session store. Icebath never live-downgrades a WAL database at startup (other processes may hold " +
			"uncheckpointed commits), so a journal mode of delete in the settings cannot apply itself to an existing WAL store. Run this with every other " +
			"icebath stopped: it refuses while any process holds the file, switches the mode, and verifies the file header.",
		Args: cobra.ExactArgs(1), ValidArgs: []string{"delete", "wal"}, RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "delete" && args[0] != "wal" {
				return fmt.Errorf("invalid choice: '%s' (choose from 'delete', 'wal')", args[0])
			}
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_JOURNAL_MODE, Mode: args[0], Path: path, Force: force})
		}}
	command.Flags().StringVar(&path, "db", "", "Convert another icebath SQLite store instead of the session database")
	command.Flags().BoolVar(&force, "force", false, "Proceed when the holder scan itself fails (cannot prove the store is quiet) after stopping every icebath "+
		"process yourself; a process the scan does find is still refused")
	parent.AddCommand(command)
}

func addRecover(parent *cobra.Command, at func(*cobra.Command) *run) {
	var source, output, workDir, report string
	var inspectOnly, allowPartial bool
	var chunk int
	command := &cobra.Command{Use: "recover", Short: "Rebuild canonical session data into a separate clean database",
		Long: "Offline, non-destructive recovery for a damaged session database. The source database and its WAL/SHM/rollback-journal sidecars are copied " +
			"before SQLite opens anything. Canonical rows are rebuilt into a new output database; derived search indexes are recreated and the active " +
			"database is never replaced automatically.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if !inspectOnly && output == "" {
				return errors.New("--output is required unless --inspect-only is used.")
			}
			if _, err := os.Stat(source); err != nil {
				return fmt.Errorf("Source is not a file: %s", source)
			}
			return at(cmd).maintain(&sessionsv1.MaintainRequest{Task: sessionsv1.Task_TASK_RECOVER, Path: source, Output: output, CheckOnly: inspectOnly,
				WorkDir: workDir, ChunkSize: int32(chunk), AllowPartial: allowPartial, Report: report})
		}}
	command.Flags().StringVar(&source, "source", "", "Source session database or preserved backup to inspect/recover")
	command.MarkFlagRequired("source")
	command.Flags().StringVar(&output, "output", "", "New recovery database path (required unless --inspect-only)")
	command.Flags().BoolVar(&inspectOnly, "inspect-only", false, "Only report canonical table readability; do not create an output database")
	command.Flags().StringVar(&workDir, "work-dir", "", "Existing directory for the disposable source copy (defaults beside the output)")
	command.Flags().IntVar(&chunk, "chunk-size", 1000, "Rows committed per recovery batch (default: 1000)")
	command.Flags().BoolVar(&allowPartial, "allow-partial", false, "Best-effort salvage across damaged row ranges; the output remains separate and every skipped range is recorded")
	command.Flags().StringVar(&report, "report", "", "JSON report path (defaults to <output>.recovery.json)")
	parent.AddCommand(command)
}
