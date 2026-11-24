package home

import (
	"slices"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestAdoptRecordKeyRenamesTheOlderSpellingInPlace(t *testing.T) {
	retired := RetiredRecordKey(RecordKey)
	rec := jsonutil.ObjectOf("pid", 7, retired, "/h", "kind", "gateway")
	AdoptRecordKey(rec)
	if !slices.Equal(rec.Keys(), []string{"pid", RecordKey, "kind"}) || rec.Value(RecordKey) != "/h" {
		t.Fatalf("adopted %v", rec)
	}
	AdoptRecordKey(rec)
	if rec.Len() != 3 || rec.Value(RecordKey) != "/h" {
		t.Fatalf("a second adoption changed the record: %v", rec)
	}
	both := jsonutil.ObjectOf(RecordKey, "/new", retired, "/old")
	AdoptRecordKey(both)
	if both.Value(RecordKey) != "/new" || both.Value(retired) != "/old" {
		t.Fatalf("a record carrying both spellings was rewritten: %v", both)
	}
	if v, ok := RecordValue(func(k string) (any, bool) { v, ok := map[string]any{retired: "/old"}[k]; return v, ok }, RecordKey); !ok || v != "/old" {
		t.Fatalf("the older spelling is no longer read: %v %v", v, ok)
	}
}
