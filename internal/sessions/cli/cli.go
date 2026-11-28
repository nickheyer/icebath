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
	picked "github.com/nickheyer/icebath/internal/profile"
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
		Use:     "sessions",
		GroupID: cli.GroupSessions,
		Short:   "List, rename, export and clean up your sessions",
		Long: "A session is one conversation with the agent, kept with its history. Use these commands to find, rename, export, archive and " +
			"delete sessions, and to keep the session store healthy.",
		Args: cobra.NoArgs,
	}
	for _, add := range []func(*cobra.Command, func(*cobra.Command) *run){
		addList, addSearch, addExport, addDelete, addPrune, addArchive, addOptimize, addCleanMarkers, addOptimizeStorage, addRepair, addJournalMode,
		addRepairRouting, addRepairProfiles, addRepairPrompts, addRefreshPrompts, addRecover, addStats, addRename, addDiscard, addAdopt, addPins,
		addRetitleSkills, addBrowse, addImport,
	} {
		add(sessions, at)
	}
	return []*cobra.Command{sessions}
}

// Searches what was written in the stored sessions for words: each match with its session, when it was written, who wrote it and the
// words around it, the newest first
func addSearch(parent *cobra.Command, at func(*cobra.Command) *run) {
	var limit int
	var oldest bool
	search := &cobra.Command{Use: "search <words...>", Short: "Search the messages of your sessions",
		Long: "Searches what you and the agent wrote in every stored session and prints each match: its session, when it was written, who " +
			"wrote it and the words around it. With -p, only the sessions of that profile are searched. Go on in a session a match is in " +
			"with icebath chat -r and its id.",
		Example: "  icebath sessions search login bug\n  icebath sessions search --limit 5 \"rate limit\"",
		Args:    cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			order := "newest"
			if oldest {
				order = "oldest"
			}
			response, err := r.service.Search(r.ctx(), &sessionsv1.SearchRequest{Query: strings.Join(args, " "), Limit: int32(limit), Sort: order,
				ExcludeSources: []string{"tool"}, Profile: picked.Picked(r.ctx())})
			if err != nil {
				return err
			}
			hits := response.GetHits()
			if len(hits) == 0 {
				r.print("No message of your sessions holds those words.")
				return nil
			}
			c := cli.NewConsole(cmd)
			for i, hit := range hits {
				if i > 0 {
					r.print("")
				}
				who := map[sessionsv1.Role]string{sessionsv1.Role_ROLE_USER: "You", sessionsv1.Role_ROLE_ASSISTANT: "The agent",
					sessionsv1.Role_ROLE_SYSTEM: "The system"}[hit.GetRole()]
				if hit.GetRole() == sessionsv1.Role_ROLE_TOOL {
					who = strs.Filled("The tool "+hit.GetToolName(), "A tool")
				}
				r.print(c.Paint(hit.GetSessionId(), cli.Bold) + c.Paint("  "+shown(hit.GetTime())+" · "+strs.Filled(who, "A message"), cli.Dim))
				// The match is marked by the store between >>> and <<<
				snippet := strings.Join(strings.Fields(hit.GetSnippet()), " ")
				snippet = strings.ReplaceAll(strings.ReplaceAll(snippet, ">>>", "\x00"), "<<<", "\x01")
				var line strings.Builder
				for _, part := range strings.SplitAfter(snippet, "\x01") {
					before, match, marked := strings.Cut(strings.TrimSuffix(part, "\x01"), "\x00")
					line.WriteString(before)
					if marked {
						line.WriteString(c.Paint(match, cli.Bold))
					}
				}
				r.print("  " + line.String())
			}
			return nil
		}}
	search.Flags().IntVar(&limit, "limit", 20, "The most matches to print")
	search.Flags().BoolVar(&oldest, "oldest", false, "Print the oldest matches first")
	parent.AddCommand(search)
}

// Text when an id names no session
func notFound(raw string) error {
	return fmt.Errorf("There is no session %s. Run icebath sessions list to find its id.", raw)
}

// Prints labels and their values, each value after its label padded to the widest
func (r *run) facts(pairs [][2]string) {
	width := 0
	for _, pair := range pairs {
		width = max(width, cli.Width(pair[0])+1)
	}
	for _, pair := range pairs {
		r.print(cli.Pad(pair[0]+":", width) + "  " + pair[1])
	}
}

