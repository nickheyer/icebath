package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Runs the terminal app, opening the session in the context in place of a new chat
type App func(ctx context.Context, conn *connect.Client) error

type commands struct {
	app App
}

func Register(host *cli.CLI, app App) {
	host.Register(commands{app: app})
}

// What every subcommand works with: the session service and where it writes
type run struct {
	cmd     *cobra.Command
	service sessionsv1connect.SessionServiceClient
	conn    *connect.Client
	app     App
}

func (r *run) ctx() context.Context { return r.cmd.Context() }

func (r *run) print(text string) {
	fmt.Fprintln(r.cmd.OutOrStdout(), text)
}

func (r *run) printf(format string, args ...any) {
	fmt.Fprintf(r.cmd.OutOrStdout(), format+"\n", args...)
}

func (c commands) Commands(conn *connect.Client) []*cobra.Command {
	service := sessionsv1connect.NewSessionServiceClient(conn)
	at := func(cmd *cobra.Command) *run { return &run{cmd: cmd, service: service, conn: conn, app: c.app} }
	sessions := &cobra.Command{
		Use:   "sessions",
		Short: "Manage session history (list, rename, export, prune, delete)",
		Long:  "View and manage the SQLite session store",
		Args:  cobra.NoArgs,
	}
	for _, add := range []func(*cobra.Command, func(*cobra.Command) *run){
		addList, addExport, addDelete, addPrune, addArchive, addOptimize, addCleanMarkers, addOptimizeStorage, addRepair, addJournalMode,
		addRepairRouting, addRepairProfiles, addRepairPrompts, addRefreshPrompts, addRecover, addStats, addRename, addDiscard, addAdopt, addPins,
		addRetitleSkills, addBrowse, addImport,
	} {
		add(sessions, at)
	}
	return []*cobra.Command{sessions}
}

// Text when an id names no session
func notFound(raw string) error {
	return fmt.Errorf("No session '%s'. Run: icebath sessions list to find the id.", raw)
}

// Session an id or the unique start of one names
func (r *run) resolve(raw string) (*sessionsv1.Session, error) {
	response, err := r.service.Resolve(r.ctx(), &sessionsv1.ResolveRequest{Ref: strings.TrimSpace(raw)})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, notFound(raw)
	}
	if err != nil {
		return nil, err
	}
	return response.GetSession(), nil
}

// Whether the person answers yes: declined says the text and goes on with nothing done, a closed stdin with no terminal tells how to
// run it unattended
func (r *run) confirm(question, declined, unattended string) (bool, error) {
	c := cli.NewConsole(r.cmd)
	answer, ok := c.Answer(question)
	if !ok {
		r.print("")
		if !c.Interactive() {
			hint := " Run it from an interactive terminal."
			if unattended != "" {
				hint = " Pass " + unattended + " to run it unattended."
			}
			return false, errors.New("No answer on a non-interactive stdin; nothing was changed." + hint)
		}
		r.print(declined)
		return false, nil
	}
	if answer = strings.ToLower(answer); answer == "y" || answer == "yes" {
		return true, nil
	}
	r.print(declined)
	return false, nil
}

