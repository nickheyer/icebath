package app

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session export calls of the console, swapped by tests the way hermes tracks SessionDB methods
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
	stripped := pystr.Strip(tools.StripANSI(line))
	if pystr.Len(stripped) < 8 {
		return false
	}
	return strings.Trim(strings.ReplaceAll(stripped, "─", "-"), "-") == ""
}

// Drops trailing lines that are blank once ANSI codes are removed
func consoleDropTrailingBlank(lines []string) []string {
	for len(lines) > 0 && pystr.Strip(tools.StripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Status text without its doctor and setup hint footer and the rule above it
func consoleStripStatusFooter(text string) string {
	lines := consoleDropTrailingBlank(pystr.Splitlines(text, false))
	if len(lines) < 2 {
		return pystr.Rstrip(text)
	}
	last := pystr.Strip(tools.StripANSI(lines[len(lines)-1]))
	prev := pystr.Strip(tools.StripANSI(lines[len(lines)-2]))
	if !strings.HasPrefix(prev, "Run 'icebath doctor'") || !strings.HasPrefix(last, "Run 'icebath setup'") {
		return pystr.Rstrip(text)
	}
	lines = consoleDropTrailingBlank(lines[:len(lines)-2])
	if len(lines) > 0 && consoleIsStatusFooterRule(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return pystr.Rstrip(strings.Join(lines, "\n"))
}

// Session rows as the fixed width id source count and title table
func consoleFormatSessions(sessions []*pyjson.Object) string {
	if len(sessions) == 0 {
		return "No sessions found."
	}
	lines := []string{fmt.Sprintf("%s %s %5s  Title / Preview", pystr.Ljust("ID", 32), pystr.Ljust("Source", 12), "Msgs"),
		strings.Repeat("-", 82)}
	field := func(row *pyjson.Object, key string) any {
		v, _ := row.Get(key)
		return v
	}
	for _, s := range sessions {
		sid := pystr.Slice(pyrepr.Str(pyrepr.Or(field(s, "id"), "")), 0, 32)
		source := pystr.Slice(pyrepr.Str(pyrepr.Or(field(s, "source"), "-")), 0, 12)
		messages, _ := pyrepr.Format(pyrepr.Or(field(s, "message_count"), 0), ">5")
		title := pyrepr.Or(field(s, "title"), pyrepr.Or(field(s, "preview"), ""))
		titleText := pystr.Slice(strings.ReplaceAll(pyrepr.Str(title), "\n", " "), 0, 60)
		lines = append(lines, fmt.Sprintf("%s %s %s  %s", pystr.Ljust(sid, 32), pystr.Ljust(source, 12), messages, titleText))
	}
	return strings.Join(lines, "\n")
}

// One cron job line naming the action, the job, its id and its effective state
func consoleFormatJob(job *cron.Job, action string) string {
	jobID := job.ID
	if jobID == "" {
		jobID = "?"
	}
	name := job.Name
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("%s job: %s (%s) [%s]", action, name, jobID, cron.EffectiveJobState(job))
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
	return 0, config.ShowConfig(os.Stdout, src)
}

// Cron scheduler status
func consoleCronStatus(ctx context.Context) (int, error) {
	return 0, cron.CronStatus(ctx, home.Dir(ctx), os.Stdout)
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
		consoleOption(big.NewInt(50), true, "-n", "--lines"), consoleOption(nil, false, "--level"),
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
		return TailLog(e.ctx, os.Stdout, TailLogOptions{Name: consoleText(ns, "log_name"), Lines: int(lines.Int64()),
			Level: consoleText(ns, "level"), Session: consoleText(ns, "session"), Since: consoleText(ns, "since"),
			Component: consoleText(ns, "component")})
	})
}

// Recent listable sessions most recently active first
func consoleSessionsList(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("sessions list", args, consoleOption(big.NewInt(20), true, "--limit"))
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
	n := int(limit.Int64())
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
		err := config.SetConfigValue(e.ctx, h, args[0], strings.Join(args[1:], " "), false, commands.ConfigCommandSources(h))
		var exit *config.ConfigExit
		if errors.As(err, &exit) {
			return exit.Code, nil
		}
		return 0, err
	})
}

