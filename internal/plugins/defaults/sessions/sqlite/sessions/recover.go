package sessions

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Tables recovery copies, parents first; leases, beats and admissions are runtime state a new process starts without
var canonical = []string{"system_prompts", "sessions", "messages", "session_model_usage", "gateway_routing", "gateway_hygiene_state",
	"conversation_generations", "state_meta"}

// Tables inspection reports on, the runtime ones too
var inventory = append(slices.Clone(canonical), "session_admissions", "session_turn_leases", "compression_locks", "backend_heartbeats")

// Range queries a partial salvage of one table may make
const salvageQueries = 10000

type tableReport struct {
	Available bool     `json:"available"`
	Columns   []string `json:"columns"`
	Rows      *int64   `json:"rows"`
	Error     string   `json:"error,omitempty"`
}

type skipped struct {
	Low   int64  `json:"low"`
	High  int64  `json:"high"`
	Error string `json:"error"`
}

type copyReport struct {
	SourceRows int64     `json:"source_rows"`
	CopiedRows int64     `json:"copied_rows"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Skipped    []skipped `json:"skipped_rowid_ranges,omitempty"`
	Rejected   int64     `json:"destination_rejected_rows,omitempty"`
}

type part struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime_ns"`
}

func bundleFingerprint(path string) map[string]part {
	out := map[string]part{}
	for _, sidecar := range []string{"", "-wal", "-shm", "-journal"} {
		if info, err := os.Stat(path + sidecar); err == nil {
			out["main"+sidecar] = part{Size: info.Size(), MTime: info.ModTime().UnixNano()}
		}
	}
	return out
}

