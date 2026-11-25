package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Session export calls of the console, swapped by tests that track SessionDB methods
var (
	consoleExportSession = (*state.SessionDB).ExportSession
	consoleExportAll     = (*state.SessionDB).ExportAll
)

// Refuses any argument with the usage line of a command that takes none
func consoleExpectNoArgs(args []string, usage string) error {
	if len(args) > 0 {
		return &ConsoleCommandError{Msg: "Usage: " + usage}
	}
	return nil
}

// Handler of a command taking no arguments whose output is the captured text of fn
func consoleSimpleCommand(usage string, fn func(ctx context.Context) (int, error)) consoleHandler {
	return func(e *ConsoleEngine, args []string) (string, error) {
		if err := consoleExpectNoArgs(args, usage); err != nil {
			return "", err
		}
		return consoleCaptureOutput(func() (int, error) { return fn(e.ctx) })
	}
}

// Handler running fn under the output capture and returning its text
func consoleCaptured(fn func(e *ConsoleEngine, args []string) (int, error)) consoleHandler {
	return func(e *ConsoleEngine, args []string) (string, error) {
		return consoleCaptureOutput(func() (int, error) { return fn(e, args) })
	}
}

// Whether a line is a rule of eight or more dashes or box drawing dashes
func consoleIsStatusFooterRule(line string) bool {
	stripped := strings.TrimSpace(tools.StripANSI(line))
	if utf8.RuneCountInString(stripped) < 8 {
		return false
	}
	return strings.Trim(strings.ReplaceAll(stripped, "─", "-"), "-") == ""
}

