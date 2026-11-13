package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	sqlite3 "modernc.org/sqlite/lib"
)

// Usage table indexes a rebuild restores
var sessionModelUsageIndexSQL = []string{
	"CREATE INDEX IF NOT EXISTS idx_session_model_usage_session ON session_model_usage(session_id)",
	"CREATE INDEX IF NOT EXISTS idx_session_model_usage_model ON session_model_usage(model)",
}

// Usage table definition the primary key heal recreates it with
const sessionModelUsageHealDDL = `CREATE TABLE session_model_usage (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    billing_provider TEXT NOT NULL DEFAULT '',
    billing_base_url TEXT NOT NULL DEFAULT '',
    billing_mode TEXT NOT NULL DEFAULT '',
    task TEXT NOT NULL DEFAULT '',
    api_call_count INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    estimated_cost_usd REAL NOT NULL DEFAULT 0,
    actual_cost_usd REAL NOT NULL DEFAULT 0,
    cost_status TEXT,
    cost_source TEXT,
    first_seen REAL,
    last_seen REAL,
    PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)
)`

// The v22 migration's rendering of the same table, column lines at 35 spaces and the paren at 31
var sessionModelUsageV22DDL = func() string {
	lines := strings.Split(sessionModelUsageHealDDL, "\n")
	out := []string{lines[0]}
	for _, ln := range lines[1 : len(lines)-1] {
		out = append(out, strings.Repeat(" ", 35)+strings.TrimSpace(ln))
	}
	return strings.Join(append(out, strings.Repeat(" ", 31)+")"), "\n")
}()

// v20 seed of one usage row per historical session from its aggregate totals, newer rows winning
const sessionModelUsageV20SeedSQL = `INSERT OR IGNORE INTO session_model_usage (
                               session_id, model, billing_provider,
                               billing_base_url, billing_mode,
                               api_call_count, input_tokens,
                               output_tokens, cache_read_tokens,
                               cache_write_tokens, reasoning_tokens,
                               estimated_cost_usd, actual_cost_usd,
                               cost_status, cost_source, first_seen, last_seen
                           )
                           SELECT id, COALESCE(model, 'unknown'),
                                  COALESCE(billing_provider, ''),
                                  COALESCE(billing_base_url, ''),
                                  COALESCE(billing_mode, ''),
                                  COALESCE(api_call_count, 0),
                                  COALESCE(input_tokens, 0),
                                  COALESCE(output_tokens, 0),
                                  COALESCE(cache_read_tokens, 0),
                                  COALESCE(cache_write_tokens, 0),
                                  COALESCE(reasoning_tokens, 0),
                                  COALESCE(estimated_cost_usd, 0),
                                  COALESCE(actual_cost_usd, 0),
                                  cost_status, cost_source,
                                  started_at, COALESCE(ended_at, started_at)
                           FROM sessions
                           WHERE COALESCE(input_tokens, 0)
                                 + COALESCE(output_tokens, 0)
                                 + COALESCE(cache_read_tokens, 0)
                                 + COALESCE(cache_write_tokens, 0)
                                 + COALESCE(reasoning_tokens, 0) > 0`

// Unique title index older stores may first need their duplicates repaired for
const titleUniqueIndexSQL = "CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_title_unique ON sessions(title) WHERE title IS NOT NULL"

// Primary key column names of table in key order, nil when the table is missing
func livePKColumns(ctx context.Context, c Conn, table string) ([]string, error) {
	cols, err := tableInfo(ctx, c, table)
	if err != nil {
		if familyOf(err) == callFamily {
			return nil, nil
		}
		return nil, err
	}
	if len(cols) == 0 {
		return nil, nil
	}
	var keyed []columnInfo
	for _, col := range cols {
		if col.pk > 0 {
			keyed = append(keyed, col)
		}
	}
	sort.SliceStable(keyed, func(i, j int) bool { return keyed[i].pk < keyed[j].pk })
	out := make([]string, len(keyed))
	for i, col := range keyed {
		out[i] = col.name
	}
	return out, nil
}

// Reports whether a transaction is open on the connection c runs on
func connInTransaction(c Conn) (bool, error) {
	r, ok := c.(*runner)
	if !ok {
		return false, fmt.Errorf("%T exposes no SQLite handle", c)
	}
	return r.c.inTransaction()
}

