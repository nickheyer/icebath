package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Recovery failure, safety for a path or overwrite guard and source for unreadable canonical tables
type sessionRecoveryError struct {
	msg  string
	kind string
}

// Message of the recovery failure
func (e *sessionRecoveryError) Error() string { return e.msg }

// Guard failure raised before recovery touches anything
func recoverySafetyError(msg string) error { return &sessionRecoveryError{msg: msg, kind: "safety"} }

// Failure of a source that cannot provide the required canonical tables
func recoverySourceError(msg string) error { return &sessionRecoveryError{msg: msg, kind: "source"} }

var (
	recoveryCanonicalTables = []string{"system_prompts", "sessions", "messages", "session_model_usage", "compression_locks",
		"gateway_routing", "async_delegations"}
	recoveryTopicTables     = []string{"telegram_dm_topic_mode", "telegram_dm_topic_bindings"}
	recoveryAuxiliaryTables = []string{"delivery_obligations"}
	recoveryRuntimeTables   = []string{"runtime_epoch", "session_admissions", "worker_executions", "worker_receipts"}
	recoveryGeneratedMeta   = []string{"fts_storage_version", "fts_optimize_available", "fts_rebuild_high_water",
		"fts_rebuild_progress", "fts_cjk_stale", "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress",
		"telegram_dm_topic_schema_version", "fts_tool_full_content_high_water"}
	recoverySidecarSuffixes = []string{"", "-wal", "-shm", "-journal"}
	recoveryDependentTables = []string{"messages", "session_model_usage", "compression_locks", "telegram_dm_topic_bindings"}
	recoveryRelinkCounters  = []string{"session_prompt_refs_cleared", "sessions_parent_cleared"}
)

const (
	recoveryMinimumSpaceHeadroom = 256 * 1024 * 1024
	recoveryMaxSalvageQueries    = 10_000
	recoveryDanglingToolPin      = "length(tool_names) = 64 AND NOT EXISTS (SELECT 1 FROM system_prompts WHERE system_prompts.hash = sessions.tool_names)"
)

// Initializers of state.db tables a gateway module creates lazily, keyed by table
var recoveryAuxiliarySchemas = map[string]func(ctx context.Context, conn state.SQLiteConn) error{
	"delivery_obligations": gateway.InitializeLedgerSchema,
}

// Every table the inspection reports, canonical first and the excluded runtime ledger last
func recoveryInventoryTables() []string {
	out := append([]string{}, recoveryCanonicalTables...)
	out = append(out, "state_meta")
	out = append(out, recoveryTopicTables...)
	out = append(out, recoveryAuxiliaryTables...)
	return append(out, recoveryRuntimeTables...)
}

// True when name is in list
func recoveryContains(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// Strings as a JSON list
func recoveryStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// True for a Python sqlite3.DatabaseError or one of its subclasses
func isSQLiteDatabaseError(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	return ok && class != state.SQLiteInterfaceError
}

// Main database path or one of its journal sidecars
func recoverySidecar(dbPath, suffix string) string {
	if suffix == "" {
		return dbPath
	}
	return dbPath + suffix
}

// Not yet created output path under its strictly resolved parent
func recoveryResolvedOutput(path string) (string, error) {
	expanded := pypath.Expanduser(path)
	parent, err := pypath.Realpath(recoveryPathParent(expanded), pypath.StrictAll)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, recoveryPathName(expanded)), nil
}

// pathlib parent of a path, a bare name parenting to the current directory
func recoveryPathParent(p string) string {
	trimmed := strings.TrimRight(p, string(filepath.Separator)+"/")
	if trimmed == "" {
		return p
	}
	dir := filepath.Dir(trimmed)
	if dir == "" {
		return "."
	}
	return dir
}

// pathlib name of a path, the last component without trailing separators
func recoveryPathName(p string) string {
	return filepath.Base(strings.TrimRight(p, string(filepath.Separator)+"/"))
}

// Lax resolution of a path that may not exist
func recoveryResolveLax(p string) string {
	resolved, err := pypath.Realpath(p, pypath.Lax)
	if err != nil {
		abs, _ := filepath.Abs(p)
		return abs
	}
	return resolved
}

// Validated source, optional output and work root, refusing anything that could touch the source or an existing output
func recoveryValidatePaths(sourcePath, outputPath, workDir string) (string, string, string, error) {
	source, err := pypath.Realpath(pypath.Expanduser(sourcePath), pypath.StrictAll)
	if err != nil {
		return "", "", "", err
	}
	if info, serr := os.Stat(source); serr != nil || !info.Mode().IsRegular() {
		return "", "", "", recoverySafetyError("Source is not a file: " + source)
	}
	output := ""
	if outputPath != "" {
		if output, err = recoveryResolvedOutput(outputPath); err != nil {
			return "", "", "", err
		}
		resolvedOutput := recoveryResolveLax(output)
		for _, suffix := range recoverySidecarSuffixes {
			if recoveryResolveLax(recoverySidecar(source, suffix)) == resolvedOutput {
				return "", "", "", recoverySafetyError("The recovery output must not be the source database or one of its journal sidecars.")
			}
		}
		for _, suffix := range recoverySidecarSuffixes {
			candidate := recoverySidecar(output, suffix)
			if _, lerr := os.Lstat(candidate); lerr == nil {
				return "", "", "", recoverySafetyError("Refusing to overwrite existing recovery output: " + candidate)
			}
		}
	}
	var workRoot string
	switch {
	case workDir != "":
		if workRoot, err = pypath.Realpath(pypath.Expanduser(workDir), pypath.StrictAll); err != nil {
			return "", "", "", err
		}
	case output != "":
		workRoot = filepath.Dir(output)
	default:
		workRoot = filepath.Dir(source)
	}
	if info, serr := os.Stat(workRoot); serr != nil || !info.IsDir() {
		return "", "", "", recoverySafetyError("Recovery work directory is not a directory: " + workRoot)
	}
	return source, output, workRoot, nil
}

// Fingerprint of the source bundle, replaced by the case that changes it between reads
var recoverySourceFingerprint = recoverySourceFingerprintOf

// Size and nanosecond mtime of the source and each sidecar present, keyed main or by suffix
func recoverySourceFingerprintOf(source string) (*pyjson.Object, error) {
	fingerprint := pyjson.NewObject()
	for _, suffix := range recoverySidecarSuffixes {
		info, err := pypath.PathlibStat(recoverySidecar(source, suffix))
		if err != nil {
			return nil, err
		}
		if info == nil {
			continue
		}
		key := suffix
		if key == "" {
			key = "main"
		}
		fingerprint.Set(key, pyjson.ObjectOf("size", info.Size(), "mtime_ns", info.ModTime().UnixNano()))
	}
	return fingerprint, nil
}

// Byte count in binary units with one decimal
func recoveryFormatBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	for i, unit := range units {
		if amount < 1024 || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", amount, unit)
		}
		amount /= 1024
	}
	return fmt.Sprintf("%d B", value)
}

// Whether two existing directories share a device, else whether their anchors match case insensitively
func recoverySameFilesystem(left, right string) bool {
	ldev, _, lerr := pyfs.LstatIdentity(left)
	rdev, _, rerr := pyfs.LstatIdentity(right)
	if lerr == nil && rerr == nil {
		return ldev == rdev
	}
	return strings.EqualFold(recoveryAnchor(left), recoveryAnchor(right))
}

// Drive and root of a path, the pathlib anchor
func recoveryAnchor(p string) string {
	volume := filepath.VolumeName(p)
	rest := p[len(volume):]
	if strings.HasPrefix(rest, string(filepath.Separator)) || strings.HasPrefix(rest, "/") {
		return volume + string(filepath.Separator)
	}
	return volume
}

// Free bytes of the filesystem holding path
func recoveryDiskFree(path string) (int64, error) {
	_, _, free, err := pyfs.DiskUsage(path)
	if err != nil {
		return 0, err
	}
	if free > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}

