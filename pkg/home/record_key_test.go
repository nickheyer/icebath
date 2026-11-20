package home

import (
	"reflect"
	"testing"
)

func TestAdoptRecordKeyRenamesTheOlderSpellingInPlace(t *testing.T) {
	retired := RetiredRecordKey(RecordKey)
	keys, vals := []string{"pid", retired, "kind"}, map[string]any{"pid": 7, retired: "/h", "kind": "gateway"}
	AdoptRecordKey(keys, vals)
	if !reflect.DeepEqual(keys, []string{"pid", RecordKey, "kind"}) || !reflect.DeepEqual(vals, map[string]any{"pid": 7, RecordKey: "/h", "kind": "gateway"}) {
		t.Fatalf("adopted %v %v", keys, vals)
	}
	AdoptRecordKey(keys, vals)
	if len(keys) != 3 || vals[RecordKey] != "/h" {
		t.Fatalf("a second adoption changed the record: %v %v", keys, vals)
	}
	both := map[string]any{RecordKey: "/new", retired: "/old"}
	AdoptRecordKey([]string{RecordKey, retired}, both)
	if both[RecordKey] != "/new" || both[retired] != "/old" {
		t.Fatalf("a record carrying both spellings was rewritten: %v", both)
	}
	if v, ok := RecordValue(func(k string) (any, bool) { v, ok := map[string]any{retired: "/old"}[k]; return v, ok }, RecordKey); !ok || v != "/old" {
		t.Fatalf("the older spelling is no longer read: %v %v", v, ok)
	}
}