// Renames table aside, recreates it, copies rows back, drops the copy and restores indexes as one write transaction
func rebuildTable(ctx context.Context, c Conn, table, legacy, ddl, copySQL string, indexes []string) error {
	steps := func() error {
		for _, q := range append([]string{"ALTER TABLE " + table + " RENAME TO " + legacy, ddl, copySQL, "DROP TABLE " + legacy}, indexes...) {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
	open, err := connInTransaction(c)
	if err != nil {
		return err
	}
	if open {
		return steps()
	}
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := steps(); err != nil {
		_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

// Rebuilds gateway_routing when its primary key predates the scope column, the newest row winning a key collision
func healGatewayRoutingPK(ctx context.Context, c Conn) error {
	pk, err := livePKColumns(ctx, c, "gateway_routing")
	if err != nil || pk == nil || (len(pk) == 2 && pk[0] == "scope" && pk[1] == "session_key") {
		return err
	}
	slog.Info(fmt.Sprintf("gateway_routing has legacy primary key %q; rebuilding with composite (scope, session_key) key", pk))
	return rebuildTable(ctx, c, "gateway_routing", "gateway_routing_legacy_pk", `CREATE TABLE gateway_routing (
    scope TEXT NOT NULL DEFAULT '',
    session_key TEXT NOT NULL,
    entry_json TEXT NOT NULL,
    updated_at REAL NOT NULL,
    PRIMARY KEY (scope, session_key)
)`, "INSERT OR REPLACE INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
		"SELECT COALESCE(scope, ''), session_key, entry_json, updated_at "+
		"FROM gateway_routing_legacy_pk ORDER BY updated_at ASC", nil)
}

// Rebuilds session_model_usage when its primary key lacks task, foreign keys off for the copy
func healSessionModelUsagePK(ctx context.Context, c Conn) error {
	pk, err := livePKColumns(ctx, c, "session_model_usage")
	if err != nil || pk == nil || containsString(pk, "task") {
		return err
	}
	sortedPK := append([]string{}, pk...)
	sort.Strings(sortedPK)
	slog.Info(fmt.Sprintf("session_model_usage has legacy primary key %q (missing task); rebuilding with composite 6-column key", sortedPK))
	if _, err := c.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	err = rebuildTable(ctx, c, "session_model_usage", "session_model_usage_legacy_pk", sessionModelUsageHealDDL,
		`INSERT OR IGNORE INTO session_model_usage (
                       session_id, model, billing_provider, billing_base_url,
                       billing_mode, task, api_call_count, input_tokens,
                       output_tokens, cache_read_tokens, cache_write_tokens,
                       reasoning_tokens, estimated_cost_usd, actual_cost_usd,
                       cost_status, cost_source, first_seen, last_seen
                   )
                   SELECT session_id, model,
                          COALESCE(billing_provider, ''),
                          COALESCE(billing_base_url, ''),
                          COALESCE(billing_mode, ''),
                          COALESCE(task, ''),
                          api_call_count, input_tokens,
                          output_tokens, cache_read_tokens, cache_write_tokens,
                          reasoning_tokens, estimated_cost_usd, actual_cost_usd,
                          cost_status, cost_source, first_seen, last_seen
                   FROM session_model_usage_legacy_pk`, sessionModelUsageIndexSQL)
	_, ferr := c.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys=ON")
	if err != nil {
		if familyOf(err) != callFamily {
			return errors.Join(err, ferr)
		}
		slog.Debug(fmt.Sprintf("session_model_usage PK heal skipped: %s", errText(err)))
	}
	return ferr
}

// v22 bakes task into the usage primary key, existing rows taking the main loop task
func migrateV22SessionModelUsage(ctx context.Context, c Conn) error {
	var keyed int
	err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('session_model_usage') WHERE name = 'task' AND pk > 0").Scan(&keyed)
	if err == nil && keyed > 0 {
		return nil
	}
	if err == nil {
		err = rebuildTable(ctx, c, "session_model_usage", "session_model_usage_v21", sessionModelUsageV22DDL,
			`INSERT INTO session_model_usage (
                                   session_id, model, billing_provider, billing_base_url,
                                   billing_mode, task, api_call_count, input_tokens,
                                   output_tokens, cache_read_tokens, cache_write_tokens,
                                   reasoning_tokens, estimated_cost_usd, actual_cost_usd,
                                   cost_status, cost_source, first_seen, last_seen
                               )
                               SELECT session_id, model, billing_provider, billing_base_url,
                                      billing_mode, '', api_call_count, input_tokens,
                                      output_tokens, cache_read_tokens, cache_write_tokens,
                                      reasoning_tokens, estimated_cost_usd, actual_cost_usd,
                                      cost_status, cost_source, first_seen, last_seen
                               FROM session_model_usage_v21`, sessionModelUsageIndexSQL)
	}
	if err != nil && familyOf(err) == callFamily {
		slog.Debug(fmt.Sprintf("v22 session_model_usage rebuild skipped: %s", errText(err)))
		return nil
	}
	return err
}

// Rank of a stored title source, an unrecorded source ranking as the user's own
func titleRank(source sql.NullString) int {
	if !source.Valid {
		return 2
	}
	switch source.String {
	case TitleSourceLLM:
		return 1
	case TitleSourceUser:
		return 2
	}
	return 0
}

// Creates the unique title index, first renaming duplicate user titles and clearing lower ranked automatic ones
func ensureUniqueTitleIndex(ctx context.Context, c Conn) error {
	_, err := c.ExecContext(ctx, titleUniqueIndexSQL)
	if err == nil {
		return nil
	}
	if !isConstraintError(err) {
		if familyOf(err) == callFamily {
			return nil
		}
		return err
	}
	if _, err := c.ExecContext(ctx, "SAVEPOINT title_repair"); err != nil {
		return err
	}
	if rerr := repairDuplicateTitles(ctx, c); rerr != nil {
		_, _ = c.ExecContext(ctx, "ROLLBACK TO title_repair")
		_, _ = c.ExecContext(ctx, "RELEASE title_repair")
		if familyOf(rerr) == notSQLite {
			return rerr
		}
		slog.Error(fmt.Sprintf("Could not repair duplicate session titles; unique title index not created: %s", errText(rerr)))
		return nil
	}
	_, err = c.ExecContext(ctx, "RELEASE title_repair")
	return err
}

// Reports a constraint violation, the integrity class of SQLite errors
func isConstraintError(err error) bool {
	code, ok := primaryCode(err)
	return ok && code == sqlite3.SQLITE_CONSTRAINT
}

// Duplicate title repair body, run inside the title_repair savepoint
func repairDuplicateTitles(ctx context.Context, c Conn) error {
	type titled struct {
		rowid   int64
		title   string
		source  sql.NullString
		started float64
	}
	rows, err := c.QueryContext(ctx, "SELECT rowid, title, title_source, started_at FROM sessions WHERE title IN "+
		"(SELECT title FROM sessions WHERE title IS NOT NULL GROUP BY title HAVING COUNT(*) > 1)")
	if err != nil {
		return err
	}
	var all []titled
	for rows.Next() {
		var t titled
		if err := rows.Scan(&t.rowid, &t.title, &t.source, &t.started); err != nil {
			rows.Close()
			return err
		}
		all = append(all, t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if ra, rb := titleRank(a.source), titleRank(b.source); ra != rb {
			return ra > rb
		}
		if a.started != b.started {
			return a.started > b.started
		}
		return a.rowid > b.rowid
	})
	var order []string
	groups := map[string][]titled{}
	for _, t := range all {
		if _, seen := groups[t.title]; !seen {
			order = append(order, t.title)
		}
		groups[t.title] = append(groups[t.title], t)
	}
	for _, title := range order {
		group := groups[title]
		var users []titled
		for i := len(group) - 1; i >= 0; i-- {
			if titleRank(group[i].source) == 2 {
				users = append(users, group[i])
			}
		}
		keep := group[0]
		if len(users) > 0 {
			keep = users[0]
			for _, u := range users[1:] {
				renamed, err := nextTitleInLineage(ctx, c, title)
				if err != nil {
					return err
				}
				if _, err := c.ExecContext(ctx, "UPDATE sessions SET title = ? WHERE rowid = ?", renamed, u.rowid); err != nil {
					return err
				}
				slog.Warn(fmt.Sprintf("Renamed duplicate user session title %q to %q", title, renamed))
			}
		}
		for _, t := range group {
			if t.rowid != keep.rowid && titleRank(t.source) != 2 {
				if _, err := c.ExecContext(ctx, "UPDATE sessions SET title = NULL WHERE rowid = ?", t.rowid); err != nil {
					return err
				}
				slog.Warn(fmt.Sprintf("Cleared duplicate %s session title %q", t.source.String, title))
			}
		}
	}
	_, err = c.ExecContext(ctx, titleUniqueIndexSQL)
	return err
}

// v18 one-time backfill of gateway metadata from sessions.json, only ever filling NULL columns
func (s *Store) backfillGatewayMetadata(ctx context.Context, c Conn) error {
	h, err := s.eng.home.Home(ctx)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(home.SessionsIndex.In(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return nil
	}
	var keys []string
	values := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, seen := values[key]; !seen {
			keys = append(keys, key)
		}
		values[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("sessions.json carries data after its object")
	}
	for _, key := range keys {
		if strings.HasPrefix(key, "_") {
			continue
		}
		if err := backfillGatewayEntry(ctx, c, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}

// Applies one sessions.json entry to its session row
func backfillGatewayEntry(ctx context.Context, c Conn, key string, raw json.RawMessage) error {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil || entry == nil {
		return nil
	}
	id, ok := jsonIDText(entry["session_id"])
	if !ok {
		return nil
	}
	var origin map[string]json.RawMessage
	originJSON := any(nil)
	if r := bytes.TrimSpace(entry["origin"]); len(r) > 0 && r[0] == '{' && json.Unmarshal(r, &origin) == nil {
		text, err := pythonJSONText(r)
		if err != nil {
			return err
		}
		originJSON = text
	}
	sessionKey := any(key)
	if v, err := jsonBindValue(entry["session_key"]); err != nil {
		return err
	} else if jsonTruthy(entry["session_key"]) {
		sessionKey = v
	}
	args := []any{sessionKey}
	for _, field := range []struct {
		src  map[string]json.RawMessage
		name string
	}{{origin, "chat_id"}, {entry, "chat_type"}, {origin, "thread_id"}, {entry, "display_name"}} {
		var v any
		if field.src != nil {
			var err error
			if v, err = jsonBindValue(field.src[field.name]); err != nil {
				return err
			}
		}
		args = append(args, v)
	}
	finalized := 0
	if jsonTruthy(entry["expiry_finalized"]) || jsonTruthy(entry["memory_flushed"]) {
		finalized = 1
	}
	args = []any{args[0], args[1], args[2], args[3], args[4], originJSON, finalized, id}
	_, err := c.ExecContext(ctx, `UPDATE sessions
                   SET session_key = COALESCE(session_key, ?),
                       chat_id = COALESCE(chat_id, ?),
                       chat_type = COALESCE(chat_type, ?),
                       thread_id = COALESCE(thread_id, ?),
                       display_name = COALESCE(display_name, ?),
                       origin_json = COALESCE(origin_json, ?),
                       expiry_finalized = CASE
                           WHEN COALESCE(expiry_finalized, 0) = 0 AND ? = 1 THEN 1
                           ELSE expiry_finalized
                       END
                   WHERE id = ?`, args...)
	return err
}

// Text of a truthy JSON session id as the reference stringifies it, false for a falsy or container value
func jsonIDText(raw json.RawMessage) (string, bool) {
	if !jsonTruthy(raw) {
		return "", false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return "True", true
	}
	return "", false
}

// Bindable value of a JSON scalar, nil for absent or null, an error for a container SQLite cannot bind
func jsonBindValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case nil, string, bool:
		return t, nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		return t.Float64()
	}
	return nil, fmt.Errorf("sessions.json value %s cannot be bound", raw)
}

// Reports whether a JSON value is truthy, false, zero, empty text, empty containers and null being falsy
func jsonTruthy(raw json.RawMessage) bool {
	r := bytes.TrimSpace(raw)
	if len(r) == 0 {
		return false
	}
	switch string(r) {
	case "null", "false", `""`, "[]", "{}":
		return false
	}
	if r[0] == '[' || r[0] == '{' {
		var v any
		if json.Unmarshal(r, &v) == nil {
			switch t := v.(type) {
			case []any:
				return len(t) > 0
			case map[string]any:
				return len(t) > 0
			}
		}
		return true
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		f, err := n.Float64()
		return err != nil || f != 0
	}
	return true
}

// JSON text re-encoded with spaced separators and escaped non-ASCII text, key order and number literals kept
func pythonJSONText(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := writePythonJSON(dec, &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Writes the next JSON value of dec in the spaced layout
func writePythonJSON(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		closer, object := byte(']'), t == '{'
		if object {
			closer = '}'
		}
		b.WriteByte(byte(t))
		for first := true; dec.More(); first = false {
			if !first {
				b.WriteString(", ")
			}
			if object {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				if err := writePythonString(b, fmt.Sprint(key)); err != nil {
					return err
				}
				b.WriteString(": ")
			}
			if err := writePythonJSON(dec, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		b.WriteByte(closer)
	case string:
		return writePythonString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		b.WriteString(fmt.Sprint(t))
	case nil:
		b.WriteString("null")
	}
	return nil
}

// Writes s as an ASCII escaped JSON string
func writePythonString(b *strings.Builder, s string) error {
	enc, err := fileutil.EncodeJSON(s, fileutil.JSONStyle{ASCII: true})
	if err != nil {
		return err
	}
	b.Write(enc)
	return nil
}
