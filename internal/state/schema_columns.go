package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// One column a table declares and the type expression ALTER TABLE ADD COLUMN reproduces it with
type declaredColumn struct {
	name string
	ddl  string
}

// One table the schema declares with its columns in declaration order
type declaredTable struct {
	name    string
	columns []declaredColumn
}

// Tables and columns the schema text declares, parsed once by running it in a memory database
var declaredSchema = sync.OnceValues(func() ([]declaredTable, error) { return parseSchemaColumns(schemaSQL) })

// Expected columns per table read back from PRAGMA table_info after running ddl in a memory database
func parseSchemaColumns(ddl string) ([]declaredTable, error) {
	ctx := context.Background()
	connector, err := sqlite.NewConnector(":memory:")
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return nil, presentable(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, presentable(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]declaredTable, 0, len(names))
	for _, n := range names {
		cols, err := tableInfo(ctx, db, n)
		if err != nil {
			return nil, err
		}
		t := declaredTable{name: n}
		for _, c := range cols {
			var parts []string
			if c.typ != "" {
				parts = append(parts, c.typ)
			}
			if c.notNull && c.pk == 0 {
				parts = append(parts, "NOT NULL")
			}
			if c.dflt.Valid {
				parts = append(parts, "DEFAULT "+c.dflt.String)
			}
			t.columns = append(t.columns, declaredColumn{name: c.name, ddl: strings.Join(parts, " ")})
		}
		out = append(out, t)
	}
	return out, nil
}

// One row of PRAGMA table_info
type columnInfo struct {
	name    string
	typ     string
	notNull bool
	dflt    sql.NullString
	pk      int
}

// Runs one query and closes its rows, the shape both a Conn and a plain database share
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Columns of table in declaration order, empty when the table is missing
func tableInfo(ctx context.Context, q querier, table string) ([]columnInfo, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, presentable(err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var cid int
		var c columnInfo
		var typ sql.NullString
		if err := rows.Scan(&cid, &c.name, &typ, &c.notNull, &c.dflt, &c.pk); err != nil {
			return nil, err
		}
		c.typ = typ.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// Double quoted SQL identifier
func quoteIdent(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// SELECT statements that fail at prepare time exactly when a live store is behind the declared schema
func SchemaReadProbeStatements() ([]string, error) {
	tables, err := declaredSchema()
	if err != nil {
		return nil, err
	}
	sorted := append([]declaredTable{}, tables...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	out := make([]string, 0, len(sorted))
	for _, t := range sorted {
		cols := make([]string, len(t.columns))
		for i, c := range t.columns {
			cols[i] = quoteIdent(t.name) + "." + quoteIdent(c.name)
		}
		out = append(out, "SELECT "+strings.Join(cols, ", ")+" FROM "+quoteIdent(t.name)+" LIMIT 0")
	}
	return out, nil
}

// Adds every declared column missing from the live tables, a lock error rising so the open retries
func reconcileColumns(ctx context.Context, c Conn) error {
	tables, err := declaredSchema()
	if err != nil {
		return err
	}
	for _, t := range tables {
		live, err := tableInfo(ctx, c, t.name)
		if err != nil {
			if familyOf(err) == callFamily {
				continue
			}
			return err
		}
		if len(live) == 0 {
			continue
		}
		have := map[string]bool{}
		for _, col := range live {
			have[col.name] = true
		}
		for _, col := range t.columns {
			if have[col.name] {
				continue
			}
			_, err := c.ExecContext(ctx, "ALTER TABLE "+quoteIdent(t.name)+" ADD COLUMN "+quoteIdent(col.name)+" "+col.ddl)
			if err == nil {
				continue
			}
			if familyOf(err) != callFamily {
				return err
			}
			switch {
			case strings.Contains(lowerText(err), "duplicate column"):
				slog.Debug(fmt.Sprintf("reconcile %s.%s: %s", t.name, col.name, errText(err)))
			case isLockError(err):
				return err
			default:
				slog.Warn(fmt.Sprintf("reconcile %s.%s failed; store remains behind SCHEMA_SQL: %s", t.name, col.name, errText(err)))
			}
		}
	}
	return nil
}

// Complete statements of a script split the way SQLite judges completeness, an unfinished tail an error
func splitStatements(script string) ([]string, error) {
	tls := libc.NewTLS()
	defer tls.Close()
	var out []string
	pending := ""
	lines := strings.Split(script, "\n")
	if strings.HasSuffix(script, "\n") {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		pending += line + "\n"
		complete, err := sqlComplete(tls, pending)
		if err != nil {
			return nil, err
		}
		if !complete {
			continue
		}
		out = append(out, strings.TrimSpace(pending))
		pending = ""
	}
	if strings.TrimSpace(pending) != "" {
		return nil, callErr(nil, "incomplete DDL statement")
	}
	return out, nil
}

// Reports whether text ends in a complete SQL statement
func sqlComplete(tls *libc.TLS, text string) (bool, error) {
	cs, err := libc.CString(text)
	if err != nil {
		return false, err
	}
	defer libc.Xfree(tls, cs)
	return sqlite3.Xsqlite3_complete(tls, cs) != 0, nil
}

// Runs ddl statement by statement, skipping each drop and create trigger pair whose stored body already matches
func executeDDLSkippingSettledTriggers(ctx context.Context, c Conn, ddl string) error {
	stored, err := storedTriggers(ctx, c)
	if err != nil {
		return err
	}
	statements, err := splitStatements(ddl)
	if err != nil {
		return err
	}
	pendingDrop := ""
	for _, current := range statements {
		upper := strings.ToUpper(current)
		if strings.HasPrefix(upper, "DROP TRIGGER IF EXISTS ") {
			pendingDrop = current
			continue
		}
		if strings.HasPrefix(upper, "CREATE TRIGGER IF NOT EXISTS ") {
			desired := strings.TrimRight(strings.Replace(current, "IF NOT EXISTS ", "", 1), ";")
			if fields := strings.Fields(desired); len(fields) > 2 && stored[fields[2]] == desired {
				pendingDrop = ""
				continue
			}
		}
		if pendingDrop != "" {
			if _, err := c.ExecContext(ctx, pendingDrop); err != nil {
				return err
			}
			pendingDrop = ""
		}
		if _, err := c.ExecContext(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

// Stored SQL of every trigger keyed by name
func storedTriggers(ctx context.Context, c Conn) (map[string]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type = 'trigger'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, text sql.NullString
		if err := rows.Scan(&name, &text); err != nil {
			return nil, err
		}
		out[name.String] = text.String
	}
	return out, rows.Err()
}

// Runs a DDL script one statement at a time so the caller's transaction stays open
func executeDDLScript(ctx context.Context, c Conn, ddl string) error {
	statements, err := splitStatements(ddl)
	if err != nil {
		return err
	}
	for _, s := range statements {
		if _, err := c.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// Additive catch-up of a raw state.db connection to the declared schema inside the caller's transaction
func ApplyDeclaredSchema(ctx context.Context, c Conn) error {
	if err := reconcileColumns(ctx, c); err != nil {
		return err
	}
	if err := executeDDLScript(ctx, c, schemaSQL); err != nil {
		return err
	}
	return reconcileColumns(ctx, c)
}
