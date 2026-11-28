// Package sqlitex keeps a SQLite file in the shape the store that owns it declares: the shape read from the store's own DDL, a file
// reconciled with it in place where SQLite allows, and a file of another shape or a damaged one moved aside under the lock of its first
// open
package sqlitex

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
)

// What runs statements: a database, a connection or a transaction
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Shape of a database: its tables, and its indexes, triggers, views and virtual tables, each by its name in lower case
type Schema struct {
	Tables  map[string]*Table
	Objects map[string]Object
}

// Kinds of object besides tables
const (
	Index   = "index"
	Trigger = "trigger"
	View    = "view"
	Virtual = "virtual"
)

// Index, trigger, view or virtual table, with the statement that makes it
type Object struct {
	Type, Name, Table, SQL string
}

// Table as the database keeps it
type Table struct {
	Name    string
	Columns []Column
	// Columns of the primary key in order; those of each UNIQUE constraint, joined by commas
	Key    []string
	Unique []string
	// Foreign keys and CHECK rules, as any spelling of the same one writes it
	Foreign []Link
	Checks  []string
	// Kept without a rowid, and STRICT
	WithoutRowid, Strict bool
}

// Foreign key of a table: the columns it links from, and the whole link as any spelling of it writes it
type Link struct {
	From []string
	Text string
}

// Column of a table
type Column struct {
	Name string
	// Type as declared
	Type    string
	NotNull bool
	// Default as any spelling of it writes it, empty for none
	Default string
	// Place in the primary key from 1, 0 outside it
	Key int
	// 0 for a plain column, 2 for one generated as it is read, 3 for one generated as it is stored
	Hidden int
	// Collating sequence named in its definition, empty for the default
	Collation string
	// Numbered with AUTOINCREMENT, and a primary key of its own declared DESC, which keeps it from standing for the rowid
	Auto, Descending bool
	// Linked to another table by a REFERENCES of its own definition, which adding the column as declared brings with it
	Links bool
	// Definition as the CREATE TABLE statement writes it, which adds the column to a table as declared
	Definition string
}

// Shape the DDL makes, read back from an empty database in memory, so a store's shape comes from the statements that make it
func Declare(ctx context.Context, ddl ...string) (*Schema, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// Every connection to memory is a database of its own, so one connection does it all
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for _, statements := range ddl {
		if _, err := conn.ExecContext(ctx, statements); err != nil {
			return nil, err
		}
	}
	return Read(ctx, conn)
}