func (s *store) recover(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	source, output := r.GetPath(), r.GetOutput()
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Source is not a file: %s", source)
	}
	if source, err = filepath.EvalSymlinks(source); err != nil {
		return err
	}
	inspect := r.GetCheckOnly()
	switch {
	case inspect && output != "":
		return errors.New("--output cannot be used with --inspect-only.")
	case inspect && r.GetAllowPartial():
		return errors.New("--allow-partial cannot be used with --inspect-only.")
	case !inspect && output == "":
		return errors.New("--output is required unless --inspect-only is used.")
	}
	report := r.GetReport()
	if !inspect {
		if output, err = filepath.Abs(output); err != nil {
			return err
		}
		if filepath.Base(output) == "" || filepath.Base(output) == string(filepath.Separator) {
			return fmt.Errorf("the recovery output path %s has no file name", output)
		}
		for _, sidecar := range []string{"", "-wal", "-shm", "-journal"} {
			if output == source+sidecar {
				return errors.New("The recovery output must not be the source database or one of its journal sidecars.")
			}
			if _, err := os.Stat(output + sidecar); err == nil {
				return fmt.Errorf("Refusing to overwrite existing recovery output: %s", output+sidecar)
			}
		}
		if report == "" {
			report = filepath.Join(filepath.Dir(output), filepath.Base(output)+".recovery.json")
		}
	}
	if report != "" {
		if _, err := os.Stat(report); err == nil {
			return fmt.Errorf("refusing to overwrite existing report: %s", report)
		}
	}
	chunk := int(r.GetChunkSize())
	if chunk == 0 {
		chunk = 1000
	}
	if chunk < 0 {
		return errors.New("chunk_size must be greater than zero")
	}
	root := cmp.Or(r.GetWorkDir(), filepath.Dir(cmp.Or(output, source)))
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("Recovery work directory is not a directory: %s", root)
	}
	var bundle int64
	for _, p := range bundleFingerprint(source) {
		bundle += p.Size
	}
	allowance := int64(0)
	if !inspect {
		allowance = bundle
	}
	headroom := max(256<<20, (bundle+allowance)/20)
	if free, ok := freeSpace(root); ok && free < bundle+allowance+headroom {
		return fmt.Errorf("Not enough free disk space for a safe recovery copy: %s available at %s, %s required (%s source bundle + %s output allowance + "+
			"%s headroom). Use --work-dir or --output on a filesystem with more free space.", human(free), root, human(bundle+allowance+headroom), human(bundle),
			human(allowance), human(headroom))
	}
	if source == s.config.GetString(sqliteconfig.Path) {
		s.Close()
	}
	work, err := os.MkdirTemp(root, "icebath-session-recovery-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	before := bundleFingerprint(source)
	snapshot := filepath.Join(work, "source.db")
	if _, err := copyBundle(source, snapshot); err != nil {
		return err
	}
	unchanged := fmt.Sprint(before) == fmt.Sprint(bundleFingerprint(source))
	if !unchanged {
		return errors.New("The source database bundle changed while it was being copied. Stop every icebath process using it and retry, or point --source " +
			"at an immutable snapshot copy of the database.")
	}
	doc := map[string]any{"source": source, "source_bundle": bundle, "source_fingerprint": before, "source_unchanged": unchanged,
		"disk_space": map[string]any{"bundle": bundle, "allowance": allowance, "headroom": headroom}}
	inspection := inspectCopy(ctx, snapshot)
	if inspect {
		for key, value := range inspection {
			doc[key] = value
		}
		doc["operation"] = "inspect"
		return finishReport(doc, report, progress, inspection["recoverable"] == true, "")
	}
	progress("Recovering canonical session data into a new database…")
	recoverable := inspection["recoverable"] == true
	if !recoverable && !r.GetAllowPartial() {
		return fmt.Errorf("Required canonical tables are not readable: %s. Re-run with --allow-partial to salvage every readable row into a new database "+
			"(the source is never modified).", strings.Join(inspection["errors"].([]string), "; "))
	}
	doc["operation"], doc["output"], doc["inspection"], doc["allow_partial"] = "recover", output, inspection, r.GetAllowPartial()
	doc["runtime_state"] = map[string]any{"policy": "transcript-only; runtime ledger excluded; fresh epoch on activation"}
	tables := inspection["tables"].(map[string]tableReport)
	from := snapshot
	bestEffort := false
	if r.GetAllowPartial() && (len(tables["sessions"].Columns) == 0 || len(tables["messages"].Columns) == 0) {
		salvaged, notes, err := lostAndFound(ctx, snapshot, work)
		if err != nil {
			return err
		}
		from, bestEffort = salvaged, true
		doc["lost_and_found"] = notes
	}
	copies, err := s.copyCanonical(ctx, from, output, chunk, r.GetAllowPartial(), progress)
	if err != nil {
		return fmt.Errorf("session recovery failed: %w\nThe supplied source database was not replaced or deleted.", err)
	}
	doc["copy"] = copies
	cleanup, err := finalize(ctx, output, r.GetAllowPartial())
	if err != nil {
		return fmt.Errorf("session recovery failed: %w\nThe supplied source database was not replaced or deleted.", err)
	}
	doc["orphan_cleanup"] = cleanup
	verified, checks, sessionCount, messageCount := verify(ctx, output, copies, r.GetAllowPartial())
	complete := verified && !r.GetAllowPartial() && !bestEffort
	for _, c := range copies {
		complete = complete && c.Status == "complete" || complete && c.Status == "empty"
	}
	doc["verification"], doc["verified"], doc["complete"], doc["partial"], doc["installed"] = checks, verified, complete, !complete, false
	verdict := ""
	switch {
	case complete:
		verdict = "✓ Recovered database verified at: " + output + "\n  The active session database was not changed.\n  Review the JSON report before installing this database."
	case r.GetAllowPartial() && verified && bestEffort:
		verdict = fmt.Sprintf("✓ BEST-EFFORT page-level salvage verified at: %s\n  The source table schemas were unreadable; rows were rebuilt from raw pages "+
			"via sqlite3 .recover.\n  Recovered %d sessions and %d messages.\n  The active session database was not changed.\n  This output is incomplete. "+
			"Review every skipped range and orphan count in the JSON report before installing it.", output, sessionCount, messageCount)
	case r.GetAllowPartial() && verified:
		verdict = fmt.Sprintf("✓ Partial recovery output verified at: %s\n  Recovered %d sessions and %d messages.\n  The active session database was not "+
			"changed.\n  This output is incomplete. Review every skipped range and orphan count in the JSON report before installing it.", output, sessionCount,
			messageCount)
	}
	if verdict == "" {
		verdict = "✗ Recovery output did not pass every verification check.\n  Do not install it. Review the JSON report for partial data or errors."
	}
	return finishReport(doc, report, progress, verdict != "" && !strings.HasPrefix(verdict, "✗"), verdict)
}

// Writes the report, or prints it when it has no path, then the verdict; fails when the outcome is not good
func finishReport(doc map[string]any, path string, progress func(string), good bool, verdict string) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if path == "" {
		progress(string(data))
	} else {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("could not write recovery report: %w", err)
		}
		_, err = f.Write(append(data, '\n'))
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("could not write recovery report: %w", err)
		}
		progress("Recovery report: " + path)
	}
	if !good {
		if verdict == "" {
			verdict = "The source is not fully recoverable."
		}
		return errors.New(verdict)
	}
	if verdict != "" {
		progress(verdict)
	}
	return nil
}