// Requires free space for the disposable bundle, the output and safety headroom, returning the space report
func recoveryDiskSpacePreflight(source, workRoot, outputParent string) (*pyjson.Object, error) {
	var bundle int64
	fingerprint, err := recoverySourceFingerprint(source)
	if err != nil {
		return nil, err
	}
	for _, key := range fingerprint.Keys {
		info, _ := fingerprint.Get(key)
		size, _ := info.(*pyjson.Object).Get("size")
		bundle += size.(int64)
	}
	outputAllowance := int64(0)
	if outputParent != "" {
		outputAllowance = bundle
	}
	headroom := max(int64(recoveryMinimumSpaceHeadroom), int64(float64(bundle+outputAllowance)*0.05))
	workFree, err := recoveryDiskFree(workRoot)
	if err != nil {
		return nil, err
	}
	report := pyjson.ObjectOf("source_bundle_bytes", bundle, "estimated_output_bytes", outputAllowance, "headroom_bytes", headroom,
		"work_dir", workRoot, "work_dir_free_bytes", workFree)
	if outputParent == "" || recoverySameFilesystem(workRoot, outputParent) {
		required := bundle + outputAllowance + headroom
		report.Set("shared_filesystem", true)
		report.Set("work_dir_required_bytes", required)
		if workFree < required {
			return nil, recoverySafetyError(fmt.Sprintf("Not enough free disk space for a safe recovery copy: %s available at "+
				"%s, %s required (%s source bundle + %s output allowance + %s headroom). "+
				"Use --work-dir or --output on a filesystem with more free space.",
				recoveryFormatBytes(workFree), workRoot, recoveryFormatBytes(required), recoveryFormatBytes(bundle),
				recoveryFormatBytes(outputAllowance), recoveryFormatBytes(headroom)))
		}
		return report, nil
	}
	outputFree, err := recoveryDiskFree(outputParent)
	if err != nil {
		return nil, err
	}
	workRequired := bundle + headroom
	outputRequired := outputAllowance + headroom
	report.Set("shared_filesystem", false)
	report.Set("work_dir_required_bytes", workRequired)
	report.Set("output_dir", outputParent)
	report.Set("output_dir_free_bytes", outputFree)
	report.Set("output_dir_required_bytes", outputRequired)
	var shortages []string
	for _, check := range []struct {
		where          string
		free, required int64
	}{{workRoot, workFree, workRequired}, {outputParent, outputFree, outputRequired}} {
		if check.free < check.required {
			shortages = append(shortages, fmt.Sprintf("%s: %s available, %s required", check.where,
				recoveryFormatBytes(check.free), recoveryFormatBytes(check.required)))
		}
	}
	if len(shortages) > 0 {
		return nil, recoverySafetyError(fmt.Sprintf("Not enough free disk space for safe recovery: %s. "+
			"Choose work/output filesystems with more free space.", strings.Join(shortages, "; ")))
	}
	return report, nil
}

// File copy of the snapshot bundle, parked by the lifecycle lock case inside the copy
var recoveryCopy2 = pyfs.Copy2

// Copies the source and its sidecars into the snapshot dir under the connection lifecycle lock
func recoveryCopySourceBundle(source, snapshotDir string) (string, []string, error) {
	snapshotSource := filepath.Join(snapshotDir, filepath.Base(source))
	var copied []string
	var copyErr error
	err := state.OfflineFileAccess(source, "snapshot", func() {
		for _, suffix := range recoverySidecarSuffixes {
			part := recoverySidecar(source, suffix)
			if _, serr := os.Stat(part); serr != nil {
				continue
			}
			destination := recoverySidecar(snapshotSource, suffix)
			if copyErr = recoveryCopy2(part, destination); copyErr != nil {
				return
			}
			copied = append(copied, filepath.Base(destination))
		}
	})
	var live *state.LiveConnectionError
	if errors.As(err, &live) {
		return "", nil, recoverySafetyError(live.Error())
	}
	if err != nil {
		return "", nil, err
	}
	if copyErr != nil {
		return "", nil, copyErr
	}
	return snapshotSource, copied, nil
}

// Autocommit connection with a one second busy timeout
func recoveryConnect(path string) (state.SQLiteConn, error) {
	return state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 1.0})
}

// Autocommit connection with the default five second busy timeout
func recoveryConnectDefault(path string) (state.SQLiteConn, error) {
	return state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5.0})
}

// Runs a statement for its effect
func recoveryExec(conn state.SQLiteConn, query string, args ...any) (int64, error) {
	res, err := conn.ExecContext(context.Background(), query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// Every row of a query as value lists
func recoveryQueryAll(conn state.SQLiteConn, query string, args ...any) ([][]any, error) {
	rows, err := conn.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for {
		row, ok, err := recoveryNextRow(rows)
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, row)
	}
}

// First row of a query, false when there is none
func recoveryQueryOne(conn state.SQLiteConn, query string, args ...any) ([]any, bool, error) {
	rows, err := conn.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	return recoveryNextRow(rows)
}

// Next row of a cursor as values, false at the end
func recoveryNextRow(rows *sql.Rows) ([]any, bool, error) {
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, false, err
	}
	return values, true, nil
}

// Integer of a SQLite value
func recoveryInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := pyjson.ParseInt(strings.TrimSpace(x)).(int64)
		return n
	}
	return 0
}

// Column names of a table, empty when it does not exist
func recoveryTableColumns(conn state.SQLiteConn, table string) ([]string, error) {
	rows, err := recoveryQueryAll(conn, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, pyrepr.Str(row[1]))
	}
	return out, nil
}

// Row count of a table
func recoveryCountRows(conn state.SQLiteConn, table string) (int64, error) {
	row, _, err := recoveryQueryOne(conn, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table))
	if err != nil {
		return 0, err
	}
	return recoveryInt(row[0]), nil
}