// Shape of the database the querier reaches
func Read(ctx context.Context, q Querier) (*Schema, error) {
	kinds := map[string][3]string{}
	listed, err := rowsOf(ctx, q, 4, `SELECT name, type, wr, strict FROM pragma_table_list WHERE schema = 'main'`)
	if err != nil {
		return nil, err
	}
	for _, row := range listed {
		kinds[strings.ToLower(row[0])] = [3]string{row[1], row[2], row[3]}
	}
	stored, err := rowsOf(ctx, q, 4, `SELECT type, name, tbl_name, sql FROM sqlite_master WHERE sql IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	s := &Schema{Tables: map[string]*Table{}, Objects: map[string]Object{}}
	for _, row := range stored {
		kind, name, table, statement := row[0], row[1], row[2], row[3]
		key := strings.ToLower(name)
		if strings.HasPrefix(key, "sqlite_") {
			continue
		}
		switch {
		case kind == "table" && kinds[key][0] == "virtual":
			s.Objects[key] = Object{Type: Virtual, Name: name, Table: table, SQL: statement}
		case kind == "table" && kinds[key][0] == "table":
			t, err := readTable(ctx, q, name, statement)
			if err != nil {
				return nil, err
			}
			t.WithoutRowid, t.Strict = kinds[key][1] == "1", kinds[key][2] == "1"
			s.Tables[key] = t
		case kind == Index || kind == Trigger || kind == View:
			s.Objects[key] = Object{Type: kind, Name: name, Table: table, SQL: statement}
		}
	}
	return s, nil
}

// Table of the name as the database keeps it, its CREATE TABLE statement read for what the pragmas leave out
func readTable(ctx context.Context, q Querier, name, statement string) (*Table, error) {
	t := &Table{Name: name}
	definitions, checks := parseTable(statement)
	t.Checks = checks
	columns, err := rowsOf(ctx, q, 6, `SELECT name, type, "notnull", COALESCE(dflt_value, ''), pk, hidden FROM pragma_table_xinfo(?)`, name)
	if err != nil {
		return nil, err
	}
	keys := map[int]string{}
	for _, row := range columns {
		c := Column{Name: row[0], Type: row[1], NotNull: row[2] == "1", Default: Normalize(row[3]), Key: atoi(row[4]), Hidden: atoi(row[5])}
		if c.Default == "null" {
			c.Default = ""
		}
		if d, ok := definitions[strings.ToLower(c.Name)]; ok {
			c.Collation, c.Auto, c.Descending, c.Links, c.Definition = d.collation, d.auto, d.descending, d.links, d.text
			t.Checks = append(t.Checks, d.checks...)
		}
		if c.Key > 0 {
			keys[c.Key] = strings.ToLower(c.Name)
		}
		t.Columns = append(t.Columns, c)
	}
	for i := 1; i <= len(keys); i++ {
		t.Key = append(t.Key, keys[i])
	}
	uniques, err := rowsOf(ctx, q, 1, `SELECT name FROM pragma_index_list(?) WHERE origin = 'u'`, name)
	if err != nil {
		return nil, err
	}
	for _, row := range uniques {
		names, err := rowsOf(ctx, q, 1, `SELECT COALESCE(name, '') FROM pragma_index_info(?) ORDER BY seqno`, row[0])
		if err != nil {
			return nil, err
		}
		t.Unique = append(t.Unique, strings.ToLower(strings.Join(column(names), ",")))
	}
	links, err := rowsOf(ctx, q, 7, `SELECT id, "table", "from", COALESCE("to", ''), on_update, on_delete, "match" FROM pragma_foreign_key_list(?)
		ORDER BY id, seq`, name)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(links); {
		end := start
		var from, to []string
		for ; end < len(links) && links[end][0] == links[start][0]; end++ {
			from, to = append(from, strings.ToLower(links[end][2])), append(to, strings.ToLower(links[end][3]))
		}
		row := links[start]
		t.Foreign = append(t.Foreign, Link{From: from, Text: strings.ToLower(strings.Join(from, ",") + ">" + row[1] + "(" + strings.Join(to, ",") +
			") on update " + row[4] + " on delete " + row[5] + " match " + row[6])})
		start = end
	}
	slices.Sort(t.Unique)
	slices.Sort(t.Checks)
	slices.SortFunc(t.Foreign, func(a, b Link) int { return cmp.Compare(a.Text, b.Text) })
	return t, nil
}

// What a column definition says beyond the pragmas: its text, collating sequence, CHECK rules, numbering and whether it links the column
// to another table
type definition struct {
	text, collation         string
	checks                  []string
	auto, descending, links bool
}

// Column definitions of a CREATE TABLE statement by column name in lower case, and the CHECK rules of its table constraints
func parseTable(statement string) (map[string]definition, []string) {
	definitions := map[string]definition{}
	var checks []string
	for _, part := range parts(statement) {
		if len(part) == 0 {
			continue
		}
		// A table constraint starts with a bare keyword; a column of such a name is quoted
		if part[0].kind == word {
			switch part[0].norm() {
			case "constraint", "primary", "unique", "check", "foreign":
				checks = append(checks, checksOf(part)...)
				continue
			}
		}
		d := definition{text: statement[part[0].start:part[len(part)-1].end], checks: checksOf(part)}
		depth := 0
		for i, t := range part {
			switch {
			case t.kind == mark && t.text == "(":
				depth++
			case t.kind == mark && t.text == ")":
				depth--
			}
			switch t.norm() {
			case "references":
				// Outside the brackets of a CHECK rule or of a default, where the word could only be a name
				d.links = d.links || t.kind == word && depth == 0
			case "collate":
				if i+1 < len(part) {
					d.collation = strings.ToLower(unquote(part[i+1].text))
				}
			case "autoincrement":
				d.auto = true
			case "desc":
				d.descending = d.descending || i >= 2 && part[i-1].norm() == "key" && part[i-2].norm() == "primary"
			}
		}
		definitions[strings.ToLower(unquote(part[0].text))] = d
	}
	return definitions, checks
}

// Column definitions and table constraints of a CREATE TABLE statement, each as its tokens
func parts(statement string) [][]token {
	tokens := tokenize(statement)
	var out [][]token
	depth, start := 0, 0
	for i, t := range tokens {
		if t.kind != mark {
			continue
		}
		switch t.text {
		case "(":
			if depth++; depth == 1 {
				start = i + 1
			}
		case ")":
			if depth == 1 {
				return append(out, tokens[start:i])
			}
			depth--
		case ",":
			if depth == 1 {
				out = append(out, tokens[start:i])
				start = i + 1
			}
		}
	}
	return out
}

// CHECK rules among the tokens of a column definition or table constraint, each as any spelling of it writes it
func checksOf(part []token) []string {
	var out []string
	for i := 0; i+1 < len(part); i++ {
		if part[i].norm() != "check" || part[i+1].text != "(" {
			continue
		}
		depth, j := 0, i+1
		for ; j < len(part); j++ {
			switch part[j].text {
			case "(":
				depth++
			case ")":
				depth--
			}
			if depth == 0 {
				break
			}
		}
		out = append(out, joined(part[i+2:min(j, len(part))]))
		i = j
	}
	return out
}

// Rows of a query, each its first n columns as text
func rowsOf(ctx context.Context, q Querier, n int, query string, args ...any) ([][]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		values := make([]sql.NullString, n)
		pointers := make([]any, n)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make([]string, n)
		for i, value := range values {
			row[i] = value.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// First column of the rows
func column(rows [][]string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row[0]
	}
	return out
}

// Whole number a pragma gave as text, 0 for none
func atoi(text string) int {
	n, _ := strconv.Atoi(text)
	return n
}