// Drops trailing lines that are blank once ANSI codes are removed
func consoleDropTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(tools.StripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Status text without its doctor and setup hint footer and the rule above it
func consoleStripStatusFooter(text string) string {
	lines := consoleDropTrailingBlank(textutil.Lines(text))
	if len(lines) < 2 {
		return strings.TrimRightFunc(text, unicode.IsSpace)
	}
	last := strings.TrimSpace(tools.StripANSI(lines[len(lines)-1]))
	prev := strings.TrimSpace(tools.StripANSI(lines[len(lines)-2]))
	if !strings.HasPrefix(prev, "Run 'icebath doctor'") || !strings.HasPrefix(last, "Start 'icebath' and type '/setup'") {
		return strings.TrimRightFunc(text, unicode.IsSpace)
	}
	lines = consoleDropTrailingBlank(lines[:len(lines)-2])
	if len(lines) > 0 && consoleIsStatusFooterRule(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace)
}

// Session rows as the fixed width id source count and title table
func consoleFormatSessions(sessions []*jsonutil.Object) string {
	if len(sessions) == 0 {
		return "No sessions found."
	}
	lines := []string{fmt.Sprintf("%-32s %-12s %5s  Title / Preview", "ID", "Source", "Msgs"),
		strings.Repeat("-", 82)}
	field := func(row *jsonutil.Object, key string) any {
		v, _ := row.Get(key)
		return v
	}
	for _, s := range sessions {
		sid := textutil.First(anyutil.String(field(s, "id")), 32)
		source := textutil.First(anyutil.String(anyutil.Or(field(s, "source"), "-")), 12)
		messages := fmt.Sprintf("%5s", anyutil.String(anyutil.Or(field(s, "message_count"), 0)))
		title := anyutil.Or(field(s, "title"), anyutil.Or(field(s, "preview"), ""))
		titleText := textutil.First(strings.ReplaceAll(anyutil.String(title), "\n", " "), 60)
		lines = append(lines, fmt.Sprintf("%-32s %-12s %s  %s", sid, source, messages, titleText))
	}
	return strings.Join(lines, "\n")
}

// Opens the session store of the process home, read only unless a command mutates it
func consoleSessionDB(ctx context.Context, readOnly bool) (*state.SessionDB, error) {
	return state.NewSessionDB(ctx, state.ResolveDefaultDBPath(ctx), readOnly)
}

// Version banner with the update check
func consoleVersion(ctx context.Context) (int, error) {
	printVersionInfoHook(true)
	return 0, nil
}

// Component status in full without the deep probes and without the hint footer
func consoleStatus(e *ConsoleEngine, args []string) (string, error) {
	if err := consoleExpectNoArgs(args, "status"); err != nil {
		return "", err
	}
	output, err := consoleCaptureOutput(func() (int, error) {
		return selfupdate.ShowStatus(e.ctx, home.Dir(e.ctx), map[string]any{"full": true, "deep": false}, os.Stdout)
	})
	if err != nil {
		return "", err
	}
	return consoleStripStatusFooter(output), nil
}

// Diagnostics without auto fix
func consoleDoctor(ctx context.Context) (int, error) {
	return selfupdate.RunDoctor(ctx, home.Dir(ctx), map[string]any{"fix": false, "ack": nil}, os.Stdout)
}

// Current configuration display
func consoleConfigShow(ctx context.Context) (int, error) {
	h := home.Dir(ctx)
	src, err := commands.ConfigCommandSources(h).Show(ctx, h)
	if err != nil {
		return 0, err
	}
	return 0, config.ShowConfig(ctx, os.Stdout, src)
}

// Text of an optional string argument, empty for None
func consoleText(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Recent lines of a named log or the log listing, never following
func consoleLogs(e *ConsoleEngine, args []string) (string, error) {
	for _, a := range args {
		if a == "-f" || a == "--follow" {
			return "", &ConsoleCommandError{Msg: "`logs -f` is not available in Icebath Console."}
		}
	}
	ns, err := consoleParse("logs", args, consolePositional("log_name", "?", "agent"),
		consoleOption(int64(50), true, "-n", "--lines"), consoleOption(nil, false, "--level"),
		consoleOption(nil, false, "--session"), consoleOption(nil, false, "--since"), consoleOption(nil, false, "--component"))
	if err != nil {
		return "", err
	}
	lines := consoleInt(ns, "lines")
	if consoleIntOutside(lines, 1, 500) {
		return "", &ConsoleCommandError{Msg: "logs --lines must be between 1 and 500"}
	}
	if consoleText(ns, "log_name") == "list" {
		return consoleCaptureOutput(func() (int, error) { return 0, ListLogs(e.ctx, os.Stdout) })
	}
	return consoleCaptureOutput(func() (int, error) {
		return TailLog(e.ctx, os.Stdout, TailLogOptions{Name: consoleText(ns, "log_name"), Lines: int(lines),
			Level: consoleText(ns, "level"), Session: consoleText(ns, "session"), Since: consoleText(ns, "since"),
			Component: consoleText(ns, "component")})
	})
}

// Recent listable sessions most recently active first
func consoleSessionsList(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("sessions list", args, consoleOption(int64(20), true, "--limit"))
	if err != nil {
		return "", err
	}
	limit := consoleInt(ns, "limit")
	if consoleIntOutside(limit, 1, 200) {
		return "", &ConsoleCommandError{Msg: "sessions list --limit must be between 1 and 200"}
	}
	db, err := consoleSessionDB(e.ctx, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	n := int(limit)
	sessions, err := db.ListSessionsRich(e.ctx, state.ListSessionsRichOptions{ExcludeSources: internalListingSources, Limit: &n,
		OrderByLastActive: true})
	if err != nil {
		return "", err
	}
	return consoleFormatSessions(sessions), nil
}

// Session store totals with per source counts of the main surfaces
func consoleSessionsStats(e *ConsoleEngine, args []string) (string, error) {
	if err := consoleExpectNoArgs(args, "sessions stats"); err != nil {
		return "", err
	}
	db, err := consoleSessionDB(e.ctx, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	total, err := db.SessionCount(e.ctx, state.SessionCountOptions{})
	if err != nil {
		return "", err
	}
	listable, err := db.SessionCount(e.ctx, state.SessionCountOptions{ExcludeChildren: true, ExcludeSources: internalListingSources})
	if err != nil {
		return "", err
	}
	messages, err := db.MessageCount(e.ctx, "")
	if err != nil {
		return "", err
	}
	lines := []string{fmt.Sprintf("Total sessions: %d", total), fmt.Sprintf("Listable sessions: %d", listable),
		fmt.Sprintf("Total messages: %d", messages)}
	for _, source := range []string{"cli", "tui", "telegram", "discord", "slack", "cron"} {
		count, err := db.SessionCount(e.ctx, state.SessionCountOptions{Source: source})
		if err != nil {
			return "", err
		}
		if count != 0 {
			lines = append(lines, fmt.Sprintf("  %s: %d", source, count))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Path of config.yaml under the active home
func consoleConfigPath(e *ConsoleEngine, args []string) (string, error) {
	if err := consoleExpectNoArgs(args, "config path"); err != nil {
		return "", err
	}
	return home.ConfigPath(home.Dir(e.ctx)), nil
}

// Sets one dotted config key to the remaining words joined by spaces
func consoleConfigSet(e *ConsoleEngine, args []string) (string, error) {
	if len(args) < 2 {
		return "", &ConsoleCommandError{Msg: "Usage: config set <key> <value>"}
	}
	h := home.Dir(e.ctx)
	return consoleCaptureOutput(func() (int, error) {
		err := config.SetConfigValue(e.ctx, h, args[0], strings.Join(args[1:], " "), commands.ConfigCommandSources(h))
		var exit *config.ConfigExit
		if errors.As(err, &exit) {
			return exit.Code, nil
		}
		return 0, err
	})
}

// Refuses an export whose single session exceeds the per session budget, zero disabling it
func consoleGuardExports(ctx context.Context, db *state.SessionDB, sessionIDs []string) error {
	err := db.AssertExportsSafe(ctx, sessionIDs, nil)
	var large *state.SessionExportTooLargeError
	if errors.As(err, &large) {
		return &ConsoleCommandError{Msg: large.Error()}
	}
	return err
}

// Exports one session or every session of a source as JSONL to a file or stdout
func consoleSessionsExport(e *ConsoleEngine, args []string) (int, error) {
	ns, err := consoleParse("sessions export", args, consolePositional("output", "", nil),
		consoleOption(nil, false, "--source"), consoleOption(nil, false, "--session-id"))
	if err != nil {
		return 0, err
	}
	db, err := consoleSessionDB(e.ctx, true)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	includeCompacted, includeInactive := state.ExportProjection(false)
	var rows []*jsonutil.Object
	if sessionID := consoleText(ns, "session_id"); sessionID != "" {
		resolved, err := db.ResolveSessionID(sessionID)
		if err != nil {
			return 0, err
		}
		if resolved == "" {
			return 0, &ConsoleCommandError{Msg: fmt.Sprintf("Session '%s' not found.", sessionID)}
		}
		if err := consoleGuardExports(e.ctx, db, []string{resolved}); err != nil {
			return 0, err
		}
		row, err := consoleExportSession(db, e.ctx, resolved, includeCompacted, includeInactive)
		if err != nil {
			return 0, err
		}
		if row == nil || row.Len() == 0 {
			return 0, &ConsoleCommandError{Msg: fmt.Sprintf("Session '%s' not found.", sessionID)}
		}
		rows = []*jsonutil.Object{row}
	} else {
		source := consoleText(ns, "source")
		var sources []string
		if source != "" {
			sources = []string{source}
		}
		found, err := db.SearchSessions(e.ctx, state.SearchSessionsOptions{Sources: sources, Limit: 100000})
		if err != nil {
			return 0, err
		}
		ids := make([]string, 0, len(found))
		for _, s := range found {
			v, _ := s.Get("id")
			ids = append(ids, anyutil.String(v))
		}
		if err := consoleGuardExports(e.ctx, db, ids); err != nil {
			return 0, err
		}
		if rows, err = consoleExportAll(db, e.ctx, source, includeCompacted, includeInactive); err != nil {
			return 0, err
		}
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		line, err := state.DumpsExportedSession(row)
		if err != nil {
			return 0, err
		}
		parts = append(parts, line)
	}
	text := strings.Join(parts, "\n")
	if text != "" {
		text += "\n"
	}
	output := consoleText(ns, "output")
	if output == "-" {
		_, err := os.Stdout.WriteString(text)
		return 0, err
	}
	if err := os.WriteFile(pathutil.ExpandUser(output), []byte(text), 0o666); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "Exported %d session(s) to %s\n", len(rows), output)
	return 0, nil
}

// Renames a session by id or unique prefix
func consoleSessionsRename(e *ConsoleEngine, args []string) (int, error) {
	ns, err := consoleParse("sessions rename", args, consolePositional("session_id", "", nil), consolePositional("title", "+", nil))
	if err != nil {
		return 0, err
	}
	db, err := consoleSessionDB(e.ctx, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	sessionID := consoleText(ns, "session_id")
	resolved, err := db.ResolveSessionID(sessionID)
	if err != nil {
		return 0, err
	}
	words, _ := ns["title"].([]any)
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, anyutil.String(w))
	}
	title := strings.Join(parts, " ")
	if resolved == "" {
		return 0, &ConsoleCommandError{Msg: fmt.Sprintf("Session '%s' not found.", sessionID)}
	}
	changed, err := db.SetSessionTitle(e.ctx, resolved, title)
	if err != nil {
		return 0, err
	}
	if !changed {
		return 0, &ConsoleCommandError{Msg: fmt.Sprintf("Session '%s' not found.", sessionID)}
	}
	fmt.Fprintf(os.Stdout, "Session '%s' renamed to: %s\n", resolved, title)
	return 0, nil
}

// Merges the FTS indexes unless another live process holds the store, which --force overrides
func consoleSessionsOptimize(e *ConsoleEngine, args []string) (int, error) {
	ns, err := consoleParse("sessions optimize", args, consoleFlag("--force"))
	if err != nil {
		return 0, err
	}
	db, err := consoleSessionDB(e.ctx, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if force, _ := ns["force"].(bool); !force {
		refusal, refused, err := state.HeldStoreRefusal(e.ctx, db.Path(), "optimize", "`sessions optimize --force`")
		if err != nil {
			return 0, err
		}
		if refused {
			return 0, &ConsoleCommandError{Msg: refusal}
		}
	}
	n, err := db.Vacuum(e.ctx)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "Optimized %d FTS index(es).\n", n)
	return 0, nil
}

// Repairs a malformed session database schema, only reporting the damage with --check-only
func consoleSessionsRepair(e *ConsoleEngine, args []string) (int, error) {
	ns, err := consoleParse("sessions repair", args, consoleFlag("--check-only"), consoleFlag("--no-backup"))
	if err != nil {
		return 0, err
	}
	dbPath := state.ResolveDefaultDBPath(e.ctx)
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stdout, "No session database at %s (nothing to repair).\n", dbPath)
		return 0, nil
	}
	reason, unhealthy, err := state.DBOpensCleanly(e.ctx, dbPath)
	if err != nil {
		return 0, err
	}
	if !unhealthy {
		fmt.Fprintf(os.Stdout, "%s opens cleanly; no repair needed.\n", dbPath)
		return 0, nil
	}
	fmt.Fprintf(os.Stdout, "%s does not open cleanly: %s\n", dbPath, reason)
	if checkOnly, _ := ns["check_only"].(bool); checkOnly {
		return 1, nil
	}
	noBackup, _ := ns["no_backup"].(bool)
	report, err := state.RepairStateDBSchema(e.ctx, dbPath, !noBackup)
	if err != nil {
		return 0, err
	}
	if !report.Repaired {
		msg := "Repair failed"
		if report.Error != "" {
			msg += ": " + report.Error
		}
		return 0, &ConsoleCommandError{Msg: msg}
	}
	if report.BackupPath != "" {
		fmt.Fprintln(os.Stdout, "backup: "+report.BackupPath)
	}
	if report.Strategy != "" {
		fmt.Fprintln(os.Stdout, "strategy: "+report.Strategy)
	}
	fmt.Fprintln(os.Stdout, "Repaired session database.")
	return 0, nil
}

// Status of the active profile, the bare profile command
func consoleProfileStatus(e *ConsoleEngine, args []string) (string, error) {
	if err := consoleExpectNoArgs(args, "profile"); err != nil {
		return "", err
	}
	return e.dispatch("profile", nil, nil, false)
}

// One built in console command row, a non empty confirmation marking it mutating
type consoleBuiltin struct {
	path           []string
	usage, summary string
	handler        consoleHandler
	confirmation   string
}

// Rows of built in commands the extras tier adds, the cron rows, empty in a build without them
var consoleTierBuiltins []consoleBuiltin

// Built in commands with their usage, summary and confirmation prompt, the _BUILTIN_COMMANDS table
func consoleBuiltinCommands() []consoleBuiltin {
	return append([]consoleBuiltin{
		{[]string{"status"}, "status", "Show Icebath component status.", consoleStatus, ""},
		{[]string{"version"}, "version", "Show Icebath version information.", consoleSimpleCommand("version", consoleVersion), ""},
		{[]string{"doctor"}, "doctor", "Run diagnostics without auto-fix.", consoleSimpleCommand("doctor", consoleDoctor), ""},
		{[]string{"logs"}, "logs [name] [-n N]", "Show recent Icebath logs.", consoleLogs, ""},
		{[]string{"sessions", "list"}, "sessions list [--limit N]", "List recent sessions.", consoleSessionsList, ""},
		{[]string{"sessions", "stats"}, "sessions stats", "Show session store statistics.", consoleSessionsStats, ""},
		{[]string{"config", "show"}, "config show", "Show current configuration.", consoleSimpleCommand("config show", consoleConfigShow), ""},
		{[]string{"config", "path"}, "config path", "Print config.yaml path.", consoleConfigPath, ""},
		{[]string{"profile"}, "profile", "Show active profile status.", consoleProfileStatus, ""},
		{[]string{"config", "set"}, "config set <key> <value>", "Set a configuration value.", consoleConfigSet,
			"Update Icebath configuration?"},
		{[]string{"sessions", "export"}, "sessions export <output> [--source SOURCE] [--session-id ID]", "Export sessions to JSONL.",
			consoleCaptured(consoleSessionsExport), "Export session data?"},
		{[]string{"sessions", "rename"}, "sessions rename <session> <title>", "Rename a session.",
			consoleCaptured(consoleSessionsRename), "Rename this session?"},
		{[]string{"sessions", "optimize"}, "sessions optimize [--force]", "Optimize the session store.",
			consoleCaptured(consoleSessionsOptimize), "Optimize the session database?"},
		{[]string{"sessions", "repair"}, "sessions repair [--check-only] [--no-backup]", "Repair a malformed session database schema.",
			consoleCaptured(consoleSessionsRepair), "Repair the session database?"},
	}, consoleTierBuiltins...)
}