// Adds missing config defaults without prompting and reports whether anything changed
func consoleConfigMigrate(e *ConsoleEngine, args []string) (int, error) {
	if err := consoleExpectNoArgs(args, "config migrate"); err != nil {
		return 0, err
	}
	results, err := config.MigrateConfig(e.ctx, home.Dir(e.ctx), false, false)
	if err != nil {
		return 0, err
	}
	if len(results.EnvAdded) > 0 || len(results.ConfigAdded) > 0 {
		fmt.Fprintln(os.Stdout, "Configuration updated.")
	} else {
		fmt.Fprintln(os.Stdout, "Configuration is up to date.")
	}
	for _, warning := range results.Warnings {
		fmt.Fprintln(os.Stdout, "Warning: "+warning)
	}
	return 0, nil
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
	var rows []*pyjson.Object
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
		rows = []*pyjson.Object{row}
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
			ids = append(ids, pyrepr.Str(v))
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
	if err := os.WriteFile(pypath.Expanduser(output), []byte(text), 0o666); err != nil {
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
		parts = append(parts, pyrepr.Str(w))
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
		return 0, &ConsoleCommandError{Msg: "Repair failed: " + consoleNoneText(report.Error)}
	}
	if report.BackupPath != "" {
		fmt.Fprintln(os.Stdout, "backup: "+report.BackupPath)
	}
	fmt.Fprintln(os.Stdout, "strategy: "+consoleNoneText(report.Strategy))
	fmt.Fprintln(os.Stdout, "Repaired session database.")
	return 0, nil
}

// Python str of an optional report field, None when absent
func consoleNoneText(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Status of the active profile, the bare profile command
func consoleProfileStatus(e *ConsoleEngine, args []string) (string, error) {
	if err := consoleExpectNoArgs(args, "profile"); err != nil {
		return "", err
	}
	return e.dispatch("profile", nil, nil, false)
}

// Scheduled jobs, disabled ones too with --all
func consoleCronList(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("cron list", args, consoleFlag("--all"))
	if err != nil {
		return "", err
	}
	all, _ := ns["all"].(bool)
	return consoleCaptureOutput(func() (int, error) { return 0, cron.CronList(e.ctx, home.Dir(e.ctx), os.Stdout, all) })
}

// Runs a single job cron action on its one reference, an ambiguous name becoming a console error
func consoleCronJobAction(args []string, usage, action string, run func(ref string) (*cron.Job, error)) (string, error) {
	if len(args) != 1 {
		return "", &ConsoleCommandError{Msg: "Usage: " + usage}
	}
	job, err := run(args[0])
	var ambiguous *cron.AmbiguousJobReferenceError
	if errors.As(err, &ambiguous) {
		return "", &ConsoleCommandError{Msg: err.Error()}
	}
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", &ConsoleCommandError{Msg: "Job not found: " + args[0]}
	}
	return consoleFormatJob(job, action), nil
}

// Pauses a job with the console as the recorded reason
func consoleCronPause(e *ConsoleEngine, args []string) (string, error) {
	reason := "paused from icebath console"
	return consoleCronJobAction(args, "cron pause <job>", "Paused", func(ref string) (*cron.Job, error) {
		return cron.PauseJob(e.ctx, home.Dir(e.ctx), ref, &reason)
	})
}

// Resumes a paused job or re-arms a one shot at --at or now
func consoleCronResume(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("cron resume", args, consolePositional("job", "", nil), consoleOption(nil, false, "--at"),
		consoleFlag("--run-now"))
	if err != nil {
		return "", err
	}
	at, runNow := consoleText(ns, "at"), ns["run_now"] == true
	if at != "" && runNow {
		return "", &ConsoleCommandError{Msg: "Use exactly one of --at or --run-now."}
	}
	h, ref := home.Dir(e.ctx), consoleText(ns, "job")
	var job *cron.Job
	switch {
	case runNow:
		job, err = cron.RearmOneshot(e.ctx, h, ref, cron.NowISO(h))
	case at != "":
		job, err = cron.RearmOneshot(e.ctx, h, ref, at)
	default:
		job, err = cron.ResumeJob(e.ctx, h, ref)
	}
	var ambiguous *cron.AmbiguousJobReferenceError
	var invalid *cron.ValueError
	if errors.As(err, &ambiguous) || errors.As(err, &invalid) {
		return "", &ConsoleCommandError{Msg: err.Error()}
	}
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", &ConsoleCommandError{Msg: "Job not found: " + ref}
	}
	return consoleFormatJob(job, "Resumed"), nil
}

