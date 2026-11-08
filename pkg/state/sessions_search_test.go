package state

import (
	"context"
	"reflect"
	"testing"
)

func TestSearchSessionsByIdMatchesExactPrefixAndSubstring(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	seed := func(sid, content string) {
		if err := db.CreateSession(NewSession{ID: sid, Source: "cli", Model: "test-model"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": content}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	seed("20260603_090200_abcd12", "content without id")
	seed("20260602_111111_other99", "other content")
	ids := func(query string) []any {
		rows, err := db.SearchSessionsByID(ctx, query, SearchSessionsByIDOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out := []any{}
		for _, row := range rows {
			out = append(out, rowValue(row, "id"))
		}
		return out
	}
	for _, query := range []string{"20260603_090200_abcd12", "20260603", "ABCD12"} {
		if got := ids(query); !reflect.DeepEqual(got, []any{"20260603_090200_abcd12"}) {
			t.Fatalf("%q matched %v", query, got)
		}
	}
}
