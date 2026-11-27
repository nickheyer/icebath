package document

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Rows shown of each table
	sqlitePreviewRows = 5
	// Tables described before the rest are summed up
	sqliteMaxTables = 200
	// Characters of a preview cell
	sqliteCellChars = 80
)

// Overview of a SQLite database: its tables with their schema, row count and first rows, then its indexes, views and triggers
func sqlite(ctx context.Context, data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte(sqliteMagic)) {
		return "", fail("not a SQLite database (magic bytes do not match)")
	}
	file, remove, err := temporary(data, ".sqlite")
	if err != nil {
		return "", err
	}
	defer remove()
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", "file://"+uriPath(abs)+"?mode=ro&immutable=1")
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return "", err
	}
	out, err := sqliteOverview(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fail("SQLite read failed: %s", err.Error())
	}
	return out, nil
}

// Path of a file URI, everything but unreserved characters, slashes and a drive's colon percent encoded
func uriPath(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("/_.-~:", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

type sqliteObject struct{ kind, name, sql string }

func sqliteOverview(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type DESC, name")
	if err != nil {
		return "", err
	}
	var tables, others []sqliteObject
	for rows.Next() {
		var o sqliteObject
		if err := rows.Scan(&o.kind, &o.name, &o.sql); err != nil {
			rows.Close()
			return "", err
		}
		if o.kind == "table" {
			tables = append(tables, o)
		} else {
			others = append(others, o)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	out := []string{"# SQLite database", "", fmt.Sprintf("%d table(s), %d index/view/trigger(s)", len(tables), len(others))}
	for _, t := range tables[:min(len(tables), sqliteMaxTables)] {
		quoted := quoteIdent(t.name)
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return "", err
		}
		out = append(out, "", fmt.Sprintf("## %s  (%s rows)", t.name, strs.Thousands(int(count))), strings.TrimSpace(t.sql))
		cols, preview, err := sqlitePreview(ctx, db, quoted)
		if err != nil {
			return "", err
		}
		if len(preview) == 0 {
			continue
		}
		out = append(out, "| "+strings.Join(cols, " | ")+" |", "|"+strings.Repeat("---|", len(cols)))
		for _, row := range preview {
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = sqliteCell(v)
			}
			out = append(out, "| "+strings.Join(cells, " | ")+" |")
		}
	}
	if len(tables) > sqliteMaxTables {
		out = append(out, fmt.Sprintf("\n... %d more tables omitted", len(tables)-sqliteMaxTables))
	}
	if len(others) > 0 {
		out = append(out, "", "## Indexes / views / triggers")
		for _, o := range others {
			out = append(out, "- "+o.kind+" "+o.name)
		}
	}
	out = append(out, "", "Query it with the terminal: sqlite3 <path> 'SELECT ...'  (or Python's sqlite3 module).")
	return strings.Join(out, "\n"), nil
}

// Column names and first rows of a table, each value read through unary plus so it keeps its storage class whatever the column's
// declared type
func sqlitePreview(ctx context.Context, db *sql.DB, quoted string) ([]string, [][]any, error) {
	probe, err := db.QueryContext(ctx, "SELECT * FROM "+quoted+" LIMIT 0")
	if err != nil {
		return nil, nil, err
	}
	cols, err := probe.Columns()
	probe.Close()
	if err != nil {
		return nil, nil, err
	}
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = "+" + quoteIdent(c)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s LIMIT %d", strings.Join(exprs, ", "), quoted, sqlitePreviewRows))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		out = append(out, vals)
	}
	return cols, out, rows.Err()
}

// One preview cell: NULL, a blob's size, or the value with pipes escaped, newlines flattened and its width capped
func sqliteCell(v any) string {
	var text string
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<blob %d bytes>", len(x))
	case int64:
		text = strconv.FormatInt(x, 10)
	case float64:
		text = formatReal(x)
	case string:
		text = x
	default:
		text = fmt.Sprint(x)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "|", `\|`), "\n", " ")
	if utf8.RuneCountInString(text) <= sqliteCellChars {
		return text
	}
	return strs.Head(text, sqliteCellChars-1) + "…"
}

// Shortest text of a REAL that reads back the same: positional with a decimal point from 1e-4 up to 1e16, else scientific with two
// exponent digits at least
func formatReal(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(sci, "-") {
		sign, sci = "-", sci[1:]
	}
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		expSign := "+"
		if exp < 0 {
			expSign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, expSign, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}