// Schedules a job for the next scheduler tick
func consoleCronRun(e *ConsoleEngine, args []string) (string, error) {
	return consoleCronJobAction(args, "cron run <job>", "Triggered", func(ref string) (*cron.Job, error) {
		return cron.TriggerJob(e.ctx, home.Dir(e.ctx), ref)
	})
}

// One built in console command row, a non empty confirmation marking it mutating
type consoleBuiltin struct {
	path           []string
	usage, summary string
	handler        consoleHandler
	confirmation   string
}

// Built in commands with their usage, summary and confirmation prompt, the _BUILTIN_COMMANDS table
func consoleBuiltinCommands() []consoleBuiltin {
	return []consoleBuiltin{
		{[]string{"status"}, "status", "Show Icebath component status.", consoleStatus, ""},
		{[]string{"version"}, "version", "Show Icebath version information.", consoleSimpleCommand("version", consoleVersion), ""},
		{[]string{"doctor"}, "doctor", "Run diagnostics without auto-fix.", consoleSimpleCommand("doctor", consoleDoctor), ""},
		{[]string{"logs"}, "logs [name] [-n N]", "Show recent Icebath logs.", consoleLogs, ""},
		{[]string{"sessions", "list"}, "sessions list [--limit N]", "List recent sessions.", consoleSessionsList, ""},
		{[]string{"sessions", "stats"}, "sessions stats", "Show session store statistics.", consoleSessionsStats, ""},
		{[]string{"config", "show"}, "config show", "Show current configuration.", consoleSimpleCommand("config show", consoleConfigShow), ""},
		{[]string{"config", "path"}, "config path", "Print config.yaml path.", consoleConfigPath, ""},
		{[]string{"cron", "list"}, "cron list [--all]", "List scheduled jobs.", consoleCronList, ""},
		{[]string{"cron", "status"}, "cron status", "Show cron scheduler status.", consoleSimpleCommand("cron status", consoleCronStatus), ""},
		{[]string{"profile"}, "profile", "Show active profile status.", consoleProfileStatus, ""},
		{[]string{"config", "set"}, "config set <key> <value>", "Set a configuration value.", consoleConfigSet,
			"Update Icebath configuration?"},
		{[]string{"cron", "pause"}, "cron pause <job>", "Pause a scheduled job.", consoleCronPause, "Pause this cron job?"},
		{[]string{"cron", "resume"}, "cron resume <job>", "Resume a paused cron job.", consoleCronResume, "Resume this cron job?"},
		{[]string{"cron", "run"}, "cron run <job>", "Run a job on the next scheduler tick.", consoleCronRun, "Trigger this cron job?"},
		{[]string{"config", "migrate"}, "config migrate", "Update config with new options.", consoleCaptured(consoleConfigMigrate),
			"Update Icebath configuration with missing defaults?"},
		{[]string{"sessions", "export"}, "sessions export <output> [--source SOURCE] [--session-id ID]", "Export sessions to JSONL.",
			consoleCaptured(consoleSessionsExport), "Export session data?"},
		{[]string{"sessions", "rename"}, "sessions rename <session> <title>", "Rename a session.",
			consoleCaptured(consoleSessionsRename), "Rename this session?"},
		{[]string{"sessions", "optimize"}, "sessions optimize [--force]", "Optimize the session store.",
			consoleCaptured(consoleSessionsOptimize), "Optimize the session database?"},
		{[]string{"sessions", "repair"}, "sessions repair [--check-only] [--no-backup]", "Repair a malformed session database schema.",
			consoleCaptured(consoleSessionsRepair), "Repair the session database?"},
	}
}