// Runs fn inside BEGIN IMMEDIATE, committing on success and rolling back on any failure
func recoveryImmediate(conn state.SQLiteConn, fn func() error) (err error) {
	if _, err := recoveryExec(conn, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_, _ = recoveryExec(conn, "ROLLBACK")
			panic(r)
		}
	}()
	if err := fn(); err != nil {
		if _, rerr := recoveryExec(conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	_, err = recoveryExec(conn, "COMMIT")
	if err != nil {
		if _, rerr := recoveryExec(conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
	}
	return err
}

// Columns shared by source and destination, setting a terminal status and returning false when there are none
func recoveryCompatibleColumns(source, destination state.SQLiteConn, table string, result *pyjson.Object) ([]string, bool, error) {
	sourceColumns, err := recoveryTableColumns(source, table)
	if err != nil {
		return nil, false, err
	}
	destColumns, err := recoveryTableColumns(destination, table)
	if err != nil {
		return nil, false, err
	}
	var columns []string
	for _, column := range destColumns {
		if recoveryContains(sourceColumns, column) {
			columns = append(columns, column)
		}
	}
	result.Set("columns", recoveryStrings(columns))
	if len(sourceColumns) == 0 {
		result.Set("status", "missing")
		return nil, false, nil
	}
	if len(columns) == 0 {
		result.Set("status", "failed")
		result.Set("error", "source and destination have no compatible columns")
		return nil, false, nil
	}
	return columns, true, nil
}

// Quoted column list and matching parameter marks
func recoveryQuotedColumns(columns []string) (string, string) {
	quoted := make([]string, len(columns))
	marks := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + column + `"`
		marks[i] = "?"
	}
	return strings.Join(quoted, ", "), strings.Join(marks, ", ")
}

// Integer value of a report counter
func recoveryCounter(result *pyjson.Object, key string) int64 {
	v, _ := result.Get(key)
	n, _ := v.(int64)
	return n
}

// Adds delta to a report counter
func recoveryBump(result *pyjson.Object, key string, delta int64) {
	result.Set(key, recoveryCounter(result, key)+delta)
}

// Fetches up to size rows of a cursor, an error discarding the partial chunk
func recoveryFetchMany(rows *sql.Rows, size int) ([][]any, error) {
	var chunk [][]any
	for len(chunk) < size {
		row, ok, err := recoveryNextRow(rows)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		chunk = append(chunk, row)
	}
	return chunk, nil
}

// Chunked straight copy filling status and error on the result
func recoveryCopyRows(source, destination state.SQLiteConn, selectSQL string, params []any, insertSQL, table string,
	chunkSize int, progress func(*pyjson.Object), expected *int64, result *pyjson.Object) (*pyjson.Object, error) {
	copyErr := func() error {
		rows, err := source.QueryContext(context.Background(), selectSQL, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for {
			chunk, err := recoveryFetchMany(rows, chunkSize)
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				return nil
			}
			if err := recoveryImmediate(destination, func() error {
				for _, row := range chunk {
					if _, err := recoveryExec(destination, insertSQL, row...); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			recoveryBump(result, "copied_rows", int64(len(chunk)))
			if progress != nil {
				progress(pyjson.ObjectOf("table", table, "copied_rows", recoveryCounter(result, "copied_rows"), "source_rows", recoveryOptional(expected)))
			}
		}
	}()
	if copyErr != nil {
		if !isSQLiteDatabaseError(copyErr) {
			return nil, copyErr
		}
		status := "failed"
		if recoveryCounter(result, "copied_rows") != 0 {
			status = "partial"
		}
		result.Set("status", status)
		result.Set("error", sessionsExcText(copyErr))
		return result, nil
	}
	if expected == nil || recoveryCounter(result, "copied_rows") == *expected {
		result.Set("status", "complete")
	} else {
		result.Set("status", "partial")
		result.Set("error", fmt.Sprintf("copied %d of %d readable rows", recoveryCounter(result, "copied_rows"), *expected))
	}
	return result, nil
}

// JSON value of an optional count
func recoveryOptional(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// Availability, columns and row count of one table, a read error recorded on the entry
func recoveryTableInventory(conn state.SQLiteConn, table string) (*pyjson.Object, error) {
	result := pyjson.ObjectOf("available", false, "columns", []any{}, "rows", nil)
	columns, err := recoveryTableColumns(conn, table)
	if err == nil {
		if len(columns) == 0 {
			return result, nil
		}
		result.Set("available", true)
		result.Set("columns", recoveryStrings(columns))
		var count int64
		if count, err = recoveryCountRows(conn, table); err == nil {
			result.Set("rows", count)
		}
	}
	if err != nil {
		if !isSQLiteDatabaseError(err) {
			return nil, err
		}
		result.Set("error", sessionsExcText(err))
	}
	return result, nil
}

// Lower cased journal mode of a connection, nil when the pragma returns no row
func recoveryJournalMode(conn state.SQLiteConn) (any, error) {
	row, ok, err := recoveryQueryOne(conn, "PRAGMA journal_mode")
	if err != nil || !ok {
		return nil, err
	}
	return strings.ToLower(pyrepr.Str(row[0])), nil
}

// Readability inventory of every reported table, recoverable when sessions and messages read completely
func recoveryInspectConnection(conn state.SQLiteConn) (*pyjson.Object, error) {
	if _, err := recoveryExec(conn, "PRAGMA writable_schema=ON"); err != nil {
		return nil, err
	}
	var errs, warnings []any
	report := pyjson.ObjectOf("tables", pyjson.NewObject(), "errors", nil, "warnings", nil)
	mode, err := recoveryJournalMode(conn)
	if err != nil {
		if !isSQLiteDatabaseError(err) {
			return nil, err
		}
		mode = nil
		warnings = append(warnings, "journal mode: "+sessionsExcText(err))
	}
	report.Set("journal_mode", mode)
	tables := pyjson.NewObject()
	for _, table := range recoveryInventoryTables() {
		inventory, err := recoveryTableInventory(conn, table)
		if err != nil {
			return nil, err
		}
		tables.Set(table, inventory)
	}
	report.Set("tables", tables)
	for _, required := range []string{"sessions", "messages"} {
		entry, _ := tables.Get(required)
		obj := entry.(*pyjson.Object)
		rows, _ := obj.Get("rows")
		if !pyTruthyValue(obj.GetOr("available", nil)) || rows == nil {
			errs = append(errs, fmt.Sprintf("required table %s is not completely readable", required))
		}
	}
	report.Set("errors", recoveryList(errs))
	report.Set("warnings", recoveryList(warnings))
	report.Set("recoverable", len(errs) == 0)
	return report, nil
}

// Non nil JSON list
func recoveryList(items []any) []any {
	if items == nil {
		return []any{}
	}
	return items
}

// Inventory entry of a table in an inspection
func recoveryTableEntry(inspection *pyjson.Object, table string) *pyjson.Object {
	tables, _ := inspection.GetOr("tables", nil).(*pyjson.Object)
	if tables == nil {
		return pyjson.NewObject()
	}
	entry, _ := tables.GetOr(table, nil).(*pyjson.Object)
	if entry == nil {
		return pyjson.NewObject()
	}
	return entry
}

// Row count an inventory entry reports, nil when unknown
func recoveryEntryRows(entry *pyjson.Object) *int64 {
	if n, ok := entry.GetOr("rows", nil).(int64); ok {
		return &n
	}
	return nil
}

// Copies the source bundle into a fresh temp dir under the work root and inspects the copy
func recoverySnapshotAndInspect(source, workRoot string) (string, string, *pyjson.Object, error) {
	before, err := recoverySourceFingerprint(source)
	if err != nil {
		return "", "", nil, err
	}
	tempDir, err := os.MkdirTemp(workRoot, "icebath-session-recovery-")
	if err != nil {
		return "", "", nil, err
	}
	snapshotSource, inspection, err := func() (string, *pyjson.Object, error) {
		snapshotSource, copied, err := recoveryCopySourceBundle(source, tempDir)
		if err != nil {
			return "", nil, err
		}
		after, err := recoverySourceFingerprint(source)
		if err != nil {
			return "", nil, err
		}
		if !pyjson.Equal(after, before) {
			return "", nil, recoverySafetyError("The source database bundle changed while it was being copied. Stop every Icebath process using this " +
				"profile and retry. This includes the interactive `icebath` CLI session this command may have been " +
				"launched from: a running parent CLI writes session bookkeeping (compression ticks, context " +
				"tracking) to state.db in the background and counts as an Icebath process even after the gateway is " +
				"stopped. Run the recovery from a fresh shell with no `icebath` session open, or point --source at an " +
				"immutable snapshot copy of the database.")
		}
		conn, err := recoveryConnect(snapshotSource)
		if err != nil {
			return "", nil, err
		}
		inspection, err := recoveryInspectConnection(conn)
		if cerr := conn.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			return "", nil, err
		}
		inspection.Set("source_bundle", recoveryStrings(copied))
		inspection.Set("source_fingerprint", before)
		return snapshotSource, inspection, nil
	}()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return "", "", nil, err
	}
	return tempDir, snapshotSource, inspection, nil
}

// Inspects canonical table readability on a disposable copy without opening the source itself
func inspectSessionDatabase(ctx context.Context, sourcePath, workDir string) (*pyjson.Object, error) {
	source, _, workRoot, err := recoveryValidatePaths(sourcePath, "", workDir)
	if err != nil {
		return nil, err
	}
	diskSpace, err := recoveryDiskSpacePreflight(source, workRoot, "")
	if err != nil {
		return nil, err
	}
	tempDir, _, inspection, err := recoverySnapshotAndInspect(source, workRoot)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	report := pyjson.ObjectOf("operation", "inspect", "source", source, "disk_space", diskSpace)
	for _, key := range inspection.Keys {
		v, _ := inspection.Get(key)
		report.Set(key, v)
	}
	fingerprint, _ := inspection.Get("source_fingerprint")
	now, err := recoverySourceFingerprint(source)
	if err != nil {
		return nil, err
	}
	report.Set("source_unchanged", pyjson.Equal(now, fingerprint))
	return report, nil
}

// Current schema database at output opened on a plain connection with foreign keys off
func recoveryFreshDestination(ctx context.Context, output string, topicTables bool) (state.SQLiteConn, error) {
	db, err := state.NewSessionDB(ctx, output, false)
	if err != nil {
		return nil, err
	}
	if topicTables {
		if err := db.ApplyTelegramTopicMigration(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	conn, err := recoveryConnect(output)
	if err != nil {
		return nil, err
	}
	if _, err := recoveryExec(conn, "PRAGMA foreign_keys=OFF"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Copies one canonical table, straight chunked or by rowid range salvage
func recoveryCopyTable(source, destination state.SQLiteConn, table string, salvage bool, chunkSize int,
	progress func(*pyjson.Object), sourceRows *int64) (*pyjson.Object, error) {
	if table == "state_meta" {
		return recoveryCopyStateMeta(source, destination, salvage, chunkSize, progress, sourceRows)
	}
	if salvage {
		return recoveryCopyTableSalvage(source, destination, table, chunkSize, progress, sourceRows, "INSERT", nil)
	}
	result := pyjson.ObjectOf("source_rows", recoveryOptional(sourceRows), "copied_rows", int64(0))
	columns, ok, err := recoveryCompatibleColumns(source, destination, table, result)
	if err != nil || !ok {
		return result, err
	}
	quoted, marks := recoveryQuotedColumns(columns)
	return recoveryCopyRows(source, destination, fmt.Sprintf(`SELECT %s FROM "%s"`, quoted, table), nil,
		fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s)`, table, quoted, marks), table, chunkSize, progress, sourceRows, result)
}

// Records a skipped rowid range, merging an adjacent range with the same error
func recoveryAppendSkippedRange(result *pyjson.Object, low, high int64, msg string) {
	ranges, _ := result.GetOr("skipped_rowid_ranges", nil).([]any)
	if n := len(ranges); n > 0 {
		last := ranges[n-1].(*pyjson.Object)
		lastHigh, _ := last.GetOr("high", nil).(int64)
		if lastHigh != math.MaxInt64 && lastHigh+1 == low && pyjson.Equal(last.GetOr("error", nil), msg) {
			last.Set("high", high)
			return
		}
	}
	result.Set("skipped_rowid_ranges", append(ranges, pyjson.ObjectOf("low", low, "high", high, "error", msg)))
}

// Readable rowid edges of a table, galloping for a finite bound where an edge is damaged
func recoverySalvageRowidBounds(source state.SQLiteConn, table string) (*pyjson.Object, error) {
	var errs []any
	var fallback []any
	result := pyjson.ObjectOf("errors", []any{}, "fallback_edges", []any{})
	edges := map[string]*int64{"low": nil, "high": nil}
	for _, edge := range []struct{ name, direction string }{{"low", "ASC"}, {"high", "DESC"}} {
		row, ok, err := recoveryQueryOne(source, fmt.Sprintf(`SELECT rowid FROM "%s" ORDER BY rowid %s LIMIT 1`, table, edge.direction))
		if err != nil {
			if !isSQLiteDatabaseError(err) {
				return nil, err
			}
			errs = append(errs, fmt.Sprintf("%s rowid: %s", edge.name, sessionsExcText(err)))
			continue
		}
		if ok {
			v := recoveryInt(row[0])
			edges[edge.name] = &v
		}
	}
	result.Set("errors", recoveryList(errs))
	if edges["low"] == nil && edges["high"] == nil {
		if len(errs) == 0 {
			result.Set("empty", true)
		} else {
			result.Set("unavailable", true)
		}
		return result, nil
	}
	if edges["low"] == nil || edges["high"] == nil {
		row, _, err := recoveryQueryOne(source, fmt.Sprintf(`SELECT min(rowid), max(rowid) FROM "%s"`, table))
		if err != nil {
			if !isSQLiteDatabaseError(err) {
				return nil, err
			}
			errs = append(errs, "aggregate rowid bounds: "+sessionsExcText(err))
			result.Set("errors", errs)
		} else {
			var aggregate []any
			for i, name := range []string{"low", "high"} {
				if edges[name] == nil && row[i] != nil {
					v := recoveryInt(row[i])
					edges[name] = &v
					aggregate = append(aggregate, name)
				}
			}
			if aggregate != nil {
				result.Set("aggregate_edges", aggregate)
			}
		}
	}
	if edges["low"] == nil {
		v := int64(math.MinInt64)
		edges["low"] = &v
		fallback = append(fallback, "low")
	}
	if edges["high"] == nil {
		v := int64(math.MaxInt64)
		edges["high"] = &v
		fallback = append(fallback, "high")
	}
	result.Set("fallback_edges", recoveryList(fallback))
	result.Set("low", *edges["low"])
	result.Set("high", *edges["high"])
	if len(fallback) > 0 {
		var probes []any
		for _, pair := range [][2]string{{"high", "low"}, {"low", "high"}} {
			if !recoveryContainsAny(fallback, pair[0]) {
				continue
			}
			anchor, _ := result.GetOr(pair[1], nil).(int64)
			probe, err := recoveryProbePopulatedEdge(source, table, pair[0], anchor)
			if err != nil {
				return nil, err
			}
			probes = append(probes, probe)
			if pyTruthyValue(probe.GetOr("capped", nil)) {
				result.Set(pair[0], probe.GetOr("bound", nil))
			}
		}
		result.Set("edge_probes", recoveryList(probes))
	}
	return result, nil
}

// True when a JSON list holds the string
func recoveryContainsAny(list []any, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Finite bound for a damaged rowid edge galloping outward from the readable anchor with doubling offsets
func recoveryProbePopulatedEdge(source state.SQLiteConn, table, edge string, anchor int64) (*pyjson.Object, error) {
	ascending := edge == "high"
	comparison, order := ">", "ASC"
	domain := big.NewInt(math.MaxInt64)
	if !ascending {
		comparison, order = "<", "DESC"
		domain = big.NewInt(math.MinInt64)
	}
	probeSQL := fmt.Sprintf(`SELECT rowid FROM "%s" WHERE rowid %s ? ORDER BY rowid %s LIMIT 1`, table, comparison, order)
	result := pyjson.ObjectOf("edge", edge, "probes", int64(0), "capped", false)
	position := big.NewInt(anchor)
	span := big.NewInt(1)
	for {
		candidate := new(big.Int)
		if ascending {
			candidate.Add(position, span)
		} else {
			candidate.Sub(position, span)
		}
		if (ascending && candidate.Cmp(domain) >= 0) || (!ascending && candidate.Cmp(domain) <= 0) {
			result.Set("bound", domain.Int64())
			return result, nil
		}
		recoveryBump(result, "probes", 1)
		row, ok, err := recoveryQueryOne(source, probeSQL, candidate.Int64())
		if err != nil {
			if !isSQLiteDatabaseError(err) {
				return nil, err
			}
			span.Lsh(span, 1)
			continue
		}
		if !ok {
			result.Set("bound", candidate.Int64())
			result.Set("capped", true)
			return result, nil
		}
		position = big.NewInt(recoveryInt(row[0]))
		span.Lsh(span, 1)
	}
}

// Bisecting rowid range copy of one table, its counters kept on the shared result
type recoveryRangeSalvage struct {
	source, destination  state.SQLiteConn
	table                string
	chunkSize            int
	progress             func(*pyjson.Object)
	sourceRows           *int64
	rowFilter            func(row []any, columns []string) bool
	result               *pyjson.Object
	columnNames          []string
	selectSQL, insertSQL string
	exactSQL             string
	stoppedAtQueryLimit  bool
}

// Bisecting copy of one table with its select, insert and exact rowid statements, counters on result
func newRecoveryRangeSalvage(source, destination state.SQLiteConn, table string, columns []string, chunkSize int,
	progress func(*pyjson.Object), sourceRows *int64, insertPrefix string, rowFilter func(row []any, columns []string) bool,
	result *pyjson.Object) *recoveryRangeSalvage {
	quoted, marks := recoveryQuotedColumns(columns)
	return &recoveryRangeSalvage{source: source, destination: destination, table: table, chunkSize: chunkSize,
		progress: progress, sourceRows: sourceRows, rowFilter: rowFilter, result: result, columnNames: append([]string{}, columns...),
		selectSQL: fmt.Sprintf(`SELECT rowid, %s FROM "%s" WHERE rowid BETWEEN ? AND ? ORDER BY rowid`, quoted, table),
		insertSQL: fmt.Sprintf(`%s INTO "%s" (%s) VALUES (%s)`, insertPrefix, table, quoted, marks),
		exactSQL:  fmt.Sprintf(`SELECT %s FROM "%s" WHERE rowid = ?`, quoted, table)}
}

// Records a skipped rowid range on the shared result
func (s *recoveryRangeSalvage) skip(low, high int64, msg string) {
	recoveryAppendSkippedRange(s.result, low, high, msg)
}

// Rows the filter keeps
func (s *recoveryRangeSalvage) keep(values [][]any) [][]any {
	if s.rowFilter == nil {
		return values
	}
	var out [][]any
	for _, row := range values {
		if s.rowFilter(row, s.columnNames) {
			out = append(out, row)
		}
	}
	return out
}

// Salvages one row by exact key lookup, false when the lookup itself fails
func (s *recoveryRangeSalvage) recoverExactRowid(rowid int64) (bool, error) {
	recoveryBump(s.result, "range_queries", 1)
	row, ok, err := recoveryQueryOne(s.source, s.exactSQL, rowid)
	if err != nil {
		if !isSQLiteDatabaseError(err) {
			return false, err
		}
		return false, nil
	}
	if !ok {
		return true, nil
	}
	if len(s.keep([][]any{row})) == 0 {
		recoveryBump(s.result, "excluded_rows", 1)
		return true, nil
	}
	err = recoveryImmediate(s.destination, func() error {
		_, err := recoveryExec(s.destination, s.insertSQL, row...)
		return err
	})
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteIntegrityError {
			recoveryBump(s.result, "destination_rejected_rows", 1)
			s.skip(rowid, rowid, "destination constraint rejected row: "+sessionsExcText(err))
			return true, nil
		}
		return false, err
	}
	recoveryBump(s.result, "copied_rows", 1)
	recoveryBump(s.result, "exact_lookup_recovered", 1)
	return true, nil
}

// Copies the rowid range, bisecting the unread remainder on a read error and exact looking up singletons
func (s *recoveryRangeSalvage) copyRange(low, high int64) error {
	if low > high {
		return nil
	}
	if recoveryCounter(s.result, "range_queries") >= recoveryMaxSalvageQueries {
		s.stoppedAtQueryLimit = true
		s.skip(low, high, "salvage range query limit reached")
		return nil
	}
	recoveryBump(s.result, "range_queries", 1)
	var lastCommitted *int64
	readErr := func() error {
		rows, err := s.source.QueryContext(context.Background(), s.selectSQL, low, high)
		if err != nil {
			return err
		}
		defer rows.Close()
		for {
			fetched, err := recoveryFetchMany(rows, s.chunkSize)
			if err != nil {
				return err
			}
			if len(fetched) == 0 {
				return nil
			}
			values := make([][]any, len(fetched))
			for i, row := range fetched {
				values[i] = row[1:]
			}
			included := s.keep(values)
			if len(included) > 0 {
				if err := recoveryImmediate(s.destination, func() error {
					for _, row := range included {
						if _, err := recoveryExec(s.destination, s.insertSQL, row...); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					return err
				}
			}
			recoveryBump(s.result, "copied_rows", int64(len(included)))
			recoveryBump(s.result, "excluded_rows", int64(len(values)-len(included)))
			last := recoveryInt(fetched[len(fetched)-1][0])
			lastCommitted = &last
			if s.progress != nil {
				ranges, _ := s.result.GetOr("skipped_rowid_ranges", nil).([]any)
				s.progress(pyjson.ObjectOf("table", s.table, "copied_rows", recoveryCounter(s.result, "copied_rows"),
					"source_rows", recoveryOptional(s.sourceRows), "skipped_ranges", int64(len(ranges))))
			}
		}
	}()
	if readErr == nil {
		return nil
	}
	if !isSQLiteDatabaseError(readErr) {
		return readErr
	}
	retryLow := low
	if lastCommitted != nil {
		if *lastCommitted == math.MaxInt64 {
			return nil
		}
		retryLow = *lastCommitted + 1
	}
	if retryLow > high {
		return nil
	}
	if retryLow == high {
		ok, err := s.recoverExactRowid(retryLow)
		if err != nil {
			return err
		}
		if !ok {
			s.skip(retryLow, high, sessionsExcText(readErr))
		}
		return nil
	}
	midpoint := retryLow + int64((uint64(high)-uint64(retryLow))/2)
	if err := s.copyRange(retryLow, midpoint); err != nil {
		return err
	}
	return s.copyRange(midpoint+1, high)
}

// Rowid range copy that continues past damaged source pages
func recoveryCopyTableSalvage(source, destination state.SQLiteConn, table string, chunkSize int, progress func(*pyjson.Object),
	sourceRows *int64, insertPrefix string, rowFilter func([]any, []string) bool) (*pyjson.Object, error) {
	result := pyjson.ObjectOf("mode", "rowid_range_salvage", "source_rows", recoveryOptional(sourceRows), "copied_rows", int64(0),
		"excluded_rows", int64(0), "columns", []any{}, "range_queries", int64(0), "exact_lookup_recovered", int64(0),
		"destination_rejected_rows", int64(0), "skipped_rowid_ranges", []any{})
	columns, ok, err := recoveryCompatibleColumns(source, destination, table, result)
	if err != nil || !ok {
		return result, err
	}
	bounds, err := recoverySalvageRowidBounds(source, table)
	if err != nil {
		return nil, err
	}
	result.Set("rowid_bounds", bounds)
	if pyTruthyValue(bounds.GetOr("empty", nil)) {
		result.Set("status", "complete")
		return result, nil
	}
	low, lowOK := bounds.GetOr("low", nil).(int64)
	high, highOK := bounds.GetOr("high", nil).(int64)
	if !lowOK || !highOK {
		var details []string
		errs, _ := bounds.GetOr("errors", nil).([]any)
		for _, e := range errs {
			details = append(details, pyrepr.Str(e))
		}
		msg := "could not determine a rowid range for salvage"
		if len(details) > 0 {
			msg += ": " + strings.Join(details, "; ")
		}
		result.Set("status", "failed")
		result.Set("error", msg)
		return result, nil
	}
	salvage := newRecoveryRangeSalvage(source, destination, table, columns, chunkSize, progress, sourceRows, insertPrefix,
		rowFilter, result)
	if err := salvage.copyRange(low, high); err != nil {
		return nil, err
	}
	ranges, _ := result.GetOr("skipped_rowid_ranges", nil).([]any)
	span := new(big.Int)
	for _, item := range ranges {
		entry := item.(*pyjson.Object)
		lo, _ := entry.GetOr("low", nil).(int64)
		hi, _ := entry.GetOr("high", nil).(int64)
		span.Add(span, new(big.Int).Add(new(big.Int).Sub(big.NewInt(hi), big.NewInt(lo)), big.NewInt(1)))
	}
	if span.IsInt64() {
		result.Set("skipped_rowid_span", span.Int64())
	} else {
		result.Set("skipped_rowid_span", span)
	}
	result.Set("query_limit_reached", salvage.stoppedAtQueryLimit)
	copied, excluded := recoveryCounter(result, "copied_rows"), recoveryCounter(result, "excluded_rows")
	switch {
	case len(ranges) > 0:
		status := "failed"
		if copied != 0 {
			status = "partial"
		}
		result.Set("status", status)
		result.Set("error", fmt.Sprintf("%d rowid range(s) skipped", len(ranges)))
	case sourceRows != nil && copied+excluded != *sourceRows:
		result.Set("status", "partial")
		result.Set("error", fmt.Sprintf("copied %d and excluded %d of %d source rows", copied, excluded, *sourceRows))
	default:
		result.Set("status", "complete")
	}
	return result, nil
}

// Generated meta keys sorted
func recoverySortedGeneratedMeta() []any {
	keys := append([]string{}, recoveryGeneratedMeta...)
	sort.Strings(keys)
	return recoveryStrings(keys)
}

// Base state_meta result with any extra fields
func recoveryStateMetaResult(sourceRows *int64, extra ...any) *pyjson.Object {
	result := pyjson.ObjectOf("source_meta_rows", recoveryOptional(sourceRows), "copied_rows", int64(0),
		"columns", []any{"key", "value"}, "excluded_keys", recoverySortedGeneratedMeta())
	for i := 0; i+1 < len(extra); i += 2 {
		result.Set(extra[i].(string), extra[i+1])
	}
	return result
}

// Terminal state_meta result when the key value schema is unusable, nil otherwise
func recoveryStateMetaPrecheck(source, destination state.SQLiteConn, sourceRows *int64, salvage bool) (*pyjson.Object, error) {
	var extra []any
	if salvage {
		extra = []any{"mode", "rowid_range_salvage"}
	}
	sourceColumns, err := recoveryTableColumns(source, "state_meta")
	if err != nil {
		return nil, err
	}
	if !recoveryContains(sourceColumns, "key") || !recoveryContains(sourceColumns, "value") {
		if salvage && len(sourceColumns) > 0 {
			found := strings.Join(sourceColumns, ", ")
			return recoveryStateMetaResult(sourceRows, append(extra, "status", "failed", "error",
				"source state_meta exists but is missing the key/value columns (found: "+found+")")...), nil
		}
		return recoveryStateMetaResult(sourceRows, append(extra, "status", "missing")...), nil
	}
	destColumns, err := recoveryTableColumns(destination, "state_meta")
	if err != nil {
		return nil, err
	}
	if !recoveryContains(destColumns, "key") || !recoveryContains(destColumns, "value") {
		return recoveryStateMetaResult(sourceRows, append(extra, "status", "failed", "error", "destination state_meta schema is incomplete")...), nil
	}
	return nil, nil
}

// Copies user metadata rows, the derived FTS and topic keys regenerated rather than copied
func recoveryCopyStateMeta(source, destination state.SQLiteConn, salvage bool, chunkSize int, progress func(*pyjson.Object),
	sourceRows *int64) (*pyjson.Object, error) {
	problem, err := recoveryStateMetaPrecheck(source, destination, sourceRows, salvage)
	if err != nil || problem != nil {
		return problem, err
	}
	if salvage {
		keepUserMeta := func(row []any, columns []string) bool {
			for i, c := range columns {
				if c == "key" {
					return !recoveryContains(recoveryGeneratedMeta, pyrepr.Str(row[i]))
				}
			}
			return true
		}
		result, err := recoveryCopyTableSalvage(source, destination, "state_meta", chunkSize, progress, sourceRows, "INSERT OR REPLACE", keepUserMeta)
		if err != nil {
			return nil, err
		}
		v, _ := result.Pop("source_rows")
		result.Set("source_meta_rows", v)
		result.Set("excluded_keys", recoverySortedGeneratedMeta())
		return result, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(recoveryGeneratedMeta)), ", ")
	params := make([]any, len(recoveryGeneratedMeta))
	for i, k := range recoveryGeneratedMeta {
		params[i] = k
	}
	var filtered *int64
	if row, ok, err := recoveryQueryOne(source, "SELECT COUNT(*) FROM state_meta WHERE key NOT IN ("+marks+")", params...); err == nil && ok {
		n := recoveryInt(row[0])
		filtered = &n
	} else if err != nil && !isSQLiteDatabaseError(err) {
		return nil, err
	}
	return recoveryCopyRows(source, destination, "SELECT key, value FROM state_meta WHERE key NOT IN ("+marks+")", params,
		"INSERT OR REPLACE INTO state_meta(key, value) VALUES (?, ?)", "state_meta", chunkSize, progress, filtered, recoveryStateMetaResult(sourceRows))
}

// Generator of unused synthetic titles of the form [prefix N] session metadata was unreadable
func recoverySyntheticTitles(destination state.SQLiteConn, prefix string) func() (string, error) {
	sequence := 1
	return func() (string, error) {
		for {
			title := fmt.Sprintf("[%s %d] session metadata was unreadable", prefix, sequence)
			sequence++
			_, found, err := recoveryQueryOne(destination, "SELECT 1 FROM sessions WHERE title = ? LIMIT 1", title)
			if err != nil {
				return "", err
			}
			if !found {
				return title, nil
			}
		}
	}
}

// Recreates synthetic sessions for salvaged messages whose session row did not survive
func recoveryReconstructMissingSessions(destination state.SQLiteConn) (*pyjson.Object, error) {
	result := pyjson.ObjectOf("sessions_reconstructed", int64(0), "messages_retained", int64(0))
	sessionColumns, err := recoveryTableColumns(destination, "sessions")
	if err != nil {
		return nil, err
	}
	messageColumns, err := recoveryTableColumns(destination, "messages")
	if err != nil {
		return nil, err
	}
	if len(sessionColumns) == 0 || len(messageColumns) == 0 {
		return result, nil
	}
	orphaned, err := recoveryQueryAll(destination, "SELECT m.session_id, MIN(m.timestamp), COUNT(*) FROM messages AS m WHERE m.session_id IS NOT NULL AND NOT "+
		"EXISTS (SELECT 1 FROM sessions WHERE sessions.id = m.session_id) GROUP BY m.session_id")
	if err != nil || len(orphaned) == 0 {
		return result, err
	}
	titles := recoverySyntheticTitles(destination, "recovered")
	for _, row := range orphaned {
		startedAt := 0.0
		if row[1] != nil {
			startedAt = recoveryFloat(row[1])
		}
		title, err := titles()
		if err != nil {
			return nil, err
		}
		count := recoveryInt(row[2])
		n, err := recoveryExec(destination, "INSERT INTO sessions (id, source, started_at, title, message_count) VALUES (?, 'recovered', ?, ?, ?)",
			row[0], startedAt, title, count)
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, &state.SQLiteError{Class: state.SQLiteIntegrityError, Msg: "failed to reconstruct missing session " + pyrepr.Repr(row[0])}
		}
		recoveryBump(result, "sessions_reconstructed", 1)
		recoveryBump(result, "messages_retained", count)
	}
	return result, nil
}

// Python float of a SQLite value
func recoveryFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case string:
		f, _ := pyFloatOf(x)
		return f
	}
	return 0
}

// Python float of text, false when float rejects it
func pyFloatOf(text string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return f, err == nil
}

// Counts rows matching where and runs the mutation over them only when there are any
func recoveryReconcile(destination state.SQLiteConn, table, where, mutation string) (int64, error) {
	row, _, err := recoveryQueryOne(destination, fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE %s`, table, where))
	if err != nil {
		return 0, err
	}
	count := recoveryInt(row[0])
	if count != 0 {
		if _, err := recoveryExec(destination, mutation+" WHERE "+where); err != nil {
			return 0, err
		}
	}
	return count, nil
}

// Reconciles references to unsalvageable sessions after rebuilding synthetic owners for orphaned messages
func recoveryCleanupPartialOrphans(destination state.SQLiteConn) (*pyjson.Object, error) {
	result := pyjson.ObjectOf("session_prompt_refs_cleared", int64(0), "system_prompts_removed", int64(0), "sessions_parent_cleared", int64(0),
		"sessions_reconstructed", int64(0), "messages_retained", int64(0))
	for _, table := range recoveryDependentTables {
		result.Set(table+"_removed", int64(0))
	}
	err := recoveryImmediate(destination, func() error {
		rebuilt, err := recoveryReconstructMissingSessions(destination)
		if err != nil {
			return err
		}
		result.Update(rebuilt)
		n, err := recoveryReconcile(destination, "sessions", "parent_session_id IS NOT NULL AND NOT EXISTS ("+
			"SELECT 1 FROM sessions AS parent WHERE parent.id = sessions.parent_session_id)", "UPDATE sessions SET parent_session_id = NULL")
		if err != nil {
			return err
		}
		result.Set("sessions_parent_cleared", n)
		n, err = recoveryReconcile(destination, "sessions", "system_prompt_hash IS NOT NULL AND NOT EXISTS ("+
			"SELECT 1 FROM system_prompts WHERE system_prompts.hash = sessions.system_prompt_hash)", "UPDATE sessions SET system_prompt_hash = NULL")
		if err != nil {
			return err
		}
		result.Set("session_prompt_refs_cleared", n)
		n, err = recoveryReconcile(destination, "sessions", recoveryDanglingToolPin, "UPDATE sessions SET tool_names = NULL")
		if err != nil {
			return err
		}
		recoveryBump(result, "session_prompt_refs_cleared", n)
		n, err = recoveryReconcile(destination, "system_prompts",
			"NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.system_prompt_hash = system_prompts.hash) "+
				"AND NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.tool_names = system_prompts.hash)", "DELETE FROM system_prompts")
		if err != nil {
			return err
		}
		result.Set("system_prompts_removed", n)
		for _, table := range recoveryDependentTables {
			columns, err := recoveryTableColumns(destination, table)
			if err != nil {
				return err
			}
			if len(columns) == 0 {
				continue
			}
			n, err := recoveryReconcile(destination, table,
				fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = "%s".session_id)`, table), fmt.Sprintf(`DELETE FROM "%s"`, table))
			if err != nil {
				return err
			}
			result.Set(table+"_removed", n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	total := int64(0)
	for _, key := range recoveryRelinkCounters {
		total += recoveryCounter(result, key)
	}
	for _, table := range recoveryDependentTables {
		total += recoveryCounter(result, table+"_removed")
	}
	result.Set("total_removed_or_relinked", total)
	return result, nil
}

// Appends a verification error
func recoveryVerifyError(verification *pyjson.Object, msg string) {
	errs, _ := verification.GetOr("errors", nil).([]any)
	verification.Set("errors", append(errs, msg))
}

// Appends a verification warning
func recoveryVerifyWarning(verification *pyjson.Object, msg string) {
	warnings, _ := verification.GetOr("warnings", nil).([]any)
	verification.Set("warnings", append(warnings, msg))
}

// Integrity, foreign keys, journal mode, schema version and FTS meta of the recovered file
func recoveryVerifyStructure(conn state.SQLiteConn, verification *pyjson.Object) error {
	rows, err := recoveryQueryAll(conn, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	var integrity []any
	for _, row := range rows {
		integrity = append(integrity, pyrepr.Str(row[0]))
	}
	verification.Set("integrity_check", recoveryList(integrity))
	if len(integrity) != 1 || integrity[0] != "ok" {
		recoveryVerifyError(verification, "PRAGMA integrity_check did not return exactly 'ok'")
	}
	fkRows, err := recoveryQueryAll(conn, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	var fks []any
	for _, row := range fkRows {
		fks = append(fks, append([]any{}, row...))
	}
	verification.Set("foreign_key_check", recoveryList(fks))
	if len(fks) > 0 {
		recoveryVerifyError(verification, "foreign key violations remain")
	}
	mode, err := recoveryJournalMode(conn)
	if err != nil {
		return err
	}
	verification.Set("journal_mode", mode)
	schemaRow, ok, err := recoveryQueryOne(conn, "SELECT version FROM schema_version LIMIT 1")
	if err != nil {
		return err
	}
	var schemaVersion any
	if ok {
		schemaVersion = recoveryInt(schemaRow[0])
	}
	verification.Set("schema_version", schemaVersion)
	if schemaVersion != int64(state.SchemaVersion) {
		shown := "None"
		if schemaVersion != nil {
			shown = fmt.Sprint(schemaVersion)
		}
		recoveryVerifyError(verification, fmt.Sprintf("schema version is %s, expected %d", shown, state.SchemaVersion))
	}
	metaRows, err := recoveryQueryAll(conn, "SELECT key, value FROM state_meta WHERE key LIKE 'fts_%'")
	if err != nil {
		return err
	}
	meta := pyjson.NewObject()
	for _, row := range metaRows {
		meta.Set(pyrepr.Str(row[0]), row[1])
	}
	verification.Set("fts_meta", meta)
	if !pyjson.Equal(meta.GetOr("fts_storage_version", nil), fmt.Sprint(state.FTSStorageVersion)) {
		recoveryVerifyError(verification, "fresh FTS storage version was not established")
	}
	var pending []string
	for _, key := range recoveryGeneratedMeta {
		if _, present := meta.Get(key); strings.HasPrefix(key, "fts_") && key != "fts_storage_version" && present {
			pending = append(pending, key)
		}
	}
	sort.Strings(pending)
	verification.Set("pending_fts_keys", recoveryStrings(pending))
	if len(pending) > 0 {
		recoveryVerifyError(verification, "derived FTS transition markers remain in the recovered database")
	}
	return nil
}

// Integrity command and empty match probe of each FTS index present
func recoveryVerifyFTSIndexes(conn state.SQLiteConn, verification *pyjson.Object) error {
	checks := pyjson.NewObject()
	for _, table := range []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"} {
		columns, err := recoveryTableColumns(conn, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			continue
		}
		_, err = recoveryExec(conn, fmt.Sprintf(`INSERT INTO "%s" ("%s") VALUES ('integrity-check')`, table, table))
		if err == nil {
			_, _, err = recoveryQueryOne(conn, fmt.Sprintf(`SELECT 1 FROM "%s" WHERE "%s" MATCH '""' LIMIT 1`, table, table))
		}
		if err != nil {
			if !isSQLiteDatabaseError(err) {
				return err
			}
			checks.Set(table, sessionsExcText(err))
			recoveryVerifyError(verification, fmt.Sprintf("%s integrity check failed: %s", table, sessionsExcText(err)))
			continue
		}
		checks.Set(table, "ok")
	}
	verification.Set("fts_checks", checks)
	return nil
}

// Compares recovered counts and copy statuses against the source, classifying shortfalls as loss
func recoveryVerifyRowCounts(conn state.SQLiteConn, verification *pyjson.Object, expected map[string]*int64,
	copyReport *pyjson.Object, allowPartial bool, orphanCleanup *pyjson.Object) error {
	flag := func(msg string, soft bool) {
		if soft {
			recoveryVerifyWarning(verification, msg)
			verification.Set("loss_detected", true)
			return
		}
		recoveryVerifyError(verification, msg)
	}
	counts := pyjson.NewObject()
	for _, table := range recoveryInventoryTables() {
		columns, err := recoveryTableColumns(conn, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			continue
		}
		n, err := recoveryCountRows(conn, table)
		if err != nil {
			return err
		}
		counts.Set(table, n)
	}
	verification.Set("table_counts", counts)
	countOf := func(table string) any { return counts.GetOr(table, nil) }
	for _, table := range append([]string{"sessions", "messages"}, recoveryAuxiliaryTables...) {
		want := expected[table]
		if want != nil && !pyjson.Equal(countOf(table), *want) {
			shown := "None"
			if v := countOf(table); v != nil {
				shown = fmt.Sprint(v)
			}
			flag(fmt.Sprintf("%s count is %s, expected %d", table, shown, *want), allowPartial)
		}
	}
	cleanup := orphanCleanup
	if cleanup == nil {
		cleanup = pyjson.NewObject()
	}
	rebuilt := recoveryInt(cleanup.GetOr("sessions_reconstructed", nil))
	retained := recoveryInt(cleanup.GetOr("messages_retained", nil))
	removed := recoveryInt(cleanup.GetOr("messages_removed", nil))
	fullyReconstructed := rebuilt > 0 && pyjson.Equal(countOf("sessions"), rebuilt) && pyjson.Equal(countOf("messages"), retained) && removed == 0
	for _, table := range copyReport.Keys {
		entry, _ := copyReport.GetOr(table, nil).(*pyjson.Object)
		status := entry.GetOr("status", nil)
		if status != "failed" && status != "partial" {
			continue
		}
		tolerable := status == "partial" || (table != "sessions" && table != "messages") || (table == "sessions" && fullyReconstructed)
		flag(fmt.Sprintf("%s copy status is %s", table, status), allowPartial && tolerable)
	}
	if orphanCleanup != nil && orphanCleanup.Len() > 0 {
		if orphanCount := recoveryInt(orphanCleanup.GetOr("total_removed_or_relinked", nil)); orphanCount != 0 {
			flag(fmt.Sprintf("%d orphaned reference(s) were removed or relinked", orphanCount), true)
		}
		if rebuilt != 0 {
			flag(fmt.Sprintf("%d session(s) could not be salvaged and were reconstructed as placeholders to "+
				"retain %d message(s); their metadata (title, model, timestamps, cost) is lost", rebuilt, retained), true)
		}
	}
	return nil
}

// Health probe, structure, counts and FTS checks of the recovered output
func recoveryVerifyRecoveredDatabase(ctx context.Context, output string, expected map[string]*int64, copyReport *pyjson.Object,
	allowPartial bool, orphanCleanup *pyjson.Object) (*pyjson.Object, error) {
	verification := pyjson.ObjectOf("errors", []any{}, "warnings", []any{}, "loss_detected", false)
	reason, unhealthy, err := state.DBOpensCleanly(ctx, output)
	if err != nil {
		return nil, err
	}
	verification.Set("opens_cleanly", !unhealthy)
	if unhealthy {
		recoveryVerifyError(verification, "database health probe: "+reason)
	}
	conn, err := recoveryConnectDefault(output)
	if err != nil {
		return nil, err
	}
	verifyErr := func() error {
		if err := recoveryVerifyStructure(conn, verification); err != nil {
			return err
		}
		if err := recoveryVerifyRowCounts(conn, verification, expected, copyReport, allowPartial, orphanCleanup); err != nil {
			return err
		}
		return recoveryVerifyFTSIndexes(conn, verification)
	}()
	closeErr := conn.Close()
	if verifyErr != nil {
		if !isSQLiteDatabaseError(verifyErr) {
			return nil, verifyErr
		}
		recoveryVerifyError(verification, "verification query failed: "+sessionsExcText(verifyErr))
	}
	if closeErr != nil {
		return nil, closeErr
	}
	errs, _ := verification.GetOr("errors", nil).([]any)
	healthy := len(errs) == 0
	verification.Set("healthy", healthy)
	verification.Set("complete", healthy && !pyTruthyValue(verification.GetOr("loss_detected", nil)))
	return verification, nil
}

// Rewrites unparseable sessions.model_config blobs to an empty object, returning the row count
func recoverySanitizeModelConfig(destination state.SQLiteConn) (int64, error) {
	columns, err := recoveryTableColumns(destination, "sessions")
	if err != nil || !recoveryContains(columns, "model_config") {
		return 0, err
	}
	var n int64
	err = recoveryImmediate(destination, func() error {
		var rerr error
		n, rerr = recoveryReconcile(destination, "sessions", "model_config IS NOT NULL AND json_valid(model_config) = 0",
			"UPDATE sessions SET model_config = '{}'")
		return rerr
	})
	return n, err
}

// Rewrites message and session timestamps outside the epoch window, returning the count
func recoveryRepairOutOfWindowTimestamps(destination state.SQLiteConn) (int64, error) {
	messageColumns, err := recoveryTableColumns(destination, "messages")
	if err != nil {
		return 0, err
	}
	sessionColumns, err := recoveryTableColumns(destination, "sessions")
	if err != nil {
		return 0, err
	}
	ok := "BETWEEN " + pyrepr.Repr(state.EpochMin) + " AND " + pyrepr.Repr(state.EpochMax)
	now := "CAST(strftime('%s', 'now') AS REAL)"
	var repaired int64
	err = recoveryImmediate(destination, func() error {
		if recoveryContains(messageColumns, "timestamp") {
			n, err := recoveryReconcile(destination, "messages", "NOT (timestamp "+ok+")",
				"UPDATE messages SET timestamp = COALESCE("+
					"(SELECT p.timestamp FROM messages p WHERE p.session_id = messages.session_id AND p.id < messages.id"+
					" AND p.timestamp "+ok+" ORDER BY p.id DESC LIMIT 1),"+
					"(SELECT n.timestamp FROM messages n WHERE n.session_id = messages.session_id AND n.id > messages.id"+
					" AND n.timestamp "+ok+" ORDER BY n.id LIMIT 1),"+
					"(SELECT s.started_at FROM sessions s WHERE s.id = messages.session_id AND s.started_at "+ok+"), "+now+")")
			if err != nil {
				return err
			}
			repaired += n
		}
		if recoveryContains(sessionColumns, "started_at") {
			n, err := recoveryReconcile(destination, "sessions", "NOT (started_at "+ok+")",
				"UPDATE sessions SET started_at = COALESCE((SELECT MIN(m.timestamp) FROM messages m"+
					" WHERE m.session_id = sessions.id AND m.timestamp "+ok+"), "+now+")")
			if err != nil {
				return err
			}
			repaired += n
		}
		for _, column := range []string{"ended_at", "last_activity_at"} {
			if recoveryContains(sessionColumns, column) {
				n, err := recoveryReconcile(destination, "sessions", "NOT ("+column+" "+ok+")", "UPDATE sessions SET "+column+" = NULL")
				if err != nil {
					return err
				}
				repaired += n
			}
		}
		return nil
	})
	return repaired, err
}

// Sanitizes copied JSON and timestamps and stamps the FTS storage version the fresh destination owns
func recoveryFinalizeDerivedMetadata(destination state.SQLiteConn) (*pyjson.Object, error) {
	modelConfigReset, err := recoverySanitizeModelConfig(destination)
	if err != nil {
		return nil, err
	}
	timestampsRepaired, err := recoveryRepairOutOfWindowTimestamps(destination)
	if err != nil {
		return nil, err
	}
	rows, err := recoveryQueryAll(destination, "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('messages_fts', 'messages_fts_trigram')")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, row := range rows {
		present[pyrepr.Str(row[0])] = true
	}
	var names []string
	for name := range present {
		names = append(names, name)
	}
	sort.Strings(names)
	result := pyjson.ObjectOf("fts_tables", recoveryStrings(names), "finalized", false, "model_config_reset", modelConfigReset,
		"timestamps_repaired", timestampsRepaired)
	if len(present) != 2 || !present["messages_fts"] || !present["messages_fts_trigram"] {
		result.Set("error", "fresh destination is missing required FTS tables")
		return result, nil
	}
	var ftsKeys []any
	for _, key := range recoveryGeneratedMeta {
		if strings.HasPrefix(key, "fts_") {
			ftsKeys = append(ftsKeys, key)
		}
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(ftsKeys)), ", ")
	if err := recoveryImmediate(destination, func() error {
		if _, err := recoveryExec(destination, "DELETE FROM state_meta WHERE key IN ("+marks+")", ftsKeys...); err != nil {
			return err
		}
		_, err := recoveryExec(destination, "INSERT INTO state_meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			"fts_storage_version", fmt.Sprint(state.FTSStorageVersion))
		return err
	}); err != nil {
		return nil, err
	}
	result.Set("finalized", true)
	return result, nil
}

// Recover report with the shared header, mode fields and verdict, a changed source clearing the named verification flag
func recoveryReport(source, output string, inspection, diskSpace, verification *pyjson.Object, onSourceChange string, fields ...any) (*pyjson.Object, error) {
	fingerprint := inspection.GetOr("source_fingerprint", nil)
	now, err := recoverySourceFingerprint(source)
	if err != nil {
		return nil, err
	}
	unchanged := pyjson.Equal(now, fingerprint)
	if !unchanged {
		recoveryVerifyError(verification, "the source database bundle changed during recovery")
		verification.Set(onSourceChange, false)
	}
	report := pyjson.ObjectOf("operation", "recover", "source", source, "output", output,
		"source_bundle", inspection.GetOr("source_bundle", nil), "source_fingerprint", fingerprint,
		"source_unchanged", unchanged, "disk_space", diskSpace,
		"inspection", pyjson.ObjectOf("journal_mode", inspection.GetOr("journal_mode", nil), "tables", inspection.GetOr("tables", nil),
			"errors", inspection.GetOr("errors", nil), "warnings", inspection.GetOr("warnings", nil)))
	for i := 0; i+1 < len(fields); i += 2 {
		report.Set(fields[i].(string), fields[i+1])
	}
	excluded := pyjson.NewObject()
	for _, table := range recoveryRuntimeTables {
		excluded.Set(table, recoveryTableEntry(inspection, table))
	}
	report.Set("verification", verification)
	report.Set("complete", pyTruthyValue(verification.GetOr("complete", nil)) && unchanged)
	report.Set("partial", pyTruthyValue(verification.GetOr("loss_detected", nil)))
	report.Set("verified", pyTruthyValue(verification.GetOr("healthy", nil)) && unchanged)
	report.Set("installed", false)
	report.Set("runtime_state", pyjson.ObjectOf("policy", "transcript-only; runtime ledger excluded; fresh epoch on activation",
		"excluded_tables", excluded))
	return report, nil
}

// Salvages canonical rows into a separate output, a state.db output first reserving offline ownership of its home
func recoverSessionDatabase(ctx context.Context, sourcePath, outputPath, workDir string, chunkSize int, progress func(*pyjson.Object), allowPartial bool) (*pyjson.Object, error) {
	_, output, _, err := recoveryValidatePaths(sourcePath, outputPath, workDir)
	if err != nil {
		return nil, err
	}
	if filepath.Base(output) == "state.db" {
		guarded, release, err := gateway.ExclusiveMaintenance(ctx, []string{filepath.Dir(output)}, "")
		var conflict *gateway.OwnershipConflict
		if errors.As(err, &conflict) {
			return nil, recoverySafetyError(conflict.Error())
		}
		if err != nil {
			return nil, err
		}
		defer release()
		ctx = guarded
	}
	report, err := recoverSessionDatabaseOutput(ctx, sourcePath, outputPath, workDir, chunkSize, progress, allowPartial)
	var conflict *gateway.OwnershipConflict
	if errors.As(err, &conflict) {
		return nil, recoverySafetyError(conflict.Error())
	}
	return report, err
}

// Recovers canonical rows into a separate current schema database from a copy of the source bundle
func recoverSessionDatabaseOutput(ctx context.Context, sourcePath, outputPath, workDir string, chunkSize int, progress func(*pyjson.Object), allowPartial bool) (*pyjson.Object, error) {
	if chunkSize <= 0 {
		return nil, recoverySafetyError("chunk_size must be greater than zero")
	}
	source, output, workRoot, err := recoveryValidatePaths(sourcePath, outputPath, workDir)
	if err != nil {
		return nil, err
	}
	diskSpace, err := recoveryDiskSpacePreflight(source, workRoot, filepath.Dir(output))
	if err != nil {
		return nil, err
	}
	tempDir, snapshotSource, inspection, err := recoverySnapshotAndInspect(source, workRoot)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	if !pyTruthyValue(inspection.GetOr("recoverable", nil)) && !allowPartial {
		var reasons []string
		errs, _ := inspection.GetOr("errors", nil).([]any)
		for _, e := range errs {
			reasons = append(reasons, pyrepr.Str(e))
		}
		if len(reasons) == 0 {
			reasons = []string{"unknown source error"}
		}
		return nil, recoverySourceError("Required canonical tables are not readable: " + strings.Join(reasons, "; ") + ". " +
			"Re-run with --allow-partial to salvage every readable row " +
			"into a new database (the source is never modified).")
	}
	var missingRequired []string
	for _, table := range []string{"sessions", "messages"} {
		if !pyTruthyValue(recoveryTableEntry(inspection, table).GetOr("available", nil)) {
			missingRequired = append(missingRequired, table)
		}
	}
	if allowPartial && len(missingRequired) > 0 {
		return recoverViaLostAndFound(ctx, source, snapshotSource, tempDir, output, inspection, diskSpace, missingRequired)
	}
	sourceConn, err := recoveryConnect(snapshotSource)
	if err != nil {
		return nil, err
	}
	copyReport := pyjson.NewObject()
	var orphanCleanup, derived *pyjson.Object
	copyErr := func() error {
		defer sourceConn.Close()
		if _, err := recoveryExec(sourceConn, "PRAGMA writable_schema=ON"); err != nil {
			return err
		}
		topic := false
		for _, table := range recoveryTopicTables {
			if pyTruthyValue(recoveryTableEntry(inspection, table).GetOr("available", nil)) {
				topic = true
			}
		}
		destination, err := recoveryFreshDestination(ctx, output, topic)
		if err != nil {
			return err
		}
		defer destination.Close()
		tables := append(append(append(append([]string{}, recoveryCanonicalTables...), "state_meta"), recoveryTopicTables...), recoveryAuxiliaryTables...)
		for _, table := range tables {
			entry := recoveryTableEntry(inspection, table)
			if !recoveryContains(recoveryCanonicalTables, table) && !pyTruthyValue(entry.GetOr("available", nil)) {
				copyReport.Set(table, pyjson.ObjectOf("status", "missing", "copied_rows", int64(0)))
				continue
			}
			if init, ok := recoveryAuxiliarySchemas[table]; ok {
				if err := init(ctx, destination); err != nil {
					return err
				}
			}
			result, err := recoveryCopyTable(sourceConn, destination, table, allowPartial, chunkSize, progress, recoveryEntryRows(entry))
			if err != nil {
				return err
			}
			copyReport.Set(table, result)
		}
		if allowPartial {
			if orphanCleanup, err = recoveryCleanupPartialOrphans(destination); err != nil {
				return err
			}
		}
		derived, err = recoveryFinalizeDerivedMetadata(destination)
		return err
	}()
	if copyErr != nil {
		return nil, copyErr
	}
	expected := map[string]*int64{}
	for _, table := range append(append([]string{}, recoveryCanonicalTables...), recoveryAuxiliaryTables...) {
		entry := recoveryTableEntry(inspection, table)
		if recoveryContains(recoveryCanonicalTables, table) || pyTruthyValue(entry.GetOr("available", nil)) {
			expected[table] = recoveryEntryRows(entry)
		}
	}
	verification, err := recoveryVerifyRecoveredDatabase(ctx, output, expected, copyReport, allowPartial, orphanCleanup)
	if err != nil {
		return nil, err
	}
	var cleanupValue any
	if orphanCleanup != nil {
		cleanupValue = orphanCleanup
	}
	return recoveryReport(source, output, inspection, diskSpace, verification, "complete",
		"allow_partial", allowPartial, "copy", copyReport, "orphan_cleanup", cleanupValue, "derived_metadata", derived)
}

// Writes the JSON report to a new file, refusing to overwrite an existing one
func writeRecoveryReport(path string, report *pyjson.Object) (string, error) {
	destination, err := recoveryResolvedOutput(path)
	if err != nil {
		return "", err
	}
	text, err := pyjson.Dumps(report, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	handle, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return "", err
	}
	if _, err := handle.WriteString(text + "\n"); err != nil {
		handle.Close()
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	return destination, nil
}