// How long ago a time was, as listings show it
func ago(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

func padLeft(text string, width int) string {
	if n := utf8.RuneCountInString(text); n < width {
		return strings.Repeat(" ", width-n) + text
	}
	return text
}

func updated(s *sessionsv1.Session) time.Time {
	if s.GetUpdated() == nil {
		return time.Time{}
	}
	return s.GetUpdated().AsTime()
}

// Source as listings show it: where it was made when that differs from where it lives
func source(s *sessionsv1.Session) string {
	if s.GetCreatedSource() != "" && s.GetCreatedSource() != s.GetSource() {
		return s.GetCreatedSource() + "→" + s.GetSource()
	}
	return s.GetSource()
}

func workspace(s *sessionsv1.Session) string {
	key := strings.TrimRight(cmp.Or(s.GetGitRoot(), s.GetDirectory()), `/\`)
	if key == "" {
		return "—"
	}
	return strs.Head(cmp.Or(filepath.Base(key), key), 16)
}

func addList(parent *cobra.Command, at func(*cobra.Command) *run) {
	var sourceFlag, workspaceFlag string
	var limit int
	list := &cobra.Command{Use: "list", Short: "List recent sessions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		r := at(cmd)
		filter := &sessionsv1.Filter{Limit: int32(limit), Source: sourceFlag, Workspace: workspaceFlag, ByStart: true}
		if sourceFlag == "" {
			filter.ExcludeSources = []string{"tool"}
		}
		response, err := r.service.List(r.ctx(), &sessionsv1.ListRequest{Filter: filter})
		if err != nil {
			return err
		}
		rows := response.GetSessions()
		if len(rows) == 0 {
			r.print("No sessions found.")
			return nil
		}
		hasWS, hasTitles := workspaceFlag != "", false
		for _, s := range rows {
			hasWS = hasWS || s.GetGitRoot() != "" || s.GetDirectory() != ""
			hasTitles = hasTitles || s.GetTitle() != ""
		}
		title := func(s *sessionsv1.Session, n int) string { return strs.Head(cmp.Or(s.GetTitle(), "—"), n) }
		switch {
		case hasWS && hasTitles:
			r.print(cli.Pad("Title", 28) + " " + cli.Pad("Workspace", 18) + " " + cli.Pad("Last Active", 13) + " ID")
			r.print(strings.Repeat("─", 110))
			for _, s := range rows {
				r.print(cli.Pad(title(s, 26), 28) + " " + cli.Pad(workspace(s), 18) + " " + cli.Pad(ago(updated(s)), 13) + " " + s.GetId())
			}
		case hasWS:
			r.print(cli.Pad("Preview", 38) + " " + cli.Pad("Workspace", 18) + " " + cli.Pad("Last Active", 13) + " " + cli.Pad("Src", 16) + " ID")
			r.print(strings.Repeat("─", 110))
			for _, s := range rows {
				r.print(cli.Pad(strs.Head(s.GetPreview(), 36), 38) + " " + cli.Pad(workspace(s), 18) + " " + cli.Pad(ago(updated(s)), 13) + " " + cli.Pad(source(s), 16) + " " + s.GetId())
			}
		case hasTitles:
			r.print(cli.Pad("Title", 32) + " " + cli.Pad("Preview", 40) + " " + cli.Pad("Last Active", 13) + " ID")
			r.print(strings.Repeat("─", 110))
			for _, s := range rows {
				r.print(cli.Pad(title(s, 30), 32) + " " + cli.Pad(strs.Head(s.GetPreview(), 38), 40) + " " + cli.Pad(ago(updated(s)), 13) + " " + s.GetId())
			}
		default:
			r.print(cli.Pad("Preview", 50) + " " + cli.Pad("Last Active", 13) + " " + cli.Pad("Src", 16) + " ID")
			r.print(strings.Repeat("─", 105))
			for _, s := range rows {
				r.print(cli.Pad(strs.Head(s.GetPreview(), 48), 50) + " " + cli.Pad(ago(updated(s)), 13) + " " + cli.Pad(source(s), 16) + " " + s.GetId())
			}
		}
		if response.GetMore() {
			r.printf("  … more not shown (use --limit %d to see more)", 2*limit)
		}
		return nil
	}}
	list.Flags().StringVar(&sourceFlag, "source", "", "Filter by source (cli, telegram, discord, etc.)")
	list.Flags().IntVar(&limit, "limit", 20, "Max sessions to show")
	list.Flags().StringVar(&workspaceFlag, "workspace", "", "Only sessions in one workspace: a git repo root or project dir (matched by path substring or basename).")
	parent.AddCommand(list)
}

func addDelete(parent *cobra.Command, at func(*cobra.Command) *run) {
	var yes bool
	command := &cobra.Command{Use: "delete <session_id>", Short: "Delete a specific session", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r := at(cmd)
		session, err := r.resolve(args[0])
		if err != nil {
			return err
		}
		id := session.GetId()
		note := ""
		if session.GetPinned() {
			note = " (this session is PINNED)"
		}
		if !yes {
			ok, err := r.confirm(fmt.Sprintf("Delete session '%s'%s and all its messages? [y/N] ", id, note), "Cancelled.", "--yes")
			if err != nil || !ok {
				return err
			}
		} else if session.GetPinned() {
			r.printf("Warning: deleting a pinned session '%s'.", id)
		}
		if _, err := r.service.Delete(r.ctx(), &sessionsv1.DeleteRequest{Id: id}); err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				return notFound(args[0])
			}
			return errors.New(tui.Reason(err))
		}
		r.printf("Deleted session '%s'.", id)
		return nil
	}}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	parent.AddCommand(command)
}

func addRename(parent *cobra.Command, at func(*cobra.Command) *run) {
	parent.AddCommand(&cobra.Command{Use: "rename <session_id> <title...>", Short: "Set or change a session's title", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			session, err := r.resolve(args[0])
			if err != nil {
				return err
			}
			title := strings.Join(args[1:], " ")
			switch {
			case strings.TrimSpace(title) == "":
				return errors.New("Error: title cannot be empty or whitespace-only.")
			case strings.ContainsAny(title, "\r\n"):
				return errors.New("Error: title cannot contain newlines.")
			}
			if _, err := r.service.Rename(r.ctx(), &sessionsv1.RenameRequest{Id: session.GetId(), Title: title}); err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return notFound(args[0])
				}
				return errors.New("Error: " + tui.Reason(err))
			}
			r.printf("Session '%s' renamed to: %s", session.GetId(), title)
			return nil
		}})
}

func addStats(parent *cobra.Command, at func(*cobra.Command) *run) {
	parent.AddCommand(&cobra.Command{Use: "stats", Short: "Show session store statistics", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		r := at(cmd)
		stats, err := r.service.Stats(r.ctx(), &sessionsv1.StatsRequest{})
		if err != nil {
			return fmt.Errorf("Could not open your session history database. Run: icebath sessions repair to fix it (a backup is made first).\nDetails: %s", tui.Reason(err))
		}
		r.printf("Total sessions: %d", stats.GetSessions())
		r.printf("Total messages: %d", stats.GetMessages())
		for _, count := range stats.GetSources() {
			if count.GetSessions() > 0 {
				r.printf("  %s: %d sessions", count.GetSource(), count.GetSessions())
			}
		}
		r.printf("Database size: %.1f MB", float64(stats.GetBytes())/(1<<20))
		if stats.GetPath() != "" {
			r.printf("Database file: %s (journal_mode=%s)", stats.GetPath(), stats.GetJournalMode())
		}
		if indexing := stats.GetIndexing(); indexing != nil {
			r.printf("Search index: rebuilding (%d%%, %d of %d messages); searches scan the rest meanwhile", indexing.GetPercent(),
				indexing.GetIndexed(), indexing.GetTotal())
		}
		return nil
	}})
}

func addPins(parent *cobra.Command, at func(*cobra.Command) *run) {
	pin := func(on bool) func(cmd *cobra.Command, args []string) error {
		return func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			failed := false
			for _, raw := range args {
				session, err := r.resolve(raw)
				if err == nil {
					_, err = r.service.Mark(r.ctx(), &sessionsv1.MarkRequest{Id: session.GetId(), Pinned: &on})
				}
				if err != nil {
					r.print(notFound(raw).Error())
					failed = true
					continue
				}
				verb := "Pinned"
				if !on {
					verb = "Unpinned"
				}
				line := fmt.Sprintf("%s session '%s'.", verb, session.GetId())
				if session.GetTitle() != "" {
					line += "  (" + session.GetTitle() + ")"
				}
				r.print(line)
			}
			if failed {
				return errors.New("Some sessions were not found.")
			}
			return nil
		}
	}
	parent.AddCommand(&cobra.Command{Use: "pin <session_ids...>", Short: "Pin session(s) — durable keep flag, exempt from auto-archive",
		Long: "Set the durable 'keep' flag on one or more sessions. Pinned sessions are exempt from the sessions.auto_archive stale sweep and always appear in listings.",
		Args: cobra.MinimumNArgs(1), RunE: pin(true)})
	parent.AddCommand(&cobra.Command{Use: "unpin <session_ids...>", Short: "Remove the pin (durable keep flag) from session(s)", Args: cobra.MinimumNArgs(1),
		RunE: pin(false)})
	var asJSON bool
	pinned := &cobra.Command{Use: "pinned", Short: "List pinned sessions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		r := at(cmd)
		response, err := r.service.List(r.ctx(), &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{OnlyPinned: true, IncludeArchived: true, IncludeEmpty: true}})
		if err != nil {
			return err
		}
		rows := response.GetSessions()
		if asJSON {
			type entry struct {
				ID           string   `json:"id"`
				Title        *string  `json:"title"`
				Source       string   `json:"source"`
				LastActive   *float64 `json:"last_active"`
				MessageCount int64    `json:"message_count"`
			}
			out := []entry{}
			for _, s := range rows {
				e := entry{ID: s.GetId(), Source: s.GetSource(), MessageCount: s.GetMessages()}
				if s.GetTitle() != "" {
					title := s.GetTitle()
					e.Title = &title
				}
				if s.GetUpdated() != nil {
					seconds := float64(s.GetUpdated().AsTime().UnixNano()) / 1e9
					e.LastActive = &seconds
				}
				out = append(out, e)
			}
			data, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return err
			}
			r.print(string(data))
			return nil
		}
		if len(rows) == 0 {
			r.print("No pinned sessions. Pin one with: icebath sessions pin <session_id>")
			return nil
		}
		r.print(cli.Pad("Title", 32) + " " + cli.Pad("Last Active", 13) + " " + cli.Pad("Src", 9) + " ID")
		r.print(strings.Repeat("─", 100))
		for _, s := range rows {
			r.print(cli.Pad(strs.Head(cmp.Or(s.GetTitle(), s.GetPreview(), "—"), 30), 32) + " " + cli.Pad(ago(updated(s)), 13) + " " + cli.Pad(cmp.Or(s.GetSource(), "-"), 9) + " " +
				s.GetId())
		}
		return nil
	}}
	pinned.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON (for backup/restore scripting)")
	parent.AddCommand(pinned)
}

func addDiscard(parent *cobra.Command, at func(*cobra.Command) *run) {
	var admissions []string
	var yes bool
	command := &cobra.Command{Use: "discard <session>", Short: "Acknowledge turns lost in a server crash (unknown admissions) so the session runs again",
		Long: "After icebath stopped mid-turn, the lost turn is fenced 'unknown' and blocks the session until it is acknowledged. This is the " +
			"non-interactive form of `/discard <admission>`. The lost input is not replayed.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			lost, err := r.service.Lost(r.ctx(), &sessionsv1.LostRequest{Session: args[0]})
			if err != nil {
				return errors.New("Error: " + tui.Reason(err))
			}
			var chosen []string
			for _, admission := range lost.GetAdmissions() {
				if len(admissions) == 0 || slices.Contains(admissions, admission.GetId()) {
					chosen = append(chosen, admission.GetId())
				}
			}
			var unknown []string
			for _, id := range admissions {
				if !slices.Contains(chosen, id) {
					unknown = append(unknown, id)
				}
			}
			if len(unknown) > 0 {
				slices.Sort(unknown)
				return errors.New("Error: Not an unknown (lost) admission of this session: " + strings.Join(unknown, ", "))
			}
			if len(chosen) == 0 {
				r.printf("Nothing to discard: session %s has no unknown admission.", lost.GetSessionId())
				return nil
			}
			if !yes {
				if !term.IsTerminal(os.Stdin.Fd()) {
					return errors.New("Refusing to discard without confirmation on a non-interactive stdin; pass --yes.")
				}
				r.printf("Discard %d lost turn(s) on session %s? They are acknowledged, not replayed.", len(chosen), lost.GetSessionId())
				for _, id := range chosen {
					r.print("  " + id)
				}
				ok, err := r.confirm("Proceed? [y/N] ", "Cancelled; nothing was discarded.", "--yes")
				if err != nil || !ok {
					return err
				}
			}
			response, err := r.service.Discard(r.ctx(), &sessionsv1.DiscardRequest{Session: lost.GetSessionId(), Admissions: chosen})
			if err != nil {
				return errors.New("Error: " + tui.Reason(err))
			}
			for _, id := range response.GetDiscarded() {
				r.print("Discarded " + id)
			}
			return nil
		}}
	command.Flags().StringArrayVar(&admissions, "admission", nil, "Discard only this unknown admission (repeatable; default: every unknown admission)")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation (required when stdin is not a TTY)")
	parent.AddCommand(command)
}

func addAdopt(parent *cobra.Command, at func(*cobra.Command) *run) {
	var dryRun bool
	command := &cobra.Command{Use: "adopt", Short: "Bring this profile's chats under the server that serves them",
		Long: "Chats written before a server owned sessions become the server's the first time a client opens them; this adopts them all at once, so " +
			"every client can continue them. Here the server owns every stored session from the moment it is made, so there is never anything left over.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if _, err := r.service.Stats(r.ctx(), &sessionsv1.StatsRequest{}); err != nil {
				return errors.New("Error: could not read the session store (" + tui.Reason(err) + "). Run `icebath sessions repair` to check it.")
			}
			r.print("Nothing to adopt: every local chat in this profile already belongs to its server.")
			if dryRun {
				r.print("Dry run: nothing was adopted.")
			}
			return nil
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Only count what would be adopted")
	parent.AddCommand(command)
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

func addRepairRouting(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply bool
	var gap float64
	command := &cobra.Command{Use: "repair-routing", Short: "Re-stamp gateway sessions that lost their routing identity",
		Long: "Find gateway conversations stranded in session rows whose routing identity (session_key/chat_id/origin) was never written. Such a row " +
			"is invisible to restart recovery, so the chat resumes an older session instead. Re-stamps each orphan from the keyed predecessor it " +
			"continues, and only when that predecessor is unambiguous. Reports without touching the database unless --apply is given.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			report, err := r.service.RepairRouting(r.ctx(), &sessionsv1.RepairRoutingRequest{MaxGapSeconds: gap})
			if err != nil {
				return err
			}
			orphans := report.GetOrphans()
			if len(orphans) == 0 {
				r.print("✓ No gateway sessions are missing their routing identity.")
				return nil
			}
			adoptable := 0
			for _, o := range orphans {
				r.printf("%s  (%s, %d messages)", o.GetId(), o.GetSource(), o.GetMessages())
				if o.GetSessionKey() != "" {
					adoptable++
					r.printf("  → adopt into %s (from %s, evidence: %s)", o.GetSessionKey(), o.GetDonorId(), o.GetEvidence())
				} else {
					r.printf("  ✗ not repairable — %s", o.GetReason())
				}
			}
			switch {
			case adoptable == 0:
				r.printf("\n%d orphaned session(s) found, none unambiguously repairable. Nothing to do.", len(orphans))
				return nil
			case !apply:
				r.printf("\n%d of %d orphaned session(s) can be repaired. Re-run with --apply to perform them.", adoptable, len(orphans))
				return nil
			}
			r.print("\nStop the gateway before applying — a running gateway still holds the old routing mapping in memory.")
			ok, err := r.confirm(fmt.Sprintf("Adopt %d orphaned session(s)? [y/N] ", adoptable), "Aborted — nothing was changed.", "")
			if err != nil || !ok {
				return err
			}
			applied, err := r.service.RepairRouting(r.ctx(), &sessionsv1.RepairRoutingRequest{Apply: true, MaxGapSeconds: gap})
			if err != nil {
				return err
			}
			repaired := 0
			for _, o := range applied.GetOrphans() {
				if o.GetSessionKey() == "" {
					continue
				}
				if o.GetRepaired() {
					repaired++
					r.printf("✓ %s now owns %s", o.GetId(), o.GetSessionKey())
				} else {
					r.printf("✗ %s was not adopted (the row changed since it was reported)", o.GetId())
				}
			}
			r.printf("\nRepaired %d of %d session(s).", repaired, adoptable)
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Perform the adoptions (default: report only)")
	command.Flags().Float64Var(&gap, "max-gap-seconds", 0, "Window between a keyed predecessor's last activity and an orphan's start for them to count as the same conversation (default: 900)")
	parent.AddCommand(command)
}

var profileLabels = map[string]string{
	"mislabelled":         "profile_name disagrees with the row's own session key",
	"unclaimed_namespace": "rows keyed to a profile that does not exist",
	"crossed_parent":      "parent_session_id crossing profile namespaces",
	"legacy_main":         "legacy agent:main rows held by a named profile",
	"routing_unclaimed":   "routing rows for a profile that does not exist",
}

func addRepairProfiles(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply, asJSON, yes bool
	var legacy string
	command := &cobra.Command{Use: "repair-profiles", Short: "Settle session/routing state that landed under the wrong profile",
		Long: "Scan the session store for durable state crossed between profiles: rows whose profile disagrees with their session key, rows held by a " +
			"profile that no longer exists, parent links crossing profiles, and routing rows for a profile that no longer exists. Reports without " +
			"touching anything unless --apply is given; --apply backs the store up first and is safe to re-run.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if legacy != "report" && legacy != "rekey" && legacy != "move" {
				return errors.New("--legacy-main must be report, rekey or move")
			}
			report, err := r.service.RepairProfiles(r.ctx(), &sessionsv1.RepairProfilesRequest{LegacyMain: legacy})
			if err != nil {
				return err
			}
			findings := report.GetFindings()
			repairable := 0
			for _, f := range findings {
				if f.GetAction() != "" {
					repairable++
				}
			}
			type finding struct {
				Kind    string `json:"kind"`
				Store   string `json:"store"`
				Subject string `json:"subject"`
				Detail  string `json:"detail"`
				Action  string `json:"action"`
				Reason  string `json:"reason"`
			}
			list := []finding{}
			for _, f := range findings {
				list = append(list, finding{f.GetKind(), "default", f.GetSubject(), f.GetDetail(), f.GetAction(), f.GetReason()})
			}
			if asJSON && !apply {
				data, _ := json.MarshalIndent(map[string]any{"stores": []string{"default"}, "findings": list, "repairable": repairable}, "", "  ")
				r.print(string(data))
				return nil
			}
			if !asJSON {
				r.print("Scanned 1 profile store(s): default")
				if len(findings) == 0 {
					r.print("✓ No crossed-profile state found.")
				}
				var kinds []string
				for _, f := range findings {
					if !slices.Contains(kinds, f.GetKind()) {
						kinds = append(kinds, f.GetKind())
					}
				}
				for _, kind := range kinds {
					var group []*sessionsv1.ProfileFinding
					for _, f := range findings {
						if f.GetKind() == kind {
							group = append(group, f)
						}
					}
					r.printf("\n%s (%d):", cmp.Or(profileLabels[kind], kind), len(group))
					for _, f := range group {
						r.printf("  [default] %s\n      %s", f.GetSubject(), f.GetDetail())
						switch {
						case f.GetAction() != "":
							r.print("      → " + f.GetAction())
						case f.GetReason() != "":
							r.print("      ✗ not repaired — " + f.GetReason())
						default:
							r.print("      ✗ not repaired")
						}
					}
				}
			}
			if !apply {
				if len(findings) > 0 {
					r.printf("\n%d of %d finding(s) can be repaired. Re-run with --apply to perform them.", repairable, len(findings))
				}
				return nil
			}
			if repairable == 0 {
				if asJSON {
					data, _ := json.Marshal(map[string]any{"findings": list, "applied": map[string]any{}})
					r.print(string(data))
				} else {
					r.print("\nNothing to repair.")
				}
				return nil
			}
			if !yes && !asJSON {
				ok, err := r.confirm(fmt.Sprintf("\nApply %d repair(s)? A snapshot of every store is taken first. [y/N] ", repairable), "Aborted — nothing was changed.", "--yes")
				if err != nil || !ok {
					return err
				}
			}
			applied, err := r.service.RepairProfiles(r.ctx(), &sessionsv1.RepairProfilesRequest{Apply: true, LegacyMain: legacy})
			if err != nil {
				return err
			}
			if asJSON {
				data, _ := json.MarshalIndent(map[string]any{"findings": list, "applied": map[string]any{"snapshots": map[string]string{"default": applied.GetBackup()},
					"totals": applied.GetApplied(), "failures": applied.GetFailures()}}, "", "  ")
				r.print(string(data))
			} else {
				r.printf("  snapshot [default]: %s", cmp.Or(applied.GetBackup(), "FAILED"))
				var totals []string
				for _, key := range slices.Sorted(mapsKeys(applied.GetApplied())) {
					totals = append(totals, fmt.Sprintf("%s=%d", key, applied.GetApplied()[key]))
				}
				r.printf("\nApplied: %s", cmp.Or(strings.Join(totals, ", "), "nothing"))
				for _, failure := range applied.GetFailures() {
					r.print("  ✗ " + failure)
				}
				r.print("Re-run without --apply to confirm the stores are clean.")
			}
			if len(applied.GetFailures()) > 0 {
				return errors.New("Some repairs failed.")
			}
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Perform the repairs (default: report only)")
	command.Flags().BoolVar(&asJSON, "json", false, "Machine-readable report")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	command.Flags().StringVar(&legacy, "legacy-main", "report", "What to do with rows keyed to the legacy main namespace held by a named profile: 'rekey' them to that profile, "+
		"'move' them to the default profile, or 'report' (default)")
	parent.AddCommand(command)
}

func addRepairPrompts(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply, asJSON bool
	command := &cobra.Command{Use: "repair-prompts [session_id]", Short: "Report or clear stored system prompts degraded to a reduced-toolset build",
		Long: "Find stored system prompts that lack the guidance the tools they were built with call for, so a session would go on with a degraded prompt. " +
			"Rows without a readable tools[] pin are reported as unverifiable and never changed by a scan. Clearing a prompt makes the next turn rebuild " +
			"and store a healthy one. Reports without touching anything unless --apply is given; a session_id is an explicit destructive override and can " +
			"clear even a healthy prompt.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			report, err := r.service.RepairPrompts(r.ctx(), &sessionsv1.RepairPromptsRequest{SessionId: target})
			if err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return fmt.Errorf("No session matches %q.", target)
				}
				return err
			}
			payload := func(cleared []string) string {
				if cleared == nil {
					cleared = []string{}
				}
				data, _ := json.MarshalIndent(map[string]any{"findings": report.GetFindings(), "unverifiable": report.GetUnverifiable(), "apply": apply,
					"cleared": cleared}, "", "  ")
				return string(data)
			}
			if len(report.GetFindings()) == 0 {
				switch {
				case asJSON:
					r.print(payload(nil))
				case report.GetEmpty():
					r.printf("%s already has no stored prompt; nothing to clear.", report.GetSessionId())
				default:
					r.print("No degraded stored system prompts found.")
					if n := len(report.GetUnverifiable()); n > 0 {
						r.printf("%d row(s) lacked enough tools[] evidence and were left unchanged.", n)
					}
				}
				return nil
			}
			if !asJSON {
				for _, f := range report.GetFindings() {
					r.printf("  %s  (%d chars) - %s", f.GetId(), f.GetPromptChars(), f.GetReason())
				}
				if n := len(report.GetUnverifiable()); n > 0 {
					r.printf("\nSkipped %d unverifiable row(s) without enough tools[] evidence.", n)
				}
			}
			if !apply {
				if asJSON {
					r.print(payload(nil))
				} else {
					r.printf("\n%d stored prompt(s) can be repaired. Re-run with --apply to clear them; the next turn rebuilds a healthy prompt (one "+
						"prefix-cache break per session).", len(report.GetFindings()))
				}
				return nil
			}
			if !asJSON {
				ok, err := r.confirm(fmt.Sprintf("Clear %d stored prompt(s)? [y/N] ", len(report.GetFindings())), "Aborted - nothing was changed.", "--json")
				if err != nil || !ok {
					return err
				}
			}
			applied, err := r.service.RepairPrompts(r.ctx(), &sessionsv1.RepairPromptsRequest{SessionId: report.GetSessionId(), Apply: true})
			if err != nil {
				return err
			}
			if asJSON {
				r.print(payload(applied.GetCleared()))
				return nil
			}
			r.printf("\nCleared %d stored prompt(s); the next turn for each rebuilds and persists a healthy prompt.", len(applied.GetCleared()))
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Clear the verified prompts (default: report only)")
	command.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output; with --apply, apply without an interactive confirmation")
	parent.AddCommand(command)
}

func addRefreshPrompts(parent *cobra.Command, at func(*cobra.Command) *run) {
	var session string
	var asJSON bool
	command := &cobra.Command{Use: "refresh-prompts", Short: "Make continuing sessions rebuild their system prompt on their next turn",
		Long: "After updating SOUL.md, skills or context files, ask every continuing session (or one with --session) to rebuild its stored system prompt " +
			"once, on its next turn, from the current sources. Conversation history and session ids are kept; each refreshed session pays one prompt-cache " +
			"miss. Works while a server runs and across restarts.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			response, err := r.service.RefreshPrompts(r.ctx(), &sessionsv1.RefreshPromptsRequest{SessionId: session})
			if err != nil {
				return errors.New(tui.Reason(err))
			}
			if asJSON {
				var id any
				if response.GetSessionId() != "" {
					id = response.GetSessionId()
				}
				data, err := json.Marshal(struct {
					SessionID any   `json:"session_id"`
					Sessions  int64 `json:"sessions"`
				}{id, response.GetSessions()})
				if err != nil {
					return err
				}
				r.print(string(data))
				return nil
			}
			scope := cmp.Or(response.GetSessionId(), "every session")
			r.printf("Requested a system prompt refresh for %s (%d session(s) with a stored prompt).\nEach rebuilds its prompt from the current SOUL.md, "+
				"context files and settings on its next turn (one prompt-cache miss per session); conversation history is kept.", scope, response.GetSessions())
			return nil
		}}
	command.Flags().StringVar(&session, "session", "", "Refresh only this session (id or unique prefix)")
	command.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	parent.AddCommand(command)
}

func addRetitleSkills(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply bool
	var limit int
	command := &cobra.Command{Use: "retitle-skills", Short: "Re-title sessions whose auto-title came from a /skill's own text",
		Long: "Sessions opened with a /skill were auto-titled from the expanded message, which embeds the whole skill body — so the title describes the " +
			"SKILL, not the request. This regenerates those titles from what the user actually typed. Lists what it would change unless --apply is passed.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if limit < 1 {
				return errors.New("--limit must be at least 1")
			}
			response, err := r.service.RetitleSkills(r.ctx(), &sessionsv1.RetitleSkillsRequest{Apply: apply, Limit: int32(limit)})
			if err != nil {
				return err
			}
			if response.GetCandidates() == 0 {
				r.print("No sessions were titled from a /skill invocation.")
				return nil
			}
			header := fmt.Sprintf("%d session(s) opened with a /skill", response.GetCandidates())
			if !apply {
				header += " (dry run — pass --apply to write)"
			}
			r.print(header + ":")
			changed := 0
			for _, retitle := range response.GetRetitles() {
				if retitle.GetKept() {
					r.printf("  %s\n    kept %s — got %s", retitle.GetId(), retitle.GetOldTitle(), retitle.GetNewTitle())
					continue
				}
				r.printf("  %s\n    %s\n    → %s", retitle.GetId(), retitle.GetOldTitle(), retitle.GetNewTitle())
				switch {
				case retitle.GetError() != "":
					r.print("    skipped: " + retitle.GetError())
				case retitle.GetRenamed() != "":
					r.printf("    (renamed to %s — title was taken)", retitle.GetRenamed())
					changed++
				default:
					changed++
				}
			}
			switch {
			case changed == 0:
				r.print("  every title already reflects the user's request.")
			case apply:
				r.printf("✓ Re-titled %d session(s).", changed)
			}
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Write the new titles (default: dry run)")
	command.Flags().IntVar(&limit, "limit", 200, "Maximum sessions to examine (default: 200)")
	parent.AddCommand(command)
}