func human(n int64) string {
	value := float64(n)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB"} {
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
		value /= 1024
	}
	return fmt.Sprintf("%.1f TiB", value)
}

// How readable each table of the copy is, and whether the canonical ones are whole
func inspectCopy(ctx context.Context, path string) map[string]any {
	tables := map[string]tableReport{}
	var errs, warnings []string
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1000)&_pragma=writable_schema(1)")
	journal := ""
	if err == nil {
		defer db.Close()
		if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			warnings = append(warnings, "journal mode: "+err.Error())
		}
	}
	for _, table := range inventory {
		report := tableReport{}
		if err != nil {
			report.Error = err.Error()
			tables[table] = report
			continue
		}
		rows, columnsErr := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if columnsErr == nil {
			report.Columns, columnsErr = collect(rows)
		}
		if columnsErr != nil {
			report.Error = columnsErr.Error()
		} else if len(report.Columns) > 0 {
			var count int64
			if countErr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&count); countErr != nil {
				report.Error = countErr.Error()
			} else {
				report.Rows, report.Available = &count, true
			}
		}
		tables[table] = report
	}
	for _, table := range []string{"sessions", "messages"} {
		if !tables[table].Available {
			errs = append(errs, "required table "+table+" is not completely readable")
		}
	}
	return map[string]any{"tables": tables, "errors": errs, "warnings": warnings, "journal_mode": journal, "recoverable": len(errs) == 0}
}

// Copies the rows of every canonical table of the source into a new database of the current schema, each chunk committed by itself; a
// partial salvage copies around ranges of rows that can't be read
func (s *store) copyCanonical(ctx context.Context, source, output string, chunk int, partial bool, progress func(string)) (map[string]*copyReport, error) {
	if err := secure(output, true); err != nil {
		return nil, err
	}
	out, err := sql.Open("sqlite", "file:"+output+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)")
	if err != nil {
		return nil, err
	}
	defer out.Close()
	for _, ddl := range []string{schema, ftsSchema, trigramSchema} {
		if _, err := out.ExecContext(ctx, ddl); err != nil {
			return nil, err
		}
	}
	in, err := sql.Open("sqlite", "file:"+source+"?_pragma=busy_timeout(1000)&_pragma=writable_schema(1)")
	if err != nil {
		return nil, err
	}
	defer in.Close()
	reports := map[string]*copyReport{}
	for _, table := range canonical {
		report := &copyReport{Status: "missing"}
		reports[table] = report
		rows, err := in.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			report.Error = err.Error()
			continue
		}
		have, err := collect(rows)
		if err != nil || len(have) == 0 {
			continue
		}
		rows, err = out.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		want, err := collect(rows)
		if err != nil {
			return nil, err
		}
		var columns []string
		for _, column := range want {
			if slices.Contains(have, column) {
				columns = append(columns, `"`+column+`"`)
			}
		}
		in.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&report.SourceRows)
		list := strings.Join(columns, ", ")
		where := ""
		if table == "state_meta" {
			where = " WHERE key NOT LIKE 'fts_%' AND key <> 'db_file_generation'"
		}
		insert := `INSERT OR IGNORE INTO "` + table + `" (` + list + `) VALUES (` + marks(len(columns)) + `)`
		copyRange := func(query string, args ...any) (int64, error) {
			rows, err := in.QueryContext(ctx, query, args...)
			if err != nil {
				return 0, err
			}
			defer rows.Close()
			var copied int64
			tx, err := out.BeginTx(ctx, nil)
			if err != nil {
				return 0, err
			}
			defer tx.Rollback()
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					return 0, err
				}
				result, err := tx.ExecContext(ctx, insert, values...)
				if err != nil {
					return 0, err
				}
				if n, _ := result.RowsAffected(); n == 0 {
					report.Rejected++
				}
				copied++
				if copied%int64(chunk) == 0 {
					if err := tx.Commit(); err != nil {
						return 0, err
					}
					if tx, err = out.BeginTx(ctx, nil); err != nil {
						return 0, err
					}
				}
			}
			if err := rows.Err(); err != nil {
				return copied, err
			}
			return copied, tx.Commit()
		}
		select_ := `SELECT ` + list + ` FROM "` + table + `"` + where
		if !partial {
			copied, err := copyRange(select_)
			report.CopiedRows = copied
			if err != nil {
				report.Status, report.Error = "failed", err.Error()
				return reports, fmt.Errorf("table %s: %w", table, err)
			}
		} else {
			s.salvage(ctx, in, table, select_, where, copyRange, report)
		}
		switch {
		case report.SourceRows == 0 && report.CopiedRows == 0:
			report.Status = "empty"
		case report.CopiedRows >= report.SourceRows && len(report.Skipped) == 0:
			report.Status = "complete"
		case report.CopiedRows > 0:
			report.Status = "partial"
		default:
			report.Status = "failed"
		}
		progress(fmt.Sprintf("  %s: %d/%d", table, report.CopiedRows, report.SourceRows))
	}
	return reports, nil
}

