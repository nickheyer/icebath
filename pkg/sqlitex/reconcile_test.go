package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestColumnsAddedWithTheirLinks(t *testing.T) {
	ctx := context.Background()
	const parent = `CREATE TABLE IF NOT EXISTS parent (id TEXT PRIMARY KEY);`
	for name, tc := range map[string]struct {
		child string
		added []string
	}{
		"a link of the column's own": {`CREATE TABLE IF NOT EXISTS child (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES parent(id), note TEXT NOT NULL DEFAULT '');`,
			[]string{"child.parent_id", "child.note"}},
		"a link of the table's": {`CREATE TABLE IF NOT EXISTS child (id TEXT PRIMARY KEY, parent_id TEXT, FOREIGN KEY (parent_id) REFERENCES parent(id));`, nil},
		"a link of two columns": {`CREATE TABLE IF NOT EXISTS child (id TEXT PRIMARY KEY, parent_id TEXT, other TEXT,
			FOREIGN KEY (parent_id, other) REFERENCES parent(id, id));`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx, parent+`CREATE TABLE child (id TEXT PRIMARY KEY);`); err != nil {
				t.Fatal(err)
			}
			want, err := Declare(ctx, parent, tc.child)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Reconcile(ctx, db, want)
			if tc.added == nil {
				// A file the link can't be added to is of another layout at once, not after its columns were added without the link
				if _, other := errors.AsType[*Mismatch](err); !other {
					t.Fatalf("reconciled with the plan %+v and the error %v", plan, err)
				}
				return
			}
			if err != nil || !slices.Equal(plan.Added, tc.added) {
				t.Fatalf("added %v with the error %v", plan.Added, err)
			}
			if again, err := Inspect(ctx, db, want); err != nil || len(again.Added)+len(again.Missing)+len(again.Dropped) > 0 {
				t.Fatalf("the reconciled file still takes %+v, with the error %v", again, err)
			}
		})
	}
}

func TestColumnDefinitionsRead(t *testing.T) {
	definitions, checks := parseTable(`CREATE TABLE t ("check" TEXT, a TEXT CHECK (a <> "references") COLLATE NOCASE, b TEXT REFERENCES t(a),
		CHECK (a <> b))`)
	if len(definitions) != 3 || definitions["check"].text != `"check" TEXT` || definitions["a"].links || !definitions["b"].links ||
		definitions["a"].collation != "nocase" || !slices.Equal(checks, []string{"a < > b"}) {
		t.Fatalf("read %+v with the table's rules %v", definitions, checks)
	}
}

func TestDamagedKinds(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"database": {append([]byte("SQLite format 3\x00"), make([]byte, 100)...), ""},
		"empty":    {nil, ""},
		"zeroed":   {make([]byte, 100000), Zeroed},
		// A file that only starts with zero bytes holds something else, and is kept under the name that says so
		"starts with zeros": {append(make([]byte, 70000), []byte("notes")...), NotADatabase},
		"text":              {[]byte("hello, this is no database"), NotADatabase},
		"short":             {[]byte("hi"), NotADatabase},
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, c.data, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, size, err := Damaged(path); err != nil || got != c.want || size != int64(len(c.data)) {
			t.Errorf("%s: Damaged = %q, %d, %v, want %q, %d", name, got, size, err, c.want, len(c.data))
		}
	}
	if got, _, err := Damaged(filepath.Join(dir, "missing")); err != nil || got != "" {
		t.Errorf("missing: Damaged = %q, %v", got, err)
	}
}
