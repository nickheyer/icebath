package sqlitex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"modernc.org/sqlite"
)

// What reconciling a database with its schema takes: the tables it lacks, which the store's DDL makes; the columns added, as table.column;
// the objects dropped for that DDL to make again
type Plan struct {
	Missing []string
	Added   []string
	Dropped []Object
	adds    []string
}

// Why a database is not of its schema and can't be made so in place
type Mismatch struct {
	Reasons []string
}

func (m *Mismatch) Error() string {
	return "The database has another layout, because " + strings.Join(m.Reasons, ", and ") + "."
}

// What reconciling the database the querier reaches with the schema takes, changing nothing: a *Mismatch saying why when it is not of the
// schema and can't be made so in place
func Inspect(ctx context.Context, q Querier, want *Schema) (Plan, error) {
	have, err := Read(ctx, q)
	if err != nil {
		return Plan{}, err
	}
	p, reasons, err := want.plan(ctx, q, have)
	if err == nil && len(reasons) > 0 {
		err = &Mismatch{Reasons: reasons}
	}
	return p, err
}

// Brings the database the transaction writes to into the shape of the schema where SQLite can do it in place: the columns it lacks are
// added as declared, and its views, triggers, indexes and virtual tables whose statement differs from the declared one, or that the schema
// does not declare, are dropped for the store's DDL to make again. Tables it lacks are left to that DDL too. A database that is not of the
// schema gets a *Mismatch, and the caller rolls the transaction back
func Reconcile(ctx context.Context, tx Querier, want *Schema) (Plan, error) {
	p, err := Inspect(ctx, tx, want)
	if err != nil {
		return p, err
	}
	for i, statement := range p.adds {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			if refused(err) {
				return p, &Mismatch{Reasons: []string{"the column " + p.Added[i] + " can't be added: " + strings.TrimSuffix(err.Error(), ".")}}
			}
			return p, err
		}
	}
	for _, o := range p.Dropped {
		kind := strings.ToUpper(o.Type)
		if o.Type == Virtual {
			kind = "TABLE"
		}
		if _, err := tx.ExecContext(ctx, "DROP "+kind+" IF EXISTS "+Quote(o.Name)); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Whether SQLite turned a statement down for what it says rather than for the state of the file
func refused(err error) bool {
	var problem *sqlite.Error
	if !errors.As(err, &problem) {
		return false
	}
	code := problem.Code() & 0xff
	return code == 1 || code == 19
}

// What reconciling the database with the schema takes, and why it can't be done in place
func (want *Schema) plan(ctx context.Context, q Querier, have *Schema) (Plan, []string, error) {
	var p Plan
	var reasons []string
	shared := false
	for _, name := range slices.Sorted(maps.Keys(want.Tables)) {
		// A view, trigger, index or virtual table where a table goes holds nothing of its own
		if o, ok := have.Objects[name]; ok {
			p.Dropped = append(p.Dropped, o)
		}
		h, ok := have.Tables[name]
		if !ok {
			p.Missing = append(p.Missing, want.Tables[name].Name)
			continue
		}
		shared = true
		why, err := want.Tables[name].compare(ctx, q, h, &p)
		if err != nil {
			return p, nil, err
		}
		reasons = append(reasons, why...)
	}
	if !shared && len(have.Tables) > 0 && len(want.Tables) > 0 {
		reasons = append(reasons, "it holds other tables and none of those it should")
	}
	for _, name := range slices.Sorted(maps.Keys(want.Objects)) {
		w := want.Objects[name]
		if _, ok := have.Tables[name]; ok {
			reasons = append(reasons, "the table "+name+" stands where the "+noun(w.Type)+" of that name goes")
			continue
		}
		if h, ok := have.Objects[name]; ok && (h.Type != w.Type || Normalize(h.SQL) != Normalize(w.SQL)) {
			p.Dropped = append(p.Dropped, h)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(have.Objects)) {
		_, object := want.Objects[name]
		_, table := want.Tables[name]
		if h := have.Objects[name]; !object && !table && h.Type != Virtual {
			p.Dropped = append(p.Dropped, h)
		}
	}
	// Triggers go first, as they may use the views and tables dropped after them
	rank := map[string]int{Trigger: 0, View: 1, Index: 2, Virtual: 3}
	slices.SortStableFunc(p.Dropped, func(a, b Object) int { return cmp.Compare(rank[a.Type], rank[b.Type]) })
	return p, reasons, nil
}

func noun(kind string) string {
	if kind == Virtual {
		return "virtual table"
	}
	return kind
}

// Why the table the database has can't take the place of the declared one, the columns it lacks and SQLite can add planned
func (w *Table) compare(ctx context.Context, q Querier, h *Table, p *Plan) ([]string, error) {
	var why []string
	say := func(format string, args ...any) { why = append(why, fmt.Sprintf(format, args...)) }
	if w.WithoutRowid != h.WithoutRowid || w.Strict != h.Strict {
		say("the table %s stores its rows another way", w.Name)
	}
	if !slices.Equal(w.Key, h.Key) {
		say("the table %s is keyed by other columns", w.Name)
	}
	if !slices.Equal(w.Unique, h.Unique) {
		say("the table %s keeps other columns unique", w.Name)
	}
	for _, check := range h.Checks {
		if !slices.Contains(w.Checks, check) {
			say("the table %s checks its rows by a rule it should not have", w.Name)
			break
		}
	}
	held := map[string]Column{}
	for _, c := range h.Columns {
		held[strings.ToLower(c.Name)] = c
	}
	declared := map[string]bool{}
	var added []string
	for _, c := range w.Columns {
		name := strings.ToLower(c.Name)
		declared[name] = true
		hc, ok := held[name]
		if !ok {
			if w.addable(c) {
				p.Added = append(p.Added, w.Name+"."+c.Name)
				p.adds = append(p.adds, "ALTER TABLE "+Quote(w.Name)+" ADD COLUMN "+c.Definition)
				added = append(added, name)
			} else {
				say("the column %s of the table %s is missing and can't be added", c.Name, w.Name)
			}
			continue
		}
		reason, err := w.differs(ctx, q, h, c, hc)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			say("the column %s of the table %s %s", c.Name, w.Name, reason)
		}
	}
	for _, hc := range h.Columns {
		if !declared[strings.ToLower(hc.Name)] && hc.Hidden == 0 && hc.NotNull && hc.Default == "" && hc.Key == 0 {
			say("the table %s has a column %s that every row must fill", w.Name, hc.Name)
		}
	}
	// Foreign keys of the columns to be added come with them
	var links, heldLinks []string
	for _, link := range w.Foreign {
		if !allIn(link.From, added) {
			links = append(links, link.Text)
		}
	}
	for _, link := range h.Foreign {
		heldLinks = append(heldLinks, link.Text)
	}
	if !slices.Equal(links, heldLinks) {
		say("the table %s links to other tables", w.Name)
	}
	return why, nil
}

// Whether ALTER TABLE ADD COLUMN can add the declared column: outside the primary key and every UNIQUE constraint, not generated as stored,
// with a constant default, a default that is not null when it must be filled, and, when it links to another table, by a REFERENCES of its
// own definition and with no default. A link the table declares apart from the column, alone or with other columns, can't be added to a
// table that is there
func (w *Table) addable(c Column) bool {
	name := strings.ToLower(c.Name)
	switch {
	case c.Key > 0, c.Hidden == 3, slices.Contains(w.Unique, name):
		return false
	case c.NotNull && c.Default == "" && c.Hidden != 2:
		return false
	case strings.HasPrefix(c.Default, "("), c.Default == "current_time", c.Default == "current_date", c.Default == "current_timestamp":
		return false
	}
	for _, link := range w.Foreign {
		if slices.Contains(link.From, name) && (len(link.From) != 1 || !c.Links || c.Default != "") {
			return false
		}
	}
	return true
}

// How the column the database has differs from the declared one in a way the store's statements can't live with, empty when it does not
func (w *Table) differs(ctx context.Context, q Querier, h *Table, c, hc Column) (string, error) {
	switch {
	case w.Strict && !strings.EqualFold(c.Type, hc.Type), affinity(c.Type) != affinity(hc.Type):
		return "holds another kind of value", nil
	case c.Hidden != hc.Hidden:
		return "is worked out another way", nil
	case c.Collation != hc.Collation:
		return "compares text another way", nil
	case c.Default != hc.Default:
		return "has another default", nil
	case c.Auto != hc.Auto || w.alias(c) != h.alias(hc):
		return "is numbered another way", nil
	case !c.NotNull && hc.NotNull:
		return "must always be filled", nil
	case c.NotNull && !hc.NotNull:
		var empty bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+Quote(h.Name)+` WHERE `+Quote(hc.Name)+` IS NULL)`).Scan(&empty); err != nil {
			return "", err
		}
		if empty {
			return "is left empty in some rows", nil
		}
	}
	return "", nil
}

// Whether the column stands for the rowid: the one column of the primary key of a table with rowids, of the type INTEGER exactly, not
// declared DESC on its own
func (w *Table) alias(c Column) bool {
	return !w.WithoutRowid && len(w.Key) == 1 && w.Key[0] == strings.ToLower(c.Name) && strings.EqualFold(strings.TrimSpace(c.Type), "INTEGER") &&
		!c.Descending
}

// Affinity SQLite gives a column of the declared type
func affinity(declared string) string {
	t := strings.ToUpper(declared)
	switch {
	case strings.Contains(t, "INT"):
		return "integer"
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return "text"
	case strings.Contains(t, "BLOB"), strings.TrimSpace(t) == "":
		return "blob"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return "real"
	}
	return "numeric"
}

func allIn(names, set []string) bool {
	for _, name := range names {
		if !slices.Contains(set, name) {
			return false
		}
	}
	return len(names) > 0
}