// Copies the table range by range of row ids, halving a range that fails until single rows are read or skipped
func (s *store) salvage(ctx context.Context, in *sql.DB, table, query, where string, copyRange func(string, ...any) (int64, error), report *copyReport) {
	var low, high sql.NullInt64
	if err := in.QueryRowContext(ctx, `SELECT MIN(rowid), MAX(rowid) FROM "`+table+`"`).Scan(&low, &high); err != nil || !low.Valid {
		if err != nil {
			report.Error = err.Error()
		}
		return
	}
	connector := " WHERE "
	if where != "" {
		connector = " AND "
	}
	ranged := query + connector + "rowid BETWEEN ? AND ?"
	queries := 0
	var walk func(a, b int64)
	walk = func(a, b int64) {
		if a > b || queries >= salvageQueries {
			return
		}
		queries++
		copied, err := copyRange(ranged, a, b)
		report.CopiedRows += copied
		if err == nil {
			return
		}
		if a == b {
			report.Skipped = append(report.Skipped, skipped{Low: a, High: b, Error: err.Error()})
			return
		}
		middle := a + (b-a)/2
		walk(a, middle)
		walk(middle+1, b)
	}
	walk(low.Int64, high.Int64)
	if queries >= salvageQueries {
		report.Skipped = append(report.Skipped, skipped{Low: low.Int64, High: high.Int64, Error: "range query budget exhausted"})
	}
}

