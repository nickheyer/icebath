package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// One icebath sessions invocation with its home, parsed namespace, output and the gateway owner a prune found
type sessionsRun struct {
	ctx           context.Context
	h             string
	ns            map[string]any
	w             io.Writer
	printHelp     func() error
	ownerEndpoint *gateway.GatewayEndpoint
}

// Writes one line the way print does
func (r *sessionsRun) print(text string) { fmt.Fprintln(r.w, text) }

// Writes one formatted line the way print of an f-string does
func (r *sessionsRun) printf(format string, a ...any) { fmt.Fprintf(r.w, format+"\n", a...) }

// Profile home the command runs against
func (r *sessionsRun) getHermesHome() string { return r.h }

// Directory holding the per session transcript files of the home
func (r *sessionsRun) sessionsDir() string { return filepath.Join(r.getHermesHome(), "sessions") }

// Namespace string of a dest, empty for None or another type
func nsText(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Python truthiness of a namespace dest, false when absent
func nsTruthy(ns map[string]any, key string) bool { return pyrepr.Truthy(ns[key]) }

// Namespace integer of a dest with its presence
func nsInt(ns map[string]any, key string) (int, bool) {
	switch v := ns[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// Value of a row key, nil when the row or the key is missing
func rowGet(row *pyjson.Object, key string) any {
	if row == nil {
		return nil
	}
	v, _ := row.Get(key)
	return v
}

// String value of a row key, empty when missing, None or another type
func rowText(row *pyjson.Object, key string) string {
	s, _ := rowGet(row, key).(string)
	return s
}

// First n code points of a string, the Python slice from the start
func runePrefix(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Left aligned text padded with spaces to a width counted in code points
func padRight(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Right aligned text padded with spaces to a width counted in code points
func padLeft(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// Python str of an exception a sessions command reports to the user
func sessionsExcText(err error) string {
	if _, ok := state.SQLiteClassOf(err); ok {
		return state.ErrText(err)
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Base name of a path under the path rules of the running platform
func sessionsBasename(p string) string {
	if runtime.GOOS == "windows" {
		return pypath.NtBasename(p)
	}
	return pypath.PosixBasename(p)
}

// File size in megabytes, zero when the path does not exist
func sizeMB(path string) float64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0.0
	}
	return float64(info.Size()) / (1024 * 1024)
}

// Size change wording that reads growth as grew by instead of a negative reclaim
func sizeDeltaLabel(savedMB float64) string {
	if savedMB >= 0 {
		return fmt.Sprintf("reclaimed %.1f MB", savedMB)
	}
	return fmt.Sprintf("grew by %.1f MB", -savedMB)
}

// Asks y or N, answered false when a non interactive stdin ended before any answer came
func confirmPrompt(r *sessionsRun, prompt string) (yes, answered bool, err error) {
	con := console()
	line, err := con.Input(prompt)
	if errors.Is(err, cliui.ErrEOF) {
		r.print("")
		if gateway.StdinIsInteractive(con.StdinIsTTY(), gateway.StdinConsoleModeOK()) {
			return false, true, nil
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	answer := pystr.Lower(pystr.Strip(line))
	return answer == "y" || answer == "yes", true, nil
}

// Confirmation gate returning refused with the exit code after saying why the action does not proceed
func confirmOrExitCode(r *sessionsRun, prompt, declined, unattended string) (code int, refused bool, err error) {
	yes, answered, err := confirmPrompt(r, prompt)
	if errors.Is(err, cliui.ErrInterrupt) {
		r.print("\nInterrupted; nothing was changed.")
		return 130, true, nil
	}
	if err != nil {
		return 1, true, err
	}
	if yes {
		return 0, false, nil
	}
	if !answered {
		hint := " Run it from an interactive terminal."
		if unattended != "" {
			hint = " Pass " + unattended + " to run it unattended."
		}
		r.print("No answer on a non-interactive stdin; nothing was changed." + hint)
		return 1, true, nil
	}
	r.print(declined)
	return 0, true, nil
}

// Reports an unknown session id and returns the failing exit code
func notFound(r *sessionsRun, sessionID string) int {
	r.printf("No session '%s'. Run: icebath sessions list to find the id.", sessionID)
	return 1
}

// Lists up to a hundred sessions a filtered export would write
func printDryRunPreview(r *sessionsRun, candidates []*pyjson.Object, filters map[string]any) {
	r.printf("Would export %d session(s) (%s).", len(candidates), describeFilters(filters))
	for _, row := range candidates[:min(len(candidates), 100)] {
		source := ""
		if v, ok := row.Get("source"); ok {
			source = pyrepr.Str(v)
		}
		r.printf("  %s  %s", pyrepr.Str(rowGet(row, "id")), source)
	}
	if len(candidates) > 100 {
		more := len(candidates) - 100
		sessionsPrintTruncated(r, &more, "")
	}
}

// Footer of a capped listing drawn on the command output
func sessionsPrintTruncated(r *sessionsRun, more *int, hint string) {
	con := console()
	(&cliui.Console{In: con.In, Out: r.w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}).PrintTruncated(r.ctx, more, hint)
}

var filterArgs = []string{
	"older_than", "newer_than", "before", "after", "source", "title", "end_reason", "cwd", "min_messages",
	"max_messages", "model", "provider", "user", "chat_id", "chat_type", "branch", "min_tokens", "max_tokens",
	"min_cost", "max_cost", "min_tool_calls", "max_tool_calls",
}

// Whether any session filter flag was given
func anyFilterArgs(ns map[string]any) bool {
	for _, a := range filterArgs {
		if ns[a] != nil {
			return true
		}
	}
	return false
}

// Directory of a multi file export, session-exports under the home when the output is empty or a dash
func exportDir(r *sessionsRun, output string) (string, error) {
	if output != "" && output != "-" {
		return pypath.PathExpanduser(pypath.PathStr(output))
	}
	return filepath.Join(r.getHermesHome(), "session-exports"), nil
}

// Output file of a single file export, the default name inside a directory spelled or existing as one
func outputFileInDir(output, defaultName string) (string, error) {
	if output == "" || output == "-" {
		return output, nil
	}
	info, err := os.Stat(output)
	isDir := err == nil && info.IsDir()
	if !strings.HasSuffix(output, "/") && !strings.HasSuffix(output, string(os.PathSeparator)) && !isDir {
		return output, nil
	}
	if err := os.MkdirAll(output, 0o777); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return pypath.NtJoin(output, defaultName), nil
	}
	return pypath.PosixJoin(output, defaultName), nil
}

// Writes text to stdout for an empty or dash output, otherwise to the file followed by the summary line
func writeOutput(r *sessionsRun, output, text, summary string) error {
	if output == "" || output == "-" {
		_, err := io.WriteString(r.w, text)
		return err
	}
	if err := os.WriteFile(output, []byte(text), 0o666); err != nil {
		return err
	}
	r.print(summary)
	return nil
}

// Pathlib rendering of a namespace path dest with its presence
func nsPath(ns map[string]any, key string) (string, bool) {
	s, ok := ns[key].(string)
	if !ok {
		return "", false
	}
	return pypath.PathStr(s), true
}

// Offline recovery working on a disposable copy of the source, never touching the active database
func cmdRecover(r *sessionsRun) (int, error) {
	source, _ := nsPath(r.ns, "source")
	output, hasOutput := nsPath(r.ns, "output")
	inspectOnly, allowPartial := nsTruthy(r.ns, "inspect_only"), nsTruthy(r.ns, "allow_partial")
	reportPath, hasReport := nsPath(r.ns, "report")
	if !inspectOnly && hasOutput && !hasReport {
		name := pypath.PathName(output)
		if name == "" {
			return 1, fmt.Errorf("ValueError: %s has an empty name", pyrepr.Repr(output))
		}
		reportPath, hasReport = pypath.PathJoin(pypath.PathParent(output), name+".recovery.json"), true
	}
	reportExists := false
	if hasReport {
		expanded, err := pypath.PathExpanduser(reportPath)
		if err != nil {
			return 1, err
		}
		_, statErr := os.Lstat(expanded)
		reportExists = statErr == nil
	}
	usageErrors := []struct {
		bad bool
		msg string
	}{
		{inspectOnly && hasOutput, "--output cannot be used with --inspect-only."},
		{inspectOnly && allowPartial, "--allow-partial cannot be used with --inspect-only."},
		{!inspectOnly && !hasOutput, "--output is required unless --inspect-only is used."},
		{reportExists, "refusing to overwrite existing report: " + reportPath},
	}
	for _, u := range usageErrors {
		if u.bad {
			r.print("Error: " + u.msg)
			return 2, nil
		}
	}
	workDir, _ := nsPath(r.ns, "work_dir")
	var report *pyjson.Object
	var err error
	if inspectOnly {
		report, err = inspectSessionDatabase(r.ctx, source, workDir)
	} else {
		r.print("Recovering canonical session data into a new database…")
		progress := &recoveryProgress{r: r}
		chunkSize, ok := nsInt(r.ns, "chunk_size")
		if !ok {
			chunkSize = 1000
		}
		report, err = recoverSessionDatabase(r.ctx, source, output, workDir, chunkSize, progress.call, allowPartial)
		if err == nil {
			progress.finish()
		}
	}
	if err != nil {
		var recoveryErr *sessionRecoveryError
		class, sqliteErr := state.SQLiteClassOf(err)
		if !errors.As(err, &recoveryErr) && !pypath.IsOSError(err) && !(sqliteErr && class != state.SQLiteInterfaceError) {
			return 1, err
		}
		r.printf("Error: session recovery failed: %s\nThe supplied source database was not replaced or deleted.", sessionsExcText(err))
		return 1, nil
	}
	if !hasReport {
		text, err := pyjson.Dumps(report, pyjson.Indent(2), pyjson.SortKeys(true))
		if err != nil {
			return 1, err
		}
		r.print(text)
	} else {
		written, err := writeRecoveryReport(reportPath, report)
		if err != nil {
			if !errors.Is(err, fs.ErrExist) && !pypath.IsOSError(err) {
				return 1, err
			}
			r.printf("Error: could not write recovery report: %s", sessionsExcText(err))
			return 1, nil
		}
		r.printf("Recovery report: %s", written)
	}
	if inspectOnly {
		if pyrepr.Truthy(rowGet(report, "recoverable")) {
			return 0, nil
		}
		return 1, nil
	}
	return printRecoveryVerdict(r, report, output, allowPartial), nil
}

// Recover progress printer drawing one live updating copied over total line per table
type recoveryProgress struct {
	r     *sessionsRun
	table any
}

// Whole number of a progress value grouped by thousands, zero for a value that is not a number
func groupedInt(v any) string {
	n, ok := pyrepr.Int(v)
	if !ok {
		if f, isFloat := v.(float64); isFloat {
			n, ok = pyrepr.Int(int64(f))
		}
	}
	if !ok {
		return "0"
	}
	text, _ := pyrepr.Format(n, ",")
	return text
}

// Draws the copied row count of the table a progress report names, opening a new line when the table changes
func (p *recoveryProgress) call(info *pyjson.Object) {
	table := rowGet(info, "table")
	if !pyrepr.Equal(table, p.table) {
		p.finish()
		fmt.Fprintf(p.r.w, "  %s: ", pyrepr.Str(table))
		p.table = table
	}
	suffix := ""
	if total := rowGet(info, "source_rows"); total != nil {
		suffix = "/" + groupedInt(total)
	}
	copied := rowGet(info, "copied_rows")
	if !pyrepr.Truthy(copied) {
		copied = 0
	}
	fmt.Fprintf(p.r.w, "\r  %s: %s%s", pyrepr.Str(table), groupedInt(copied), suffix)
}

// Ends the live line of the last table drawn
func (p *recoveryProgress) finish() {
	if p.table != nil {
		p.r.print("")
	}
}

// Says whether the recovered database passed verification and returns the exit code
func printRecoveryVerdict(r *sessionsRun, report *pyjson.Object, output string, allowPartial bool) int {
	if pyrepr.Truthy(rowGet(report, "complete")) {
		r.print("✓ Recovered database verified at: " + output + "\n" +
			"  The active session database was not changed.\n" +
			"  Review the JSON report before installing this database.")
		return 0
	}
	if allowPartial && pyrepr.Truthy(rowGet(report, "verified")) {
		var counts *pyjson.Object
		if verification, ok := rowGet(report, "verification").(*pyjson.Object); ok {
			counts, _ = rowGet(verification, "table_counts").(*pyjson.Object)
		}
		if pyrepr.Truthy(rowGet(report, "best_effort")) {
			r.print("✓ BEST-EFFORT page-level salvage verified at: " + output + "\n" +
				"  The source table schemas were unreadable; rows were rebuilt from raw pages " +
				"via sqlite3 .recover and mapped heuristically.")
		} else {
			r.print("✓ Partial recovery output verified at: " + output)
		}
		count := func(key string) string {
			if v := rowGet(counts, key); pyrepr.Truthy(v) {
				return groupedInt(v)
			}
			return "0"
		}
		r.print("  Recovered " + count("sessions") + " sessions and " + count("messages") + " messages.\n" +
			"  The active session database was not changed.\n" +
			"  This output is incomplete. Review every skipped range and orphan count in the " +
			"JSON report before installing it.")
		return 0
	}
	r.print("✗ Recovery output did not pass every verification check.\n" +
		"  Do not install it. Review the JSON report for partial data or errors.")
	return 1
}

// Imports a foreign session, failing only when an explicit path imported nothing
func cmdImport(r *sessionsRun) (int, error) {
	_, imported, err := runSessionsImport(r.ctx, r.ns, r.w)
	if err != nil {
		return 1, err
	}
	if !imported && nsTruthy(r.ns, "path") {
		return 1, nil
	}
	return 0, nil
}

// Sources hidden by default, third party tool sessions unless a source was asked for
func defaultExclude(ns map[string]any) []string {
	if nsTruthy(ns, "source") {
		return nil
	}
	return []string{"tool"}
}

// Reader whose source filter also matches a gateway row stored as local with that created surface
func surfaceReader(db *state.SessionDB) *gatewaycli.SurfaceLabelReader {
	return gatewaycli.NewSurfaceLabelReader(db)
}

// Prints the recent sessions table, with a workspace column when any row is bound and a footer when cut
func cmdList(r *sessionsRun, db *state.SessionDB) (int, error) {
	limit, _ := nsInt(r.ns, "limit")
	queryLimit := limit
	if limit > 0 {
		queryLimit = limit + 1
	}
	sessions, err := surfaceReader(db).ListSessionsRich(r.ctx, state.ListSessionsRichOptions{
		Source: nsText(r.ns, "source"), ExcludeSources: defaultExclude(r.ns), Limit: &queryLimit})
	if err != nil {
		return 1, err
	}
	truncated := limit > 0 && len(sessions) > limit
	if truncated {
		sessions = sessions[:limit]
	}
	wsKey := func(s *pyjson.Object) string {
		key, _ := state.WorkspaceKey(s)
		return key
	}
	wsFilter := pystr.Strip(nsText(r.ns, "workspace"))
	if wsFilter != "" {
		needle := pystr.Lower(wsFilter)
		var kept []*pyjson.Object
		for _, s := range sessions {
			key := pystr.Lower(wsKey(s))
			if key != "" && (strings.Contains(key, needle) || needle == sessionsBasename(strings.TrimRight(key, "/\\"))) {
				kept = append(kept, s)
			}
		}
		sessions = kept
	}
	if len(sessions) == 0 {
		r.print("No sessions found.")
		return 0, nil
	}
	hasWS, hasTitles := wsFilter != "", false
	for _, s := range sessions {
		hasWS = hasWS || wsKey(s) != ""
		hasTitles = hasTitles || pyrepr.Truthy(rowGet(s, "title"))
	}
	ws := func(s *pyjson.Object) string {
		key := wsKey(s)
		if key == "" {
			return "—"
		}
		base := sessionsBasename(strings.TrimRight(key, "/\\"))
		if base == "" {
			base = key
		}
		return runePrefix(base, 16)
	}
	title := func(s *pyjson.Object, n int) string {
		t := rowText(s, "title")
		if t == "" {
			t = "—"
		}
		return runePrefix(t, n)
	}
	preview := func(s *pyjson.Object, n int) string { return runePrefix(rowText(s, "preview"), n) }
	now := time.Now()
	ago := func(s *pyjson.Object) string {
		return pydatetime.RelativeTime(rowGet(s, "last_active"), rowText(s, "id"), now)
	}
	src := func(s *pyjson.Object) string {
		created, source := rowText(s, "created_source"), pyrepr.Str(rowGet(s, "source"))
		if created != "" && created != source {
			return created + "→" + source
		}
		return source
	}
	id := func(s *pyjson.Object) string { return pyrepr.Str(rowGet(s, "id")) }
	var header string
	var rule int
	var format func(s *pyjson.Object) string
	switch {
	case hasWS && hasTitles:
		header, rule = padRight("Title", 28)+" "+padRight("Workspace", 18)+" "+padRight("Last Active", 13)+" ID", 110
		format = func(s *pyjson.Object) string {
			return padRight(title(s, 26), 28) + " " + padRight(ws(s), 18) + " " + padRight(ago(s), 13) + " " + id(s)
		}
	case hasWS:
		header, rule = padRight("Preview", 38)+" "+padRight("Workspace", 18)+" "+padRight("Last Active", 13)+" "+padRight("Src", 16)+" ID", 110
		format = func(s *pyjson.Object) string {
			return padRight(preview(s, 36), 38) + " " + padRight(ws(s), 18) + " " + padRight(ago(s), 13) + " " + padRight(src(s), 16) + " " + id(s)
		}
	case hasTitles:
		header, rule = padRight("Title", 32)+" "+padRight("Preview", 40)+" "+padRight("Last Active", 13)+" ID", 110
		format = func(s *pyjson.Object) string {
			return padRight(title(s, 30), 32) + " " + padRight(preview(s, 38), 40) + " " + padRight(ago(s), 13) + " " + id(s)
		}
	default:
		header, rule = padRight("Preview", 50)+" "+padRight("Last Active", 13)+" "+padRight("Src", 16)+" ID", 105
		format = func(s *pyjson.Object) string {
			return padRight(preview(s, 48), 50) + " " + padRight(ago(s), 13) + " " + padRight(src(s), 16) + " " + id(s)
		}
	}
	r.print(header + "\n" + strings.Repeat("─", rule))
	for _, s := range sessions {
		r.print(format(s))
	}
	if truncated {
		sessionsPrintTruncated(r, nil, fmt.Sprintf("use --limit %d to see more", limit*2))
	}
	return 0, nil
}
