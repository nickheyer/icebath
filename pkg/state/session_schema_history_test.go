package state

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Column order SchemaSQL declares for a table today
func declaredNow(t *testing.T, table string) []string {
	t.Helper()
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mustExecConn(t, conn, SchemaSQL)
	rows, err := conn.QueryContext(context.Background(), fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notnull, pk int64
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func TestReplayedHistoryEndsAtCurrentSchema(t *testing.T) {
	tables := make([]string, 0, len(SchemaHistory))
	for table := range SchemaHistory {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var labels []int
			for _, event := range SchemaHistory[table].Events {
				n, err := strconv.Atoi(strings.Fields(event.Label)[0])
				if err != nil {
					t.Fatal(err)
				}
				labels = append(labels, n)
			}
			for i, n := range labels {
				if n != i+1 {
					t.Fatalf("SchemaHistory[%q].Events is out of order: append new events at the END with the next sequence number, never insert mid-list (got %v)", table, labels)
				}
			}
			replayed, err := CurrentDeclaredColumns(table)
			if err != nil {
				t.Fatal(err)
			}
			declared := declaredNow(t, table)
			var missing, extra []string
			for _, c := range declared {
				if !slices.Contains(replayed, c) {
					missing = append(missing, c)
				}
			}
			for _, c := range replayed {
				if !slices.Contains(declared, c) {
					extra = append(extra, c)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("SchemaSQL declares %v for %s but the replayed history does not", missing, table)
			}
			if len(extra) > 0 {
				t.Fatalf("the replayed history declares %v for %s but SchemaSQL no longer does", extra, table)
			}
			if !slices.Equal(replayed, declared) {
				t.Fatalf("%s column order drifted: replayed %v vs SchemaSQL %v", table, replayed, declared)
			}
		})
	}
}

func TestSchemaHistoryEditsAndLookups(t *testing.T) {
	if _, err := DeclaredSnapshots("nope"); err == nil || err.Error() != "KeyError: 'nope'" {
		t.Fatalf("unknown table error = %v", err)
	}
	cols, err := applySchemaEdits([]string{"a", "b"}, []SchemaEdit{{"+", "z", ""}, {"+", "c", "a"}, {"-", "b", ""}})
	if err != nil || !slices.Equal(cols, []string{"z", "a", "c"}) {
		t.Fatalf("edits = %v %v", cols, err)
	}
	if _, err := applySchemaEdits([]string{"a"}, []SchemaEdit{{"-", "x", ""}}); err == nil || err.Error() != "ValueError: list.remove(x): x not in list" {
		t.Fatalf("remove missing = %v", err)
	}
	if _, err := applySchemaEdits([]string{"a"}, []SchemaEdit{{"+", "c", "x"}}); err == nil || err.Error() != "ValueError: 'x' is not in list" {
		t.Fatalf("insert after missing = %v", err)
	}
}

func TestReachablePhysicalLayoutsAppendUnseenColumnsPerUpgrade(t *testing.T) {
	saved := SchemaHistory
	t.Cleanup(func() { SchemaHistory = saved })
	SchemaHistory = map[string]TableHistory{"t": {
		Base: []string{"a", "b"},
		Events: []SchemaEvent{
			{"01", []SchemaEdit{{"+", "x", "a"}}},
			{"02", []SchemaEdit{{"-", "x", ""}, {"+", "y", "b"}}},
		},
	}}
	seq, err := ReachablePhysicalLayouts("t", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for layout := range seq {
		got = append(got, strings.Join(layout, ","))
	}
	want := []string{"a,b", "a,x,b", "a,b,y", "a,b,x", "a,x,b,y", "a,b,x,y"}
	if !slices.Equal(got, want) {
		t.Fatalf("layouts = %v, want %v", got, want)
	}
	var firsts []int
	seq, _ = ReachablePhysicalLayouts("t", func(layout []string, first int) bool {
		firsts = append(firsts, first)
		return !slices.Contains(layout, "x")
	})
	got = got[:0]
	for layout := range seq {
		got = append(got, strings.Join(layout, ","))
	}
	if !slices.Equal(got, []string{"a,b", "a,b,y"}) || !slices.Equal(firsts, []int{0, 0, 0, 2}) {
		t.Fatalf("pruned layouts = %v firsts = %v", got, firsts)
	}
	SchemaHistory = saved
	sessions, err := ReachablePhysicalLayouts("sessions", func(layout []string, first int) bool { return len(layout) <= 16 })
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for range sessions {
		count++
	}
	if count == 0 {
		t.Fatal("no sessions layouts")
	}
}