// Puts the recovered rows in order: rows of sessions that are gone given a stand-in session or dropped, links to missing rows cut, bad
// times and settings mended, the search indexes rebuilt
func finalize(ctx context.Context, output string, partial bool) (map[string]int64, error) {
	db, err := sql.Open("sqlite", "file:"+output+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	counts := map[string]int64{}
	run := func(key, query string, args ...any) error {
		result, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		counts[key] += n
		return nil
	}
	if partial {
		rows, err := db.QueryContext(ctx, `SELECT session_id, MIN(timestamp), COUNT(*) FROM messages WHERE session_id NOT IN (SELECT id FROM sessions) GROUP BY session_id`)
		if err != nil {
			return nil, err
		}
		type orphan struct {
			id    string
			start float64
			count int64
		}
		var orphans []orphan
		for rows.Next() {
			var o orphan
			var start sql.NullFloat64
			if err := rows.Scan(&o.id, &start, &o.count); err != nil {
				rows.Close()
				return nil, err
			}
			o.start = start.Float64
			orphans = append(orphans, o)
		}
		rows.Close()
		for i, o := range orphans {
			if err := run("placeholder_sessions", `INSERT INTO sessions (id, profile, source, started_at, title, title_source) VALUES (?, 'default', 'recovered', ?, ?, 'user')`,
				o.id, o.start, fmt.Sprintf("[recovered %d] session metadata was unreadable", i+1)); err != nil {
				return nil, err
			}
		}
		for key, query := range map[string]string{
			"dangling_parents": `UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IS NOT NULL AND parent_session_id NOT IN (SELECT id FROM sessions)`,
			"dangling_prompts": `UPDATE sessions SET system_prompt_hash = NULL WHERE system_prompt_hash IS NOT NULL AND system_prompt_hash NOT IN (SELECT hash FROM system_prompts)`,
			"orphan_usage":     `DELETE FROM session_model_usage WHERE session_id NOT IN (SELECT id FROM sessions)`,
		} {
			if err := run(key, query); err != nil {
				return nil, err
			}
		}
	}
	for key, query := range map[string]string{
		"unreferenced_prompts": `DELETE FROM system_prompts WHERE hash NOT IN (SELECT system_prompt_hash FROM sessions WHERE system_prompt_hash IS NOT NULL)`,
		"invalid_model_config": `UPDATE sessions SET model_config = '{}' WHERE model_config IS NOT NULL AND json_valid(model_config) = 0`,
		"started_out_of_range": `UPDATE sessions SET started_at = COALESCE((SELECT MIN(timestamp) FROM messages m WHERE m.session_id = sessions.id
			AND m.timestamp BETWEEN 0 AND 4200000000), strftime('%s', 'now')) WHERE started_at NOT BETWEEN 0 AND 4200000000`,
		"ended_out_of_range":    `UPDATE sessions SET ended_at = NULL WHERE ended_at IS NOT NULL AND ended_at NOT BETWEEN 0 AND 4200000000`,
		"activity_out_of_range": `UPDATE sessions SET last_activity_at = NULL WHERE last_activity_at IS NOT NULL AND last_activity_at NOT BETWEEN 0 AND 4200000000`,
		"messages_out_of_range": `UPDATE messages SET timestamp = (SELECT started_at FROM sessions s WHERE s.id = messages.session_id) WHERE timestamp NOT BETWEEN 0 AND 4200000000`,
	} {
		if err := run(key, query); err != nil {
			return nil, err
		}
	}
	for _, statement := range []string{`INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')`, `INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES ('rebuild')`,
		`UPDATE sessions SET message_count = (SELECT COUNT(*) FROM messages m WHERE m.session_id = sessions.id AND m.active = 1)`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// Checks the recovered database: it opens and works, its integrity and keys hold, its indexes answer, and it holds every row copied
func verify(ctx context.Context, output string, copies map[string]*copyReport, partial bool) (bool, map[string]any, int64, int64) {
	checks := map[string]any{}
	healthy, why := probe(ctx, output)
	checks["health"] = map[string]any{"healthy": healthy, "reason": why}
	ok := healthy
	db, err := sql.Open("sqlite", "file:"+output+"?mode=ro")
	if err != nil {
		checks["open"] = err.Error()
		return false, checks, 0, 0
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err == nil {
		var problems int
		for rows.Next() {
			problems++
		}
		rows.Close()
		checks["foreign_key_violations"] = problems
		ok = ok && problems == 0
	}
	var one int
	ftsErr := db.QueryRowContext(ctx, `SELECT 1 FROM messages_fts WHERE messages_fts MATCH 'icebath' LIMIT 1`).Scan(&one)
	checks["fts_probe"] = ftsErr == nil || errors.Is(ftsErr, sql.ErrNoRows)
	ok = ok && checks["fts_probe"] == true
	var sessionCount, messageCount int64
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount)
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messageCount)
	losses := []string{}
	for table, c := range copies {
		if c.Status == "partial" || c.Status == "failed" {
			losses = append(losses, table)
		}
	}
	slices.Sort(losses)
	checks["loss_detected"] = len(losses) > 0
	checks["tables_with_loss"] = losses
	if !partial {
		ok = ok && len(losses) == 0
	}
	return ok, checks, sessionCount, messageCount
}

// Rebuilds the rows of a copy whose schema can't be read from its raw pages with the sqlite3 command line's .recover, into a database whose
// tables keep their names
func lostAndFound(ctx context.Context, snapshot, work string) (string, map[string]any, error) {
	cli, err := exec.LookPath("sqlite3")
	if err != nil {
		return "", nil, errors.New("Partial recovery still requires readable table schemas for: sessions, messages. Page-level salvage needs the sqlite3 " +
			"command line (3.29 or later, with .recover) on PATH; install it, then re-run.")
	}
	salvaged := filepath.Join(work, "lost_and_found.db")
	notes := map[string]any{"best_effort": true}
	run := func(args ...string) error {
		dump := exec.CommandContext(ctx, cli, append([]string{"-readonly", snapshot}, args...)...)
		var sql bytes.Buffer
		var stderr bytes.Buffer
		dump.Stdout, dump.Stderr = &sql, &stderr
		if err := dump.Run(); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
		}
		load := exec.CommandContext(ctx, cli, salvaged)
		load.Stdin, load.Stderr = &sql, &stderr
		return load.Run()
	}
	if err := run(".recover --ignore-freelist"); err != nil {
		os.Remove(salvaged)
		if err := run(".recover"); err != nil {
			if !strings.Contains(err.Error(), "not a database") {
				return "", nil, fmt.Errorf("page-level salvage failed: %w", err)
			}
			if err := zeroHeader(snapshot); err != nil {
				return "", nil, err
			}
			notes["header_zeroed"] = true
			os.Remove(salvaged)
			if err := run(".recover"); err != nil {
				return "", nil, fmt.Errorf("page-level salvage failed: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", "file:"+salvaged+"?mode=ro")
	if err == nil {
		var unmapped int64
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lost_and_found`).Scan(&unmapped)
		db.Close()
		notes["unmapped_rows"] = unmapped
	}
	return salvaged, notes, nil
}

// Clears the first hundred bytes of a copy, so .recover reads its pages past a header it can't parse
func zeroHeader(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt(make([]byte, 100), 0)
	return err
}