// Session as a sentence names it: its title in quotes, else its id
func named(s *sessionsv1.Session) string {
	if s.GetTitle() != "" {
		return "“" + s.GetTitle() + "”"
	}
	return s.GetId()
}

// Text cut to n characters, an ellipsis ending it when it was longer
func clip(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return strs.Head(text, n-1) + "…"
}

// Session an id or the unique start of one names
func (r *run) resolve(raw string) (*sessionsv1.Session, error) {
	response, err := r.service.Resolve(r.ctx(), &sessionsv1.ResolveRequest{Profile: picked.Picked(r.ctx()), Ref: strings.TrimSpace(raw)})
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
			hint := " Run it in a terminal."
			if unattended != "" {
				hint = " Pass " + unattended + " to run it without asking."
			}
			return false, errors.New("Nothing changed, because there is no terminal to answer in." + hint)
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
		return "Unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "Just now"
	case d < time.Hour:
		return strs.Count(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return strs.Count(int(d.Hours()), "hour") + " ago"
	case d < 48*time.Hour:
		return "Yesterday"
	case d < 7*24*time.Hour:
		return strs.Count(int(d.Hours()/24), "day") + " ago"
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
		return s.GetSource() + ", started in " + s.GetCreatedSource()
	}
	return cmp.Or(s.GetSource(), "Unknown")
}

