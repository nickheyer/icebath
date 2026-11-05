package state

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var ftsDDLTables = []string{"messages_fts", "messages_fts_trigram"}

var (
	storedFormPunct  = regexp.MustCompile(`\s*([(),])\s*`)
	storedFormQuoted = regexp.MustCompile(`^CREATE (VIRTUAL TABLE|VIEW) "([^"]+)"`)
)

// Stored definition with whitespace collapsed, none around parentheses and commas, and the object name unquoted
func storedForm(sql string) string {
	collapsed := storedFormPunct.ReplaceAllString(strings.Join(pystr.SplitWhitespace(sql, -1), " "), "$1")
	return storedFormQuoted.ReplaceAllString(collapsed, "CREATE $1 $2")
}

// Stored form of the CREATE kind IF NOT EXISTS name statement in ddl, a ValueError when ddl lacks it
func expectedStoredSQL(ddl, kind, name string) (string, error) {
	start := strings.Index(ddl, "CREATE "+kind+" IF NOT EXISTS "+name+" ")
	if start < 0 {
		return "", &pyConvError{"ValueError", "substring not found"}
	}
	end := strings.Index(ddl[start:], ";")
	if end < 0 {
		return "", &pyConvError{"ValueError", "substring not found"}
	}
	return storedForm(strings.Replace(ddl[start:start+end], " IF NOT EXISTS", "", 1)), nil
}

// Stored form of the virtual table statement for table in ddl
func expectedVTableSQL(ddl, table string) (string, error) {
	return expectedStoredSQL(ddl, "VIRTUAL TABLE", table)
}

// Every stored definition a released version created per FTS table, oldest first, the last one current
var FTSVTableLayouts = map[string][]string{
	"messages_fts": {
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,content=messages,content_rowid=id)",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content)",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name,tool_calls,content='messages'," +
			"content_rowid='id')",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name,tool_calls," +
			"content='messages_fts_src',content_rowid='id')",
	},
	"messages_fts_trigram": {
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,content=messages,content_rowid=id," +
			"tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tool_name,tool_calls," +
			"content='messages_fts_trigram_src',content_rowid='id',tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tool_name," +
			"content='messages_fts_trigram_src',content_rowid='id',tokenize='trigram')",
	},
}

// One FTS virtual table and its stored definition
type storedVTable struct{ name, sql string }

// Stored forms of the present FTS virtual tables in sqlite_master row order
func storedVTables(ctx context.Context, conn SQLiteConn) ([]storedVTable, error) {
	rows, err := conn.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name IN (?, ?)",
		ftsDDLTables[0], ftsDDLTables[1])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedVTable
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		out = append(out, storedVTable{name, storedForm(ddl.String)})
	}
	return out, rows.Err()
}

// Reports a definition an earlier release created, including the pre registry shapes kept on the opt in path
func isOlderLayout(table, stored string) bool {
	layouts := FTSVTableLayouts[table]
	if len(layouts) > 0 && slices.Contains(layouts[:len(layouts)-1], stored) {
		return true
	}
	if table == "messages_fts" {
		return !strings.Contains(stored, "tool_name")
	}
	return strings.Contains(pystr.Lower(stored), "tool_calls")
}

// Reports messages_fts holding a definition this code or an earlier release created
func BaseIndexRecognised(ctx context.Context, conn SQLiteConn) (bool, error) {
	stored, err := storedVTables(ctx, conn)
	if err != nil {
		return false, err
	}
	for _, s := range stored {
		if s.name == "messages_fts" {
			return slices.Contains(FTSVTableLayouts["messages_fts"], s.sql), nil
		}
	}
	return false, nil
}

// Reports an external content base index not reading messages_fts_src, which the next read write open rebuilds
func BaseIndexRealignedOnOpen(stored string) bool {
	return strings.Contains(stored, "tool_name") && !strings.Contains(stored, "messages_fts_src")
}

// FTS tables on an older release definition that only optimize-storage replaces, empty otherwise
func FTSLayoutDrift(ctx context.Context, conn SQLiteConn) ([]string, error) {
	stored, err := storedVTables(ctx, conn)
	if err != nil {
		return nil, err
	}
	drift := []string{}
	for _, s := range stored {
		if isOlderLayout(s.name, s.sql) && !(s.name == "messages_fts" && BaseIndexRealignedOnOpen(s.sql)) {
			drift = append(drift, s.name)
		}
	}
	return drift, nil
}

// Stored form of the base index source view, false when the view is absent
func baseSourceView(ctx context.Context, conn SQLiteConn) (string, bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'messages_fts_src'")
	if err != nil || row == nil {
		return "", false, err
	}
	text, _ := row[0].(string)
	return storedForm(text), true, nil
}

// Reports every present FTS table and the base source view exactly matching what this code creates
func FTSLayoutCurrent(ctx context.Context, conn SQLiteConn) (bool, error) {
	stored, err := storedVTables(ctx, conn)
	if err != nil {
		return false, err
	}
	hasBase := false
	for _, s := range stored {
		layouts := FTSVTableLayouts[s.name]
		if s.sql != layouts[len(layouts)-1] {
			return false, nil
		}
		hasBase = hasBase || s.name == "messages_fts"
	}
	if !hasBase {
		return true, nil
	}
	view, ok, err := baseSourceView(ctx, conn)
	if err != nil || !ok {
		return false, err
	}
	want, err := expectedStoredSQL(FTSSQL, "VIEW", "messages_fts_src")
	return err == nil && view == want, err
}

// Reports messages_fts_src existing with a definition other than the one this code creates
func BaseSourceViewIsStale(ctx context.Context, conn SQLiteConn) (bool, error) {
	view, ok, err := baseSourceView(ctx, conn)
	if err != nil || !ok {
		return false, err
	}
	want, err := expectedStoredSQL(FTSSQL, "VIEW", "messages_fts_src")
	return err == nil && view != want, err
}
