package docconv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	sqliteMagic       = "SQLite format 3\x00"
	sqlitePreviewRows = 5
	sqliteMaxTables   = 200
	sqliteCellChars   = 80
)

type sqliteObject struct {
	typ, name, sql string
}

func sqliteURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String() + "?mode=ro&immutable=1"
}

func extractSQLite(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	head := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, head)
	f.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}
	if string(head[:n]) != sqliteMagic {
		return "", extractErr("not a SQLite database (magic bytes do not match)")
	}
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		return "", &ExtractError{Msg: "SQLite read failed: " + err.Error(), Err: err}
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	text, err := sqliteOverview(ctx, db)
	if err != nil {
		return "", &ExtractError{Msg: "SQLite read failed: " + err.Error(), Err: err}
	}
	return text, nil
}

func sqliteObjects(ctx context.Context, db *sql.DB) ([]sqliteObject, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type DESC, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objs []sqliteObject
	for rows.Next() {
		var o sqliteObject
		if err := rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	return objs, rows.Err()
}

func sqlitePreview(ctx context.Context, db *sql.DB, quoted string) ([]string, [][]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+quoted+" LIMIT "+strconv.Itoa(sqlitePreviewRows))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = sqliteCell(v)
		}
		out = append(out, cells)
	}
	return cols, out, rows.Err()
}

func sqliteOverview(ctx context.Context, db *sql.DB) (string, error) {
	objs, err := sqliteObjects(ctx, db)
	if err != nil {
		return "", err
	}
	var tables, others []sqliteObject
	for _, o := range objs {
		if o.typ == "table" {
			tables = append(tables, o)
		} else {
			others = append(others, o)
		}
	}
	out := []string{"# SQLite database", "", fmt.Sprintf("%d table(s), %d index/view/trigger(s)", len(tables), len(others))}
	for _, t := range tables[:min(len(tables), sqliteMaxTables)] {
		quoted := `"` + strings.ReplaceAll(t.name, `"`, `""`) + `"`
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return "", err
		}
		out = append(out, "", "## "+t.name+"  ("+commas(count)+" rows)", strip(t.sql))
		cols, rows, err := sqlitePreview(ctx, db, quoted)
		if err != nil {
			return "", err
		}
		if len(rows) > 0 {
			out = append(out, "| "+strings.Join(cols, " | ")+" |", "|"+strings.Repeat("---|", len(cols)))
			for _, cells := range rows {
				out = append(out, "| "+strings.Join(cells, " | ")+" |")
			}
		}
	}
	if len(tables) > sqliteMaxTables {
		out = append(out, fmt.Sprintf("\n... %d more tables omitted", len(tables)-sqliteMaxTables))
	}
	if len(others) > 0 {
		out = append(out, "", "## Indexes / views / triggers")
		for _, o := range others {
			out = append(out, "- "+o.typ+" "+o.name)
		}
	}
	out = append(out, "", "Query it with the terminal: sqlite3 <path> 'SELECT ...'")
	return strings.Join(out, "\n"), nil
}

func sqliteCell(v any) string {
	var text string
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<blob %d bytes>", len(t))
	case string:
		text = t
	case int64:
		text = strconv.FormatInt(t, 10)
	case float64:
		text = formatReal(t)
	default:
		text = fmt.Sprint(t)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "|", `\|`), "\n", " ")
	if utf8.RuneCountInString(text) <= sqliteCellChars {
		return text
	}
	return text[:runeOffset(text, sqliteCellChars-1)] + "…"
}