func workspace(s *sessionsv1.Session) string {
	key := strings.TrimRight(cmp.Or(s.GetGitRoot(), s.GetDirectory()), `/\`)
	if key == "" {
		return "None"
	}
	return cmp.Or(filepath.Base(key), key)
}

func addList(parent *cobra.Command, at func(*cobra.Command) *run) {
	var sourceFlag, workspaceFlag string
	var limit int
	list := &cobra.Command{Use: "list", Short: "List your recent sessions",
		Long: "Lists the most recent sessions with their title, where they came from, their folder and when they were last used.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
				if sourceFlag != "" || workspaceFlag != "" {
					r.print("No session matches. Run icebath sessions list without --source and --workspace to see every session.")
				} else {
					r.print("You have no sessions yet. Run icebath to start one.")
				}
				return nil
			}
			hasWS, hasTitles := workspaceFlag != "", false
			for _, s := range rows {
				hasWS = hasWS || s.GetGitRoot() != "" || s.GetDirectory() != ""
				hasTitles = hasTitles || s.GetTitle() != ""
			}
			title := func(s *sessionsv1.Session) string { return cmp.Or(s.GetTitle(), "Untitled") }
			preview := func(s *sessionsv1.Session, n int) string { return clip(cmp.Or(s.GetPreview(), "No messages yet"), n) }
			var headers []string
			var table [][]string
			switch {
			case hasWS && hasTitles:
				headers = []string{"Title", "Project", "Last used", "ID"}
				for _, s := range rows {
					table = append(table, []string{title(s), workspace(s), ago(updated(s)), s.GetId()})
				}
			case hasWS:
				headers = []string{"Preview", "Project", "Last used", "Source", "ID"}
				for _, s := range rows {
					table = append(table, []string{preview(s, 36), workspace(s), ago(updated(s)), source(s), s.GetId()})
				}
			case hasTitles:
				headers = []string{"Title", "Preview", "Last used", "ID"}
				for _, s := range rows {
					table = append(table, []string{title(s), preview(s, 38), ago(updated(s)), s.GetId()})
				}
			default:
				headers = []string{"Preview", "Last used", "Source", "ID"}
				for _, s := range rows {
					table = append(table, []string{preview(s, 48), ago(updated(s)), source(s), s.GetId()})
				}
			}
			cli.NewConsole(cmd).Table(headers, table)
			if response.GetMore() {
				r.printf("\nMore sessions are not shown. Add --limit %s to see more.", strs.Number(float64(2*limit)))
			}
			return nil
		}}
	list.Flags().StringVar(&sourceFlag, "source", "", "Only sessions from this source, such as cli or telegram")
	list.Flags().IntVar(&limit, "limit", 20, "Largest number of sessions to show")
	list.Flags().StringVar(&workspaceFlag, "workspace", "", "Only sessions of one project, matched by part of its path or its name")
	parent.AddCommand(list)
}

func addDelete(parent *cobra.Command, at func(*cobra.Command) *run) {
	var yes bool
	command := &cobra.Command{Use: "delete <session_id>", Short: "Delete a session",
		Long: "Deletes the session and its history, after asking. The id may be the start of one, as long as no other id starts the same.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			session, err := r.resolve(args[0])
			if err != nil {
				return err
			}
			id := session.GetId()
			if !yes {
				kind := "session"
				if session.GetPinned() {
					kind = "pinned session"
				}
				question := fmt.Sprintf("Delete the %s %s and all its messages? Its history can't be brought back. [y/N] ", kind, named(session))
				ok, err := r.confirm(question, "Nothing was deleted.", "--yes")
				if err != nil || !ok {
					return err
				}
			} else if session.GetPinned() {
				r.printf("⚠ Deleting the pinned session %s.", named(session))
			}
			if _, err := r.service.Delete(r.ctx(), &sessionsv1.DeleteRequest{Id: id}); err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return notFound(args[0])
				}
				return errors.New(strs.Sentence(tui.Reason(err)))
			}
			r.printf("✓ Deleted the session %s.", named(session))
			return nil
		}}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking first")
	parent.AddCommand(command)
}

func addRename(parent *cobra.Command, at func(*cobra.Command) *run) {
	parent.AddCommand(&cobra.Command{Use: "rename <session_id> <title...>", Short: "Set or change the title of a session",
		Long: "Gives the session the title you write after its id.", Example: "  icebath sessions rename 3f2a9c Refactor the parser",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			session, err := r.resolve(args[0])
			if err != nil {
				return err
			}
			title := strings.Join(args[1:], " ")
			switch {
			case strings.TrimSpace(title) == "":
				return errors.New("Give the session a title.")
			case strings.ContainsAny(title, "\r\n"):
				return errors.New("A title must fit on one line.")
			}
			if _, err := r.service.Rename(r.ctx(), &sessionsv1.RenameRequest{Id: session.GetId(), Title: title}); err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return notFound(args[0])
				}
				return errors.New(strs.Sentence(tui.Reason(err)))
			}
			r.printf("✓ The session %s is now called %s.", session.GetId(), title)
			return nil
		}})
}

func addStats(parent *cobra.Command, at func(*cobra.Command) *run) {
	parent.AddCommand(&cobra.Command{Use: "stats", Short: "Show how many sessions are kept and their size",
		Long: "Shows statistics of the session store, such as how many sessions and messages it holds and how much space it takes.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			stats, err := r.service.Stats(r.ctx(), &sessionsv1.StatsRequest{})
			if err != nil {
				return fmt.Errorf("icebath can't open your saved sessions. Run icebath sessions repair to fix them, which makes a backup first.\n%s",
					strs.Sentence(tui.Reason(err)))
			}
			r.print("Your saved sessions\n")
			pairs := [][2]string{{"Sessions", strs.Number(float64(stats.GetSessions()))}, {"Messages", strs.Number(float64(stats.GetMessages()))}}
			for _, count := range stats.GetSources() {
				if count.GetSessions() > 0 {
					pairs = append(pairs, [2]string{"  From " + count.GetSource(), strs.Count(int(count.GetSessions()), "session")})
				}
			}
			pairs = append(pairs, [2]string{"Size", strs.Size(float64(stats.GetBytes()))})
			if stats.GetPath() != "" {
				pairs = append(pairs, [2]string{"File", stats.GetPath()}, [2]string{"Journal mode", stats.GetJournalMode()})
			}
			if indexing := stats.GetIndexing(); indexing != nil {
				pairs = append(pairs, [2]string{"Search index", fmt.Sprintf("Rebuilding, %d%% done, %s of %s messages. Searches read the rest "+
					"directly meanwhile.", indexing.GetPercent(), strs.Number(float64(indexing.GetIndexed())), strs.Number(float64(indexing.GetTotal())))})
			}
			r.facts(pairs)
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
				line := fmt.Sprintf("✓ %s the session %s", verb, session.GetId())
				if session.GetTitle() != "" {
					line += ", “" + session.GetTitle() + "”"
				}
				r.print(line + ".")
			}
			if failed {
				return errors.New("Some of those sessions were not found, as said above.")
			}
			return nil
		}
	}
	parent.AddCommand(&cobra.Command{Use: "pin <session_ids...>", Short: "Pin sessions so they are never archived",
		Long: "Pins one or more sessions. A pinned session is never archived for being idle and always shows in lists.",
		Args: cobra.MinimumNArgs(1), RunE: pin(true)})
	parent.AddCommand(&cobra.Command{Use: "unpin <session_ids...>", Short: "Unpin sessions",
		Long: "Takes the pin off one or more sessions, so they can be archived for being idle again.", Args: cobra.MinimumNArgs(1),
		RunE: pin(false)})
	var asJSON bool
	pinned := &cobra.Command{Use: "pinned", Short: "List your pinned sessions", Long: "Lists every pinned session.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				r.print("You have no pinned sessions. Pin one with icebath sessions pin <session_ids...>.")
				return nil
			}
			var table [][]string
			for _, s := range rows {
				table = append(table, []string{cmp.Or(s.GetTitle(), clip(s.GetPreview(), 40), "Untitled"), ago(updated(s)), cmp.Or(s.GetSource(), "Unknown"),
					s.GetId()})
			}
			cli.NewConsole(cmd).Table([]string{"Title", "Last used", "Source", "ID"}, table)
			return nil
		}}
	pinned.Flags().BoolVar(&asJSON, "json", false, "Print the list as JSON, for scripts that back up or restore pins")
	parent.AddCommand(pinned)
}

func addDiscard(parent *cobra.Command, at func(*cobra.Command) *run) {
	var admissions []string
	var yes bool
	command := &cobra.Command{Use: "discard <session>", Short: "Unblock a session that icebath stopped mid-reply",
		Long: "When icebath stops in the middle of a reply, the message that was lost blocks the session until you discard it. discard does " +
			"that from the command line, as /discard does in a chat. The lost message is not sent again.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			lost, err := r.service.Lost(r.ctx(), &sessionsv1.LostRequest{Session: args[0]})
			if err != nil {
				return errors.New(strs.Sentence(tui.Reason(err)))
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
				return errors.New("These are not lost messages of this session: " + strings.Join(unknown, ", ") + ".")
			}
			if len(chosen) == 0 {
				r.printf("The session %s has no lost message, so there is nothing to discard.", lost.GetSessionId())
				return nil
			}
			if !yes {
				if !term.IsTerminal(os.Stdin.Fd()) {
					return errors.New("Nothing was discarded, because there is no terminal to ask in. Pass --yes to discard without asking.")
				}
				r.printf("The session %s has %s that icebath lost:", lost.GetSessionId(), strs.Count(len(chosen), "lost message"))
				for _, id := range chosen {
					r.print("  " + id)
				}
				ok, err := r.confirm("Discard them? They are marked as handled and never sent again. [y/N] ", "Nothing was discarded.", "--yes")
				if err != nil || !ok {
					return err
				}
			}
			response, err := r.service.Discard(r.ctx(), &sessionsv1.DiscardRequest{Session: lost.GetSessionId(), Admissions: chosen})
			if err != nil {
				return errors.New(strs.Sentence(tui.Reason(err)))
			}
			for _, id := range response.GetDiscarded() {
				r.print("✓ Discarded " + id + ".")
			}
			return nil
		}}
	command.Flags().StringArrayVar(&admissions, "admission", nil, "Lost message to discard by id, instead of all of them, repeatable")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking, which you need when no terminal can ask")
	parent.AddCommand(command)
}

func addAdopt(parent *cobra.Command, at func(*cobra.Command) *run) {
	var dryRun bool
	command := &cobra.Command{Use: "adopt", Short: "Move older sessions under the server that serves them",
		Long: "Sessions saved before a server kept them move to the server the first time a client opens them. adopt moves them all at once, " +
			"so every client can continue them. In this version the server keeps every session from the start, so adopt only checks the " +
			"store and finds nothing to move.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if _, err := r.service.Stats(r.ctx(), &sessionsv1.StatsRequest{}); err != nil {
				return errors.New("icebath can't read your saved sessions: " + strings.TrimSuffix(tui.Reason(err), ".") + ". Run icebath sessions " +
					"repair to check them.")
			}
			r.print("There is nothing to move: every session of this profile already belongs to its server.")
			if dryRun {
				r.print("Nothing was moved, because you passed --dry-run.")
			}
			return nil
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Only count what would move")
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
	command := &cobra.Command{Use: "repair-routing", Short: "Reconnect chat app sessions that lost their route",
		Long: "Finds sessions from chat apps whose route back to the chat was never saved. icebath cannot find such a session after a restart, " +
			"so the chat goes on in an older session instead. repair-routing gives each one the route of the session it continues, but only " +
			"when that session is certain. Nothing changes unless you pass --apply.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			report, err := r.service.RepairRouting(r.ctx(), &sessionsv1.RepairRoutingRequest{MaxGapSeconds: gap})
			if err != nil {
				return err
			}
			orphans := report.GetOrphans()
			if len(orphans) == 0 {
				r.print("✓ Every chat app session has its route back to its chat.")
				return nil
			}
			adoptable := 0
			for _, o := range orphans {
				r.printf("%s, from %s, %s", o.GetId(), o.GetSource(), strs.Count(int(o.GetMessages()), "message"))
				if o.GetSessionKey() != "" {
					adoptable++
					why := "because it started right after that session went quiet"
					if o.GetEvidence() == "lineage" {
						why = "because it continues that session"
					}
					r.printf("  Can take the route %s of the session %s, %s.", o.GetSessionKey(), o.GetDonorId(), why)
				} else {
					r.printf("  ✗ Can't be repaired. %s", strs.Sentence(o.GetReason()))
				}
			}
			switch {
			case adoptable == 0:
				r.printf("\nFound %s without a route, and none can be repaired for certain. Nothing changed.", strs.Count(len(orphans), "session"))
				return nil
			case !apply:
				r.printf("\n%s of %s without a route can be repaired. Run the command again with --apply to repair them.",
					strs.Number(float64(adoptable)), strs.Count(len(orphans), "session"))
				return nil
			}
			r.print("\nStop the connection of icebath to your chat apps first, because while it runs it keeps the old routes in memory.")
			ok, err := r.confirm(fmt.Sprintf("Repair the routes of %s? [y/N] ", strs.Count(adoptable, "session")), "Nothing changed.", "")
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
					r.printf("✓ %s now has the route %s.", o.GetId(), o.GetSessionKey())
				} else {
					r.printf("✗ %s was not repaired, because it changed after the report.", o.GetId())
				}
			}
			r.printf("\nRepaired %s of %s.", strs.Number(float64(repaired)), strs.Count(adoptable, "session"))
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Make the repairs instead of only reporting them")
	command.Flags().Float64Var(&gap, "max-gap-seconds", 0, "Longest gap in seconds between parts of one conversation, 0 for 900")
	parent.AddCommand(command)
}

var profileLabels = map[string]string{
	"mislabelled":         "Sessions whose profile does not match their key",
	"unclaimed_namespace": "Sessions of a profile that no longer exists",
	"crossed_parent":      "Sessions linked to a session of another profile",
	"legacy_main":         "Old main sessions held by a named profile",
	"routing_unclaimed":   "Routes of a profile that no longer exists",
}

// Repairs a profile repair made, by the key it counts them under
var appliedLabels = map[string]string{
	"rekeyed":         "Given the key of their own profile",
	"moved":           "Moved to the default profile",
	"relabelled":      "Filed under the right profile",
	"severed":         "Links to another profile cut",
	"routing_deleted": "Routes deleted",
}

func addRepairProfiles(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply, asJSON, yes bool
	var legacy string
	command := &cobra.Command{Use: "repair-profiles", Short: "Fix session data that landed under the wrong profile",
		Long: "Looks for session data that crossed between profiles: sessions whose profile does not match their key, sessions of a profile " +
			"that no longer exists, links between sessions of two profiles and routes of a profile that no longer exists. Nothing changes " +
			"unless you pass --apply, which backs the store up first and is safe to run again. For old main sessions held by a named profile, " +
			"--legacy-main rekey gives them to that profile and move gives them to the default profile.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if legacy != "report" && legacy != "rekey" && legacy != "move" {
				return errors.New("--legacy-main takes report, rekey or move.")
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
				r.print("Checked the session store of the default profile.")
				if len(findings) == 0 {
					r.print("✓ No session data crossed between profiles.")
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
					r.printf("\n%s: %s", cmp.Or(profileLabels[kind], kind), strs.Number(float64(len(group))))
					for _, f := range group {
						r.printf("  %s\n      %s", f.GetSubject(), strs.Sentence(f.GetDetail()))
						switch {
						case f.GetAction() != "":
							r.print("      Repair: " + strs.Sentence(f.GetAction()))
						case f.GetReason() != "":
							r.print("      ✗ Not repaired. " + strs.Sentence(f.GetReason()))
						default:
							r.print("      ✗ Not repaired.")
						}
					}
				}
			}
			if !apply {
				if len(findings) > 0 {
					r.printf("\n%s of %s can be repaired. Run the command again with --apply to repair them.", strs.Number(float64(repairable)),
						strs.Count(len(findings), "finding"))
				}
				return nil
			}
			if repairable == 0 {
				if asJSON {
					data, _ := json.Marshal(map[string]any{"findings": list, "applied": map[string]any{}})
					r.print(string(data))
				} else {
					r.print("\nThere is nothing to repair.")
				}
				return nil
			}
			if !yes && !asJSON {
				ok, err := r.confirm(fmt.Sprintf("\nMake %s? icebath backs up the session store first. [y/N] ", strs.Count(repairable, "repair")), "Nothing changed.",
					"--yes")
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
				if applied.GetBackup() != "" {
					r.printf("  Backed up the session store to %s.", applied.GetBackup())
				} else {
					r.print("  ✗ The backup of the session store failed.")
				}
				if len(applied.GetApplied()) == 0 {
					r.print("\nNo repairs were made.")
				} else {
					r.print("\nRepairs made:")
					for _, key := range slices.Sorted(mapsKeys(applied.GetApplied())) {
						r.printf("  %s: %s", cmp.Or(appliedLabels[key], key), strs.Number(float64(applied.GetApplied()[key])))
					}
				}
				for _, failure := range applied.GetFailures() {
					r.print("  ✗ " + strs.Sentence(failure))
				}
				r.print("Run the command again without --apply to check that nothing is left.")
			}
			if len(applied.GetFailures()) > 0 {
				return errors.New("Some repairs failed, as listed above.")
			}
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Make the repairs instead of only reporting them")
	command.Flags().BoolVar(&asJSON, "json", false, "Print the report as JSON")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking first")
	command.Flags().StringVar(&legacy, "legacy-main", "report", "What to do with old main sessions held by a named profile: report, rekey or move")
	parent.AddCommand(command)
}

func addRepairPrompts(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply, asJSON bool
	command := &cobra.Command{Use: "repair-prompts [session_id]", Short: "Find and clear damaged system prompts",
		Long: "Finds saved system prompts that lack the guidance their tools need, so a session would go on with a damaged prompt. Clearing " +
			"a prompt makes the next reply build and save a healthy one. Prompts whose tools cannot be read are reported and never changed by " +
			"a scan. Nothing changes unless you pass --apply. Naming a session clears its prompt even when the prompt is healthy.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			report, err := r.service.RepairPrompts(r.ctx(), &sessionsv1.RepairPromptsRequest{SessionId: target})
			if err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return notFound(target)
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
					r.printf("%s has no saved system prompt, so there is nothing to clear.", report.GetSessionId())
				default:
					r.print("✓ No saved system prompt is damaged.")
					if n := len(report.GetUnverifiable()); n > 0 {
						r.printf("%s could not be checked, because they keep too little about their tools, and were left as they are.",
							strs.Count(n, "saved prompt"))
					}
				}
				return nil
			}
			if !asJSON {
				for _, f := range report.GetFindings() {
					r.printf("  %s, %s: %s", f.GetId(), strs.Counted(float64(f.GetPromptChars()), "characters"), strs.Sentence(f.GetReason()))
				}
				if n := len(report.GetUnverifiable()); n > 0 {
					r.printf("\nSkipped %s that keep too little about their tools to check.", strs.Count(n, "saved prompt"))
				}
			}
			if !apply {
				if asJSON {
					r.print(payload(nil))
				} else {
					r.printf("\n%s can be repaired. Run the command again with --apply to clear them. The next reply in each session builds a "+
						"healthy prompt, which misses the prompt cache once.", strs.Count(len(report.GetFindings()), "saved prompt"))
				}
				return nil
			}
			if !asJSON {
				ok, err := r.confirm(fmt.Sprintf("Clear %s? Each session builds a new one at its next reply. [y/N] ",
					strs.Count(len(report.GetFindings()), "saved prompt")), "Nothing changed.", "--json")
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
			r.printf("\n✓ Cleared %s. The next reply in each session builds and saves a healthy prompt.", strs.Count(len(applied.GetCleared()), "saved prompt"))
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Clear the damaged prompts instead of only reporting them")
	command.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON, and with --apply clear without asking")
	parent.AddCommand(command)
}

func addRefreshPrompts(parent *cobra.Command, at func(*cobra.Command) *run) {
	var session string
	var asJSON bool
	command := &cobra.Command{Use: "refresh-prompts", Short: "Have sessions rebuild their system prompt",
		Long: "After you change SOUL.md, skills or context files, refresh-prompts asks every session, or the one --session names, to rebuild " +
			"its saved system prompt once, at its next message. History and session ids stay. Each refreshed session costs one miss of the " +
			"prompt cache. refresh-prompts works while a server runs and across restarts.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			response, err := r.service.RefreshPrompts(r.ctx(), &sessionsv1.RefreshPromptsRequest{SessionId: session})
			if err != nil {
				return errors.New(strs.Sentence(tui.Reason(err)))
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
			scope := "every session"
			if id := response.GetSessionId(); id != "" {
				scope = "the session " + id
			}
			r.printf("✓ Asked %s to build its system prompt again. %s a saved prompt.", scope, strs.Count(int(response.GetSessions()), "session")+" had")
			r.print("Each builds its prompt from SOUL.md, the context files and the settings at its next message, which misses the prompt cache once. " +
				"The history of each session stays.")
			return nil
		}}
	command.Flags().StringVar(&session, "session", "", "Session to refresh, by id or the unique start of one")
	command.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON")
	parent.AddCommand(command)
}

func addRetitleSkills(parent *cobra.Command, at func(*cobra.Command) *run) {
	var apply bool
	var limit int
	command := &cobra.Command{Use: "retitle-skills", Short: "Retitle sessions named after a skill instead of your message",
		Long: "A session opened with a skill's slash command was titled from the whole text of the skill, so the title describes the skill " +
			"and not what you asked. retitle-skills writes those titles again from what you typed. Without --apply it lists what it would " +
			"change.",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if limit < 1 {
				return errors.New("--limit must be at least 1.")
			}
			response, err := r.service.RetitleSkills(r.ctx(), &sessionsv1.RetitleSkillsRequest{Apply: apply, Limit: int32(limit)})
			if err != nil {
				return err
			}
			if response.GetCandidates() == 0 {
				r.print("✓ No session is titled after a skill.")
				return nil
			}
			r.printf("%s opened with a skill:", strs.Count(int(response.GetCandidates()), "session"))
			changed := 0
			for _, retitle := range response.GetRetitles() {
				if retitle.GetKept() {
					r.printf("  %s\n    Kept %s, because the new title %s does not start with a letter or digit.", retitle.GetId(), retitle.GetOldTitle(),
						retitle.GetNewTitle())
					continue
				}
				r.printf("  %s\n    %s\n    becomes %s", retitle.GetId(), retitle.GetOldTitle(), retitle.GetNewTitle())
				switch {
				case retitle.GetError() != "":
					r.print("    Skipped. " + strs.Sentence(retitle.GetError()))
				case retitle.GetRenamed() != "":
					r.printf("    It is called %s instead, because that title was taken.", retitle.GetRenamed())
					changed++
				default:
					changed++
				}
			}
			switch {
			case changed == 0:
				r.print("  Every title already says what you asked.")
			case apply:
				r.printf("✓ Retitled %s.", strs.Count(changed, "session"))
			default:
				r.print("Nothing changed. Pass --apply to write the new titles.")
			}
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Write the new titles instead of only listing them")
	command.Flags().IntVar(&limit, "limit", 200, "Largest number of sessions to look at")
	parent.AddCommand(command)
}
